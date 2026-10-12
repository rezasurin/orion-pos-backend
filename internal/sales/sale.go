package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
)

// saleCompleted projects sale.completed. The event id is the sale's id. The payload (see
// validateSale) carries the pricing settings the device used, every line with its modifiers and
// discounts, the totals the device computed, and the payments:
//
//	{"shift_id": "...", "receipt_number": "JKT1-03-000482", "catalog_seq": 1234,
//	 "pricing": {"version": 1, "price_includes_tax": false, "tax_rate_bp": 1100, "service_charge_rate_bp": 500,
//	             "service_charge_taxable": true, "cash_rounding_unit": 100, "cash_rounding_mode": "nearest"},
//	 "lines": [{"variant_id": "...", "name": "Latte (Hot)", "unit_price": 28000, "quantity": 2,
//	            "modifiers": [{"modifier_id": "...", "name": "Oat milk", "price_delta": 4000}],
//	            "discount": 0, "allocated_bill_discount": 0, "total": 64000}],
//	 "discounts": [{"line": 0, "kind": "percent", "value": 1000, "amount": 6400, "reason": "staff", "approved_by": "..."}],
//	 "totals": {"subtotal": 64000, "discount_total": 0, "service_charge": 3200, "tax": 7392, "rounding_amount": 8, "total": 74592},
//	 "payments": [{"method": "cash", "amount": 74600, "tendered": 100000, "change": 25400}]}
func (p *Projector) saleCompleted(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	s, b := validateSale(ev.Payload)
	if b != nil {
		return sync.Outcome{}, b
	}
	outlet, err := p.ten.OutletInTx(ctx, tx, env.TenantID, env.OutletID)
	if err != nil {
		return sync.Outcome{}, err
	}
	if s.Receipt.OutletCode != outlet.Code || s.Receipt.DeviceCode != int32(env.DeviceCode) { //nolint:gosec // device codes are small
		return sync.Outcome{}, &bad{code: "invalid_receipt_number", detail: "the receipt number does not belong to this outlet and device"}
	}

	q := db.New(tx)
	shift, err := q.GetShift(ctx, db.GetShiftParams{TenantID: env.TenantID, ID: s.ShiftID})
	if err != nil {
		return notFoundIsPending(err, s.ShiftID)
	}
	if shift.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the shift belongs to a different outlet"}
	}

	// Everything the sale needs to be judged, before anything is written.
	approvers := map[uuid.UUID]identity.Acting{}
	for _, d := range s.Discounts {
		if d.ApprovedBy == nil {
			continue
		}
		if _, done := approvers[*d.ApprovedBy]; done {
			continue
		}
		a, err := p.ids.LoadActing(ctx, tx, env.TenantID, *d.ApprovedBy, env.OutletID)
		if err != nil {
			return sync.Outcome{}, err
		}
		if !a.Known {
			return sync.Outcome{}, &bad{code: "unknown_staff", detail: "a discount was approved by someone who is not part of this business"}
		}
		approvers[*d.ApprovedBy] = a
	}
	flags, err := p.judgeSale(ctx, tx, env, ev, s, shift, approvers)
	if err != nil {
		return sync.Outcome{}, err
	}

	if err := p.insertSale(ctx, q, env, ev, s, outlet.Settings.BusinessDate(ev.DeviceTime)); err != nil {
		if constraintOf(err) == "sale_receipt_unique" {
			return sync.Outcome{}, &bad{code: "duplicate_receipt_number", detail: "another sale already has receipt number " + s.ReceiptNumber}
		}
		return sync.Outcome{}, err
	}
	if err := raiseFlags(ctx, q, env, ev, "sale", ev.ID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(ev.ID), nil
}

// judgeSale decides what to flag about a sale that is going to be recorded as it is.
func (p *Projector) judgeSale(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event, s *sale, shift db.Shift, approvers map[uuid.UUID]identity.Acting) ([]flagSpec, error) {
	var flags []flagSpec

	// Who did it, and whether they were allowed.
	if !env.Staff.Has(identity.PermSaleCreate) {
		flags = append(flags, permissionFlag(env, identity.PermSaleCreate))
	}
	for i, d := range s.Discounts {
		who := env.Staff
		if d.ApprovedBy != nil {
			who = approvers[*d.ApprovedBy]
		}
		if !who.Has(identity.PermDiscountApplyManual) {
			f := permissionFlag(env, identity.PermDiscountApplyManual)
			f.Detail["discount"], f.Detail["staff_id"] = i, who.StaffID
			flags = append(flags, f)
		}
	}

	// Where and when.
	if shift.DeviceID != env.DeviceID {
		flags = append(flags, flagSpec{Code: FlagShiftOtherDevice, Detail: map[string]any{"shift_id": shift.ID, "opened_on": shift.DeviceID}})
	}
	if ev.DeviceTime.Before(shift.OpenedAt) || (shift.ClosedAt != nil && ev.DeviceTime.After(*shift.ClosedAt)) {
		flags = append(flags, flagSpec{Code: FlagSaleOutsideShift, Detail: map[string]any{
			"shift_id": shift.ID, "opened_at": shift.OpenedAt, "closed_at": shift.ClosedAt,
		}})
	}
	flags = append(flags, clockFlags(env, ev)...)

	// What it was priced with.
	variantIDs := make([]uuid.UUID, 0, len(s.Lines))
	for _, l := range s.Lines {
		variantIDs = append(variantIDs, l.VariantID)
	}
	checks, err := p.cat.CheckPrices(ctx, tx, env.TenantID, env.OutletID, variantIDs, s.CatalogSeq, ev.DeviceTime)
	if err != nil {
		return nil, err
	}
	var stale []map[string]any
	for i, l := range s.Lines {
		c, ok := checks[l.VariantID]
		if !ok {
			return nil, &bad{code: "unknown_reference", detail: fmt.Sprintf("lines[%d].variant_id is not in this business's catalog", i)}
		}
		if c.MissedUpdate && int64(c.Current) != l.UnitPrice {
			stale = append(stale, map[string]any{"line": i, "variant_id": l.VariantID, "sold_at": l.UnitPrice, "current": int64(c.Current)})
		}
	}
	if len(stale) > 0 {
		flags = append(flags, flagSpec{Code: FlagStalePrice, Detail: map[string]any{"lines": stale, "catalog_seq": s.CatalogSeq}})
	}

	// Whether the numbers are the numbers.
	flags = append(flags, recompute(s)...)
	flags = append(flags, checkPayments(s)...)
	return flags, nil
}

