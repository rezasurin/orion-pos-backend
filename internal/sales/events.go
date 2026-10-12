package sales

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
)

// Event types this module projects, with the schema version each understands.
const (
	TypeShiftOpened   = "shift.opened"
	TypeShiftClosed   = "shift.closed"
	TypeCashMovement  = "cash.movement"
	TypeSaleCompleted = "sale.completed"
	TypeSaleVoided    = "sale.voided"
	TypeRefundIssued  = "refund.issued"
)

// bad is a payload the module cannot make sense of: the event is rejected with its code.
type bad struct{ code, detail string }

func (b *bad) Error() string { return b.code + ": " + b.detail }

func invalid(format string, args ...any) *bad {
	return &bad{code: "invalid_payload", detail: fmt.Sprintf(format, args...)}
}

// Largest amounts a device may send, in rupiah. The database checks the same bounds.
const (
	maxAmount     = 1_000_000_000_000
	maxUnitPrice  = 1_000_000_000
	maxText       = 200
	maxLines      = pricing.MaxLines
	maxModifiers  = pricing.MaxModifiersPerLine
	maxDiscounts  = pricing.MaxLines + 1
	maxPayments   = 10
	maxNameLength = 200
)

// decode reads a payload into v. Unknown fields are ignored, so a newer device may add fields within
// a schema version.
func decode(raw json.RawMessage, v any) *bad {
	if err := json.Unmarshal(raw, v); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// need checks a required whole number.
func need(field string, p *int64, lo, hi int64) (int64, *bad) {
	if p == nil {
		return 0, invalid("%s is required", field)
	}
	if *p < lo || *p > hi {
		return 0, invalid("%s must be between %d and %d", field, lo, hi)
	}
	return *p, nil
}

// optional checks an optional whole number.
func optional(field string, p *int64, lo, hi int64) (*int64, *bad) {
	if p == nil {
		return nil, nil
	}
	if *p < lo || *p > hi {
		return nil, invalid("%s must be between %d and %d", field, lo, hi)
	}
	return p, nil
}

func text(field, v string, required bool, max int) (string, *bad) {
	v = strings.TrimSpace(v)
	if required && v == "" {
		return "", invalid("%s is required", field)
	}
	if len([]rune(v)) > max {
		return "", invalid("%s is at most %d characters", field, max)
	}
	return v, nil
}

// ---- shifts and cash ----

type shiftOpened struct {
	OpeningCash *int64 `json:"opening_cash"`
}

type shiftClosed struct {
	ShiftID     uuid.UUID `json:"shift_id"`
	CountedCash *int64    `json:"counted_cash"`
}

type cashMovement struct {
	ShiftID uuid.UUID `json:"shift_id"`
	Kind    string    `json:"kind"`
	Amount  *int64    `json:"amount"`
	Reason  string    `json:"reason"`
}

type saleVoided struct {
	SaleID     uuid.UUID  `json:"sale_id"`
	ShiftID    *uuid.UUID `json:"shift_id"`
	Reason     string     `json:"reason"`
	ApprovedBy *uuid.UUID `json:"approved_by"`
}

// ---- sales ----

type saleCompleted struct {
	ShiftID       uuid.UUID `json:"shift_id"`
	ReceiptNumber string    `json:"receipt_number"`
	CatalogSeq    *int64    `json:"catalog_seq"`
	Pricing       *struct {
		Version              *int64 `json:"version"`
		PriceIncludesTax     bool   `json:"price_includes_tax"`
		TaxRateBP            *int64 `json:"tax_rate_bp"`
		ServiceChargeRateBP  *int64 `json:"service_charge_rate_bp"`
		ServiceChargeTaxable bool   `json:"service_charge_taxable"`
		CashRoundingUnit     *int64 `json:"cash_rounding_unit"`
		CashRoundingMode     string `json:"cash_rounding_mode"`
	} `json:"pricing"`
	Lines     []saleLine     `json:"lines"`
	Discounts []saleDiscount `json:"discounts"`
	Totals    *struct {
		Subtotal       *int64 `json:"subtotal"`
		DiscountTotal  *int64 `json:"discount_total"`
		ServiceCharge  *int64 `json:"service_charge"`
		Tax            *int64 `json:"tax"`
		RoundingAmount *int64 `json:"rounding_amount"`
		Total          *int64 `json:"total"`
	} `json:"totals"`
	Payments []salePayment `json:"payments"`
}

type saleLine struct {
	VariantID             uuid.UUID      `json:"variant_id"`
	Name                  string         `json:"name"`
	UnitPrice             *int64         `json:"unit_price"`
	Quantity              *int64         `json:"quantity"`
	Modifiers             []saleModifier `json:"modifiers"`
	Discount              *int64         `json:"discount"`
	AllocatedBillDiscount *int64         `json:"allocated_bill_discount"`
	Total                 *int64         `json:"total"`
}

type saleModifier struct {
	ModifierID uuid.UUID `json:"modifier_id"`
	Name       string    `json:"name"`
	PriceDelta *int64    `json:"price_delta"`
}

type saleDiscount struct {
	Line       *int       `json:"line"`
	Kind       string     `json:"kind"`
	Value      *int64     `json:"value"`
	Amount     *int64     `json:"amount"`
	Reason     string     `json:"reason"`
	ApprovedBy *uuid.UUID `json:"approved_by"`
}

type salePayment struct {
	Method    string `json:"method"`
	Amount    *int64 `json:"amount"`
	Tendered  *int64 `json:"tendered"`
	Change    *int64 `json:"change"`
	Reference string `json:"reference"`
}

// sale is a validated sale.completed payload: every field present and in range.
type sale struct {
	ShiftID       uuid.UUID
	ReceiptNumber string
	Receipt       receipt
	CatalogSeq    int64
	Settings      pricing.Settings
	Version       int64
	Lines         []validLine
	Discounts     []validDiscount
	Subtotal      int64
	DiscountTotal int64
	ServiceCharge int64
	Tax           int64
	Rounding      int64
	Total         int64
	Payments      []validPayment
}

type validLine struct {
	VariantID             uuid.UUID
	Name                  string
	UnitPrice             int64
	Quantity              int64
	Modifiers             []validModifier
	Discount              int64
	AllocatedBillDiscount int64
	Total                 int64
}

type validModifier struct {
	ID         uuid.UUID
	Name       string
	PriceDelta int64
}

type validDiscount struct {
	Line       *int
	Kind       pricing.DiscountKind
	Value      int64
	Amount     int64
	Reason     string
	ApprovedBy *uuid.UUID
}

type validPayment struct {
	Method    string
	Amount    int64
	Tendered  *int64
	Change    *int64
	Reference string
}

var paymentMethods = map[string]bool{"cash": true, "qris_manual": true, "qris_dynamic": true, "ewallet": true, "card_manual": true}

// receipt is a parsed receipt number: {outlet code}-{device code}-{counter}.
type receipt struct {
	OutletCode string
	DeviceCode int32
	Counter    int64
}

var receiptPattern = regexp.MustCompile(`^([A-Z0-9]{2,6})-([0-9]{2,6})-([0-9]{6,12})$`)

func parseReceipt(s string) (receipt, *bad) {
	m := receiptPattern.FindStringSubmatch(s)
	if m == nil {
		return receipt{}, &bad{code: "invalid_receipt_number", detail: "a receipt number looks like JKT1-03-000482"}
	}
	dev, _ := strconv.ParseInt(m[2], 10, 32)
	ctr, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil || dev < 1 || ctr < 1 {
		return receipt{}, &bad{code: "invalid_receipt_number", detail: "the device code and counter must be at least 1"}
	}
	return receipt{OutletCode: m[1], DeviceCode: int32(dev), Counter: ctr}, nil //nolint:gosec // at most 6 digits
}

// validateSale checks a sale.completed payload for what makes it unusable (a missing field, a
// number out of range, a bad receipt number) and returns it with every amount in plain form.
// Whether the numbers add up is not decided here: a sale that does not recompute is accepted and
// flagged.
func validateSale(raw json.RawMessage) (*sale, *bad) {
	var p saleCompleted
	if b := decode(raw, &p); b != nil {
		return nil, b
	}
	out := &sale{ShiftID: p.ShiftID, ReceiptNumber: p.ReceiptNumber}
	if p.ShiftID == uuid.Nil {
		return nil, invalid("shift_id is required")
	}
	var b *bad
	if out.Receipt, b = parseReceipt(p.ReceiptNumber); b != nil {
		return nil, b
	}
	if out.CatalogSeq, b = need("catalog_seq", p.CatalogSeq, 0, 1<<53); b != nil {
		return nil, b
	}

	if p.Pricing == nil {
		return nil, invalid("pricing is required")
	}
	pr := p.Pricing
	if out.Version, b = need("pricing.version", pr.Version, 1, 1000); b != nil {
		return nil, b
	}
	tax, b := need("pricing.tax_rate_bp", pr.TaxRateBP, 0, 10_000)
	if b != nil {
		return nil, b
	}
	svc, b := need("pricing.service_charge_rate_bp", pr.ServiceChargeRateBP, 0, 10_000)
	if b != nil {
		return nil, b
	}
	unit, b := need("pricing.cash_rounding_unit", pr.CashRoundingUnit, 0, 1_000_000)
	if b != nil {
		return nil, b
	}
	switch pr.CashRoundingMode {
	case "nearest", "down", "up":
	default:
		return nil, invalid("pricing.cash_rounding_mode must be nearest, down or up")
	}
	out.Settings = pricing.Settings{
		PriceIncludesTax: pr.PriceIncludesTax, TaxRate: kernel.BasisPoints(tax), ServiceChargeRate: kernel.BasisPoints(svc),
		ServiceChargeTaxable: pr.ServiceChargeTaxable, CashRoundingUnit: kernel.Rupiah(unit), CashRoundingMode: pricing.RoundMode(pr.CashRoundingMode),
	}

	if len(p.Lines) == 0 || len(p.Lines) > maxLines {
		return nil, invalid("a sale has between 1 and %d lines", maxLines)
	}
	for i, l := range p.Lines {
		vl, b := validateLine(i, l)
		if b != nil {
			return nil, b
		}
		out.Lines = append(out.Lines, vl)
	}

	if len(p.Discounts) > maxDiscounts {
		return nil, invalid("too many discounts")
	}
	for i, d := range p.Discounts {
		vd, b := validateDiscount(i, d, len(p.Lines))
		if b != nil {
			return nil, b
		}
		out.Discounts = append(out.Discounts, vd)
	}

	if p.Totals == nil {
		return nil, invalid("totals is required")
	}
	t := p.Totals
	for _, f := range []struct {
		name string
		src  *int64
		dst  *int64
		lo   int64
	}{
		{"totals.subtotal", t.Subtotal, &out.Subtotal, 0}, {"totals.discount_total", t.DiscountTotal, &out.DiscountTotal, 0},
		{"totals.service_charge", t.ServiceCharge, &out.ServiceCharge, 0}, {"totals.tax", t.Tax, &out.Tax, 0},
		{"totals.rounding_amount", t.RoundingAmount, &out.Rounding, -1_000_000}, {"totals.total", t.Total, &out.Total, 0},
	} {
		v, b := need(f.name, f.src, f.lo, maxAmount)
		if b != nil {
			return nil, b
		}
		*f.dst = v
	}

	if len(p.Payments) > maxPayments {
		return nil, invalid("a sale has at most %d payments", maxPayments)
	}
	for i, pay := range p.Payments {
		vp, b := validatePayment(i, pay)
		if b != nil {
			return nil, b
		}
		out.Payments = append(out.Payments, vp)
	}
	if len(out.Payments) == 0 && out.Total+out.Rounding != 0 {
		return nil, invalid("a sale that is not free needs at least one payment")
	}
	return out, nil
}

func validateLine(i int, l saleLine) (validLine, *bad) {
	f := func(name string) string { return fmt.Sprintf("lines[%d].%s", i, name) }
	var out validLine
	var b *bad
	if l.VariantID == uuid.Nil {
		return out, invalid("%s is required", f("variant_id"))
	}
	out.VariantID = l.VariantID
	if out.Name, b = text(f("name"), l.Name, true, maxNameLength); b != nil {
		return out, b
	}
	if out.UnitPrice, b = need(f("unit_price"), l.UnitPrice, 0, maxUnitPrice); b != nil {
		return out, b
	}
	if out.Quantity, b = need(f("quantity"), l.Quantity, 1, pricing.MaxQuantity); b != nil {
		return out, b
	}
	if out.Discount, b = need(f("discount"), l.Discount, 0, maxAmount); b != nil {
		return out, b
	}
	if out.AllocatedBillDiscount, b = need(f("allocated_bill_discount"), l.AllocatedBillDiscount, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Total, b = need(f("total"), l.Total, 0, maxAmount); b != nil {
		return out, b
	}
	if len(l.Modifiers) > maxModifiers {
		return out, invalid("%s has too many modifiers", f("modifiers"))
	}
	for j, m := range l.Modifiers {
		name := fmt.Sprintf("lines[%d].modifiers[%d]", i, j)
		if m.ModifierID == uuid.Nil {
			return out, invalid("%s.modifier_id is required", name)
		}
		vm := validModifier{ID: m.ModifierID}
		if vm.Name, b = text(name+".name", m.Name, true, maxNameLength); b != nil {
			return out, b
		}
		if vm.PriceDelta, b = need(name+".price_delta", m.PriceDelta, -maxUnitPrice, maxUnitPrice); b != nil {
			return out, b
		}
		out.Modifiers = append(out.Modifiers, vm)
	}
	return out, nil
}

func validateDiscount(i int, d saleDiscount, lines int) (validDiscount, *bad) {
	name := fmt.Sprintf("discounts[%d]", i)
	out := validDiscount{Line: d.Line, ApprovedBy: d.ApprovedBy}
	var b *bad
	if d.Line != nil && (*d.Line < 0 || *d.Line >= lines) {
		return out, invalid("%s.line refers to a line that does not exist", name)
	}
	switch d.Kind {
	case "percent":
		out.Kind = pricing.DiscountPercent
	case "amount":
		out.Kind = pricing.DiscountAmount
	default:
		return out, invalid("%s.kind must be percent or amount", name)
	}
	if out.Value, b = need(name+".value", d.Value, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Amount, b = need(name+".amount", d.Amount, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Reason, b = text(name+".reason", d.Reason, false, maxText); b != nil {
		return out, b
	}
	return out, nil
}

func validatePayment(i int, p salePayment) (validPayment, *bad) {
	name := fmt.Sprintf("payments[%d]", i)
	out := validPayment{Method: p.Method}
	var b *bad
	if !paymentMethods[p.Method] {
		return out, invalid("%s.method must be one of cash, qris_manual, qris_dynamic, ewallet, card_manual", name)
	}
	if out.Amount, b = need(name+".amount", p.Amount, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Tendered, b = optional(name+".tendered", p.Tendered, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Change, b = optional(name+".change", p.Change, 0, maxAmount); b != nil {
		return out, b
	}
	if out.Reference, b = text(name+".reference", p.Reference, false, 100); b != nil {
		return out, b
	}
	return out, nil
}
