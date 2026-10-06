package sales

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
)

type refundIssued struct {
	SaleID     uuid.UUID  `json:"sale_id"`
	ShiftID    uuid.UUID  `json:"shift_id"`
	Method     string     `json:"method"`
	Reason     string     `json:"reason"`
	ApprovedBy *uuid.UUID `json:"approved_by"`
	Lines      []struct {
		LineNo   *int64 `json:"line_no"`
		Quantity *int64 `json:"quantity"`
		Amount   *int64 `json:"amount"`
	} `json:"lines"`
}

// refundIssued projects refund.issued:
//
//	{"sale_id": "...", "shift_id": "...", "method": "cash", "reason": "cold coffee", "approved_by": "...",
//	 "lines": [{"line_no": 0, "quantity": 1, "amount": 22000}]}
//
// The event has its own id. Money went back to the customer for some of a completed sale: the sale
// stays as rung up, and the refund counts on the business day it happened, from the drawer of its
// shift when in cash. The device decides each line's amount (its share of what was paid, tax and
// service included); the server records it as sent and flags a refund that gives back more of a
// line than was sold, more money than the sale took, or that was done without sale.refund. A refund
// waits for its sale and shift; a voided sale cannot be refunded (already_voided).
func (p *Projector) refundIssued(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var in refundIssued
	if b := decode(ev.Payload, &in); b != nil {
		return sync.Outcome{}, b
	}
	if in.SaleID == uuid.Nil || in.ShiftID == uuid.Nil {
		return sync.Outcome{}, invalid("sale_id and shift_id are required")
	}
	if !paymentMethods[in.Method] {
		return sync.Outcome{}, invalid("method must be one of cash, qris_manual, qris_dynamic, ewallet, card_manual")
	}
	reason, b := text("reason", in.Reason, true, maxText)
	if b != nil {
		return sync.Outcome{}, b
	}
	if len(in.Lines) == 0 || len(in.Lines) > maxLines {
		return sync.Outcome{}, invalid("lines must have between 1 and %d entries", maxLines)
	}

	q := db.New(tx)
	sale, err := q.GetSaleForRefund(ctx, db.GetSaleForRefundParams{TenantID: env.TenantID, ID: in.SaleID})
	if err != nil {
		return notFoundIsPending(err, in.SaleID)
	}
	if sale.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the sale belongs to a different outlet"}
	}
	if sale.Status == "voided" {
		return sync.Outcome{}, &bad{code: "already_voided", detail: "the sale was voided; a voided sale cannot be refunded"}
	}
	shift, err := q.GetShift(ctx, db.GetShiftParams{TenantID: env.TenantID, ID: in.ShiftID})
	if err != nil {
		return notFoundIsPending(err, in.ShiftID)
	}
	if shift.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the shift belongs to a different outlet"}
	}
	approver := env.Staff
	if in.ApprovedBy != nil {
		if approver, err = p.ids.LoadActing(ctx, tx, env.TenantID, *in.ApprovedBy, env.OutletID); err != nil {
			return sync.Outcome{}, err
		}
		if !approver.Known {
			return sync.Outcome{}, &bad{code: "unknown_staff", detail: "the refund was approved by someone who is not part of this business"}
		}
	}

	saleLines, err := q.SaleLinesForRefund(ctx, db.SaleLinesForRefundParams{TenantID: env.TenantID, SaleID: in.SaleID})
	if err != nil {
		return sync.Outcome{}, err
	}
	byNo := make(map[int64]db.SaleLinesForRefundRow, len(saleLines))
	for _, l := range saleLines {
		byNo[int64(l.LineNo)] = l
	}
	var flags []flagSpec
	lines := db.InsertRefundLinesParams{TenantID: env.TenantID, RefundID: ev.ID}
	var total int64
	for i, l := range in.Lines {
		name := fmt.Sprintf("lines[%d]", i)
		no, b := need(name+".line_no", l.LineNo, 0, maxLines)
		if b != nil {
			return sync.Outcome{}, b
		}
		qty, b := need(name+".quantity", l.Quantity, 1, 10_000)
		if b != nil {
			return sync.Outcome{}, b
		}
		amount, b := need(name+".amount", l.Amount, 0, maxAmount)
		if b != nil {
			return sync.Outcome{}, b
		}
		sl, ok := byNo[no]
		if !ok {
			return sync.Outcome{}, invalid("%s.line_no %d is not a line of the sale", name, no)
		}
		for _, seen := range lines.SaleLineIds {
			if seen == sl.ID {
				return sync.Outcome{}, invalid("%s.line_no %d is listed twice", name, no)
			}
		}
		if sl.Refunded+qty > int64(sl.Quantity) {
			flags = append(flags, flagSpec{Code: FlagRefundOverQuantity, Detail: map[string]any{
				"sale_id": in.SaleID, "line_no": no, "sold": sl.Quantity, "refunded_before": sl.Refunded, "refunded_now": qty,
			}})
		}
		lines.SaleLineIds = append(lines.SaleLineIds, sl.ID)
		lines.Quantities = append(lines.Quantities, int32(qty))
		lines.Amounts = append(lines.Amounts, amount)
		total += amount
	}
	if total > maxAmount {
		return sync.Outcome{}, invalid("the lines add up to more than %d", int64(maxAmount))
	}
	before, err := q.SaleRefundedTotal(ctx, db.SaleRefundedTotalParams{TenantID: env.TenantID, SaleID: in.SaleID})
	if err != nil {
		return sync.Outcome{}, err
	}
	// Only a refund that gives money back is to blame for going over.
	if paid := sale.Total + sale.RoundingAmount; total > 0 && before+total > paid {
		flags = append(flags, flagSpec{Code: FlagRefundOverPaid, Detail: map[string]any{
			"sale_id": in.SaleID, "paid": paid, "refunded_before": before, "refunded_now": total,
		}})
	}

	outlet, err := p.ten.OutletInTx(ctx, tx, env.TenantID, env.OutletID)
	if err != nil {
		return sync.Outcome{}, err
	}
	if err := q.InsertRefund(ctx, db.InsertRefundParams{
		ID: ev.ID, TenantID: env.TenantID, OutletID: env.OutletID, SaleID: in.SaleID, ShiftID: in.ShiftID, StaffID: ev.StaffID,
		ApprovedBy: in.ApprovedBy, Method: in.Method, Amount: total, Reason: reason, DeviceTime: ev.DeviceTime,
		ReceivedAt: env.ReceivedAt, BusinessDate: pgDate(outlet.Settings.BusinessDate(ev.DeviceTime)),
	}); err != nil {
		return sync.Outcome{}, err
	}
	if err := q.InsertRefundLines(ctx, lines); err != nil {
		return sync.Outcome{}, err
	}

	if !env.Staff.Has(identity.PermSaleRefund) && !approver.Has(identity.PermSaleRefund) {
		f := permissionFlag(env, identity.PermSaleRefund)
		f.Detail["sale_id"] = in.SaleID
		if in.ApprovedBy != nil {
			f.Detail["approved_by"] = *in.ApprovedBy
		}
		flags = append(flags, f)
	}
	flags = append(flags, clockFlags(env, ev)...)
	if err := raiseFlags(ctx, q, env, ev, "refund", ev.ID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(ev.ID), nil
}