// tenderOf says how the sale was paid, which decides whether cash rounding applies.
func tenderOf(payments []validPayment) pricing.Tender {
	cash, other := 0, 0
	for _, p := range payments {
		if p.Method == "cash" {
			cash++
		} else {
			other++
		}
	}
	switch {
	case cash > 0 && other == 0:
		return pricing.TenderCash
	case cash > 0:
		return pricing.TenderMixed
	}
	return pricing.TenderNonCash
}

type mismatch struct {
	Field  string `json:"field"`
	Device int64  `json:"device"`
	Server int64  `json:"server"`
}

// recompute prices the sale with the settings the device says it used and compares every amount
// with what the device recorded.
func recompute(s *sale) []flagSpec {
	if s.Version != pricing.Version {
		return []flagSpec{{Code: FlagPricingVersion, Detail: map[string]any{"device": s.Version, "server": pricing.Version}}}
	}
	bill := pricing.Bill{Settings: s.Settings, Tender: tenderOf(s.Payments)}
	for _, l := range s.Lines {
		line := pricing.Line{UnitPrice: kernel.Rupiah(l.UnitPrice), Quantity: l.Quantity}
		for _, m := range l.Modifiers {
			line.ModifierDeltas = append(line.ModifierDeltas, kernel.Rupiah(m.PriceDelta))
		}
		bill.Lines = append(bill.Lines, line)
	}
	for _, d := range s.Discounts {
		bill.Discounts = append(bill.Discounts, pricing.Discount{Line: d.Line, Kind: d.Kind, Value: d.Value})
	}
	res, err := pricing.Calculate(bill)
	if err != nil {
		detail := map[string]any{"error": err.Error()}
		var perr *pricing.Error
		if errors.As(err, &perr) {
			detail["code"] = perr.Code
		}
		return []flagSpec{{Code: FlagPricingInvalid, Detail: detail}}
	}

	var diffs []mismatch
	check := func(field string, device int64, server kernel.Rupiah) {
		if device != int64(server) {
			diffs = append(diffs, mismatch{Field: field, Device: device, Server: int64(server)})
		}
	}
	for i, l := range s.Lines {
		check(fmt.Sprintf("lines[%d].discount", i), l.Discount, res.Lines[i].Discount)
		check(fmt.Sprintf("lines[%d].allocated_bill_discount", i), l.AllocatedBillDiscount, res.Lines[i].AllocatedBillDiscount)
		check(fmt.Sprintf("lines[%d].total", i), l.Total, res.Lines[i].Total)
	}
	for i, d := range s.Discounts {
		check(fmt.Sprintf("discounts[%d].amount", i), d.Amount, res.Discounts[i].Amount)
	}
	check("totals.subtotal", s.Subtotal, res.Subtotal)
	check("totals.discount_total", s.DiscountTotal, res.DiscountTotal)
	check("totals.service_charge", s.ServiceCharge, res.ServiceCharge)
	check("totals.tax", s.Tax, res.Tax)
	check("totals.total", s.Total, res.Total)
	check("totals.rounding_amount", s.Rounding, res.RoundingAmount)
	if len(diffs) == 0 {
		return nil
	}
	detail := map[string]any{"count": len(diffs)}
	if len(diffs) > maxMismatchesListed {
		diffs = diffs[:maxMismatchesListed]
	}
	detail["mismatches"] = diffs
	return []flagSpec{{Code: FlagTotalMismatch, Detail: detail}}
}

