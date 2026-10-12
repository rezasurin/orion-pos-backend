package sales

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
)

// saleVoided projects sale.voided:
//
//	{"sale_id": "...", "reason": "customer changed their mind", "approved_by": "...", "shift_id": "..."}
//
// The event has its own id. A void that arrives before its sale waits for it. The void needs
// sale.void from the person who did it or, if named, the person who approved it; without it the
// void is still recorded (the refund has happened) and flagged. shift_id, when given, is the shift
// the void happened in, which is where the refund left the drawer; it defaults to the sale's own.
func (p *Projector) saleVoided(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var in saleVoided
	if b := decode(ev.Payload, &in); b != nil {
		return sync.Outcome{}, b
	}
	if in.SaleID == uuid.Nil {
		return sync.Outcome{}, invalid("sale_id is required")
	}
	reason, b := text("reason", in.Reason, true, maxText)
	if b != nil {
		return sync.Outcome{}, b
	}

	q := db.New(tx)
	target, err := q.GetSaleForVoid(ctx, db.GetSaleForVoidParams{TenantID: env.TenantID, ID: in.SaleID})
	if err != nil {
		return notFoundIsPending(err, in.SaleID)
	}
	if target.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the sale belongs to a different outlet"}
	}
	if target.Status == "voided" {
		return sync.Outcome{}, &bad{code: "already_voided", detail: "the sale was already voided"}
	}
	// A void gives the whole sale back; after a refund that would pay part of it twice.
	refunded, err := q.SaleRefundedTotal(ctx, db.SaleRefundedTotalParams{TenantID: env.TenantID, SaleID: in.SaleID})
	if err != nil {
		return sync.Outcome{}, err
	}
	if refunded > 0 {
		return sync.Outcome{}, &bad{code: "already_refunded", detail: "the sale has a refund; refund the rest instead of voiding it"}
	}

	shiftID := target.ShiftID
	if in.ShiftID != nil {
		shiftID = *in.ShiftID
	}
	shift, err := q.GetShift(ctx, db.GetShiftParams{TenantID: env.TenantID, ID: shiftID})
	if err != nil {
		return notFoundIsPending(err, shiftID)
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
			return sync.Outcome{}, &bad{code: "unknown_staff", detail: "the void was approved by someone who is not part of this business"}
		}
	}

	outlet, err := p.ten.OutletInTx(ctx, tx, env.TenantID, env.OutletID)
	if err != nil {
		return sync.Outcome{}, err
	}
	if err := q.MarkSaleVoided(ctx, db.MarkSaleVoidedParams{TenantID: env.TenantID, ID: in.SaleID}); err != nil {
		return sync.Outcome{}, err
	}
	if err := q.InsertVoid(ctx, db.InsertVoidParams{
		ID: ev.ID, TenantID: env.TenantID, OutletID: env.OutletID, SaleID: in.SaleID, ShiftID: shiftID, StaffID: ev.StaffID,
		ApprovedBy: in.ApprovedBy, Reason: reason, DeviceTime: ev.DeviceTime, ReceivedAt: env.ReceivedAt,
		BusinessDate: pgDate(outlet.Settings.BusinessDate(ev.DeviceTime)),
	}); err != nil {
		return sync.Outcome{}, err
	}

	var flags []flagSpec
	if !env.Staff.Has(identity.PermSaleVoid) && !approver.Has(identity.PermSaleVoid) {
		f := permissionFlag(env, identity.PermSaleVoid)
		f.Detail["sale_id"] = in.SaleID
		if in.ApprovedBy != nil {
			f.Detail["approved_by"] = *in.ApprovedBy
		}
		flags = append(flags, f)
	}
	flags = append(flags, clockFlags(env, ev)...)
	if err := raiseFlags(ctx, q, env, ev, "void", ev.ID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(ev.ID), nil
}