// checkPayments flags payments that do not add up to what was due.
func checkPayments(s *sale) []flagSpec {
	due := s.Total + s.Rounding
	var paid int64
	var flags []flagSpec
	for i, p := range s.Payments {
		paid += p.Amount
		if p.Method == "cash" && p.Tendered != nil && p.Change != nil && *p.Tendered-*p.Change != p.Amount {
			flags = append(flags, flagSpec{Code: FlagPaymentMismatch, Detail: map[string]any{
				"payment": i, "amount": p.Amount, "tendered": *p.Tendered, "change": *p.Change,
			}})
		}
	}
	if paid != due {
		flags = append(flags, flagSpec{Code: FlagPaymentMismatch, Detail: map[string]any{"due": due, "paid": paid}})
	}
	return flags
}

// insertSale writes the sale as the device recorded it: the sale row, its lines, their modifiers,
// the discounts and the payments, each group in one round trip.
func (p *Projector) insertSale(ctx context.Context, q *db.Queries, env sync.Env, ev sync.Event, s *sale, businessDate time.Time) error {
	settings, err := json.Marshal(map[string]any{
		"version": s.Version, "price_includes_tax": s.Settings.PriceIncludesTax, "tax_rate_bp": int64(s.Settings.TaxRate),
		"service_charge_rate_bp": int64(s.Settings.ServiceChargeRate), "service_charge_taxable": s.Settings.ServiceChargeTaxable,
		"cash_rounding_unit": int64(s.Settings.CashRoundingUnit), "cash_rounding_mode": string(s.Settings.CashRoundingMode),
	})
	if err != nil {
		return err
	}
	if err := q.InsertSale(ctx, db.InsertSaleParams{
		ID: ev.ID, TenantID: env.TenantID, OutletID: env.OutletID, DeviceID: env.DeviceID, ShiftID: s.ShiftID, StaffID: ev.StaffID,
		ReceiptNumber: s.ReceiptNumber, ReceiptDeviceCode: s.Receipt.DeviceCode, ReceiptCounter: s.Receipt.Counter,
		DeviceTime: ev.DeviceTime, ReceivedAt: env.ReceivedAt, BusinessDate: pgDate(businessDate),
		PricingVersion: int32(s.Version), //nolint:gosec // validated to 1..1000
		Pricing:        settings, CatalogSeq: s.CatalogSeq,
		Subtotal: s.Subtotal, DiscountTotal: s.DiscountTotal, ServiceCharge: s.ServiceCharge, Tax: s.Tax,
		TaxIncluded: s.Settings.PriceIncludesTax, RoundingAmount: s.Rounding, Total: s.Total,
	}); err != nil {
		return err
	}

	lineIDs := make([]uuid.UUID, len(s.Lines))
	lines := make([]db.InsertSaleLineParams, len(s.Lines))
	var mods []db.InsertSaleLineModifierParams
	for i, l := range s.Lines {
		lineIDs[i] = kernel.NewID()
		lines[i] = db.InsertSaleLineParams{
			ID: lineIDs[i], TenantID: env.TenantID, SaleID: ev.ID, LineNo: int32(i), VariantID: l.VariantID, NameSnapshot: l.Name, //nolint:gosec // at most 200 lines
			UnitPrice: l.UnitPrice, Quantity: int32(l.Quantity), LineDiscount: l.Discount, //nolint:gosec // validated to at most 10000
			AllocatedBillDiscount: l.AllocatedBillDiscount, LineTotal: l.Total,
		}
		for j, m := range l.Modifiers {
			mods = append(mods, db.InsertSaleLineModifierParams{
				TenantID: env.TenantID, SaleLineID: lineIDs[i], Position: int32(j), ModifierID: m.ID, NameSnapshot: m.Name, PriceDelta: m.PriceDelta, //nolint:gosec // at most 50
			})
		}
	}
	if err := runBatch(q.InsertSaleLine(ctx, lines).Exec); err != nil {
		return err
	}
	if len(mods) > 0 {
		if err := runBatch(q.InsertSaleLineModifier(ctx, mods).Exec); err != nil {
			return err
		}
	}

	if len(s.Discounts) > 0 {
		ds := make([]db.InsertSaleDiscountParams, len(s.Discounts))
		for i, d := range s.Discounts {
			ds[i] = db.InsertSaleDiscountParams{
				ID: kernel.NewID(), TenantID: env.TenantID, SaleID: ev.ID, Kind: string(d.Kind), Value: d.Value, Amount: d.Amount,
				Reason: d.Reason, ApprovedBy: d.ApprovedBy,
			}
			if d.Line != nil {
				ds[i].SaleLineID = &lineIDs[*d.Line]
			}
		}
		if err := runBatch(q.InsertSaleDiscount(ctx, ds).Exec); err != nil {
			return err
		}
	}
	if len(s.Payments) > 0 {
		ps := make([]db.InsertPaymentParams, len(s.Payments))
		for i, pay := range s.Payments {
			ps[i] = db.InsertPaymentParams{
				ID: kernel.NewID(), TenantID: env.TenantID, SaleID: ev.ID, Method: pay.Method, Amount: pay.Amount,
				Tendered: pay.Tendered, Change: pay.Change, Reference: pay.Reference,
			}
		}
		if err := runBatch(q.InsertPayment(ctx, ps).Exec); err != nil {
			return err
		}
	}
	return nil
}
