package sales

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
)

// permissionFlag is the flag for an action by someone who lacks permission p.
func permissionFlag(env sync.Env, p identity.Permission) flagSpec {
	return flagSpec{Code: FlagPermissionMissing, Detail: map[string]any{"permission": string(p), "staff_id": env.Staff.StaffID}}
}

// shiftOpened projects shift.opened: {"opening_cash": 50000}. The event id is the shift's id.
func (p *Projector) shiftOpened(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var in shiftOpened
	if b := decode(ev.Payload, &in); b != nil {
		return sync.Outcome{}, b
	}
	cash, b := need("opening_cash", in.OpeningCash, 0, maxAmount)
	if b != nil {
		return sync.Outcome{}, b
	}
	outlet, err := p.ten.OutletInTx(ctx, tx, env.TenantID, env.OutletID)
	if err != nil {
		return sync.Outcome{}, err
	}
	q := db.New(tx)
	if err := q.InsertShift(ctx, db.InsertShiftParams{
		ID: ev.ID, TenantID: env.TenantID, OutletID: env.OutletID, DeviceID: env.DeviceID, OpenedBy: ev.StaffID,
		OpenedAt: ev.DeviceTime, OpeningCash: cash, BusinessDate: pgDate(outlet.Settings.BusinessDate(ev.DeviceTime)), ReceivedAt: env.ReceivedAt,
	}); err != nil {
		return sync.Outcome{}, err
	}
	var flags []flagSpec
	if !env.Staff.Has(identity.PermShiftOpen) {
		flags = append(flags, permissionFlag(env, identity.PermShiftOpen))
	}
	flags = append(flags, clockFlags(env, ev)...)
	if err := raiseFlags(ctx, q, env, ev, "shift", ev.ID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(ev.ID), nil
}

// shiftClosed projects shift.closed: {"shift_id": "...", "counted_cash": 183000}. The event has its
// own id; the shift it closes is named in the payload. A shift closes once.
func (p *Projector) shiftClosed(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var in shiftClosed
	if b := decode(ev.Payload, &in); b != nil {
		return sync.Outcome{}, b
	}
	if in.ShiftID == uuid.Nil {
		return sync.Outcome{}, invalid("shift_id is required")
	}
	counted, b := need("counted_cash", in.CountedCash, 0, maxAmount)
	if b != nil {
		return sync.Outcome{}, b
	}
	q := db.New(tx)
	shift, err := q.GetShiftForUpdate(ctx, db.GetShiftForUpdateParams{TenantID: env.TenantID, ID: in.ShiftID})
	if err != nil {
		return notFoundIsPending(err, in.ShiftID)
	}
	if shift.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the shift belongs to a different outlet"}
	}
	if shift.ClosedAt != nil {
		return sync.Outcome{}, &bad{code: "already_closed", detail: "the shift was already closed"}
	}
	if _, err := q.CloseShift(ctx, db.CloseShiftParams{
		TenantID: env.TenantID, ID: in.ShiftID, ClosedBy: &ev.StaffID, ClosedAt: &ev.DeviceTime, CountedCash: &counted, CloseEventID: &ev.ID,
	}); err != nil {
		return sync.Outcome{}, err
	}
	var flags []flagSpec
	if !env.Staff.Has(identity.PermShiftClose) {
		flags = append(flags, permissionFlag(env, identity.PermShiftClose))
	}
	if shift.DeviceID != env.DeviceID {
		flags = append(flags, flagSpec{Code: FlagShiftOtherDevice, Detail: map[string]any{"shift_id": in.ShiftID, "opened_on": shift.DeviceID}})
	}
	flags = append(flags, clockFlags(env, ev)...)
	if err := raiseFlags(ctx, q, env, ev, "shift", in.ShiftID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(), nil
}

// cashMovement projects cash.movement: pay in, pay out, or a no-sale drawer opening.
//
//	{"shift_id": "...", "kind": "pay_in" | "pay_out" | "no_sale", "amount": 20000, "reason": "change float"}
//
// Opening the drawer for any reason needs drawer.open_no_sale, since there is no separate cash
// permission.
func (p *Projector) cashMovement(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var in cashMovement
	if b := decode(ev.Payload, &in); b != nil {
		return sync.Outcome{}, b
	}
	if in.ShiftID == uuid.Nil {
		return sync.Outcome{}, invalid("shift_id is required")
	}
	switch in.Kind {
	case "pay_in", "pay_out", "no_sale":
	default:
		return sync.Outcome{}, invalid("kind must be pay_in, pay_out or no_sale")
	}
	amount, b := need("amount", in.Amount, 0, maxAmount)
	if b != nil {
		return sync.Outcome{}, b
	}
	if in.Kind == "no_sale" && amount != 0 {
		return sync.Outcome{}, invalid("a no-sale opening has no amount")
	}
	if in.Kind != "no_sale" && amount == 0 {
		return sync.Outcome{}, invalid("amount must be more than 0")
	}
	reason, b := text("reason", in.Reason, in.Kind == "pay_out", maxText)
	if b != nil {
		return sync.Outcome{}, b
	}

	q := db.New(tx)
	shift, err := q.GetShift(ctx, db.GetShiftParams{TenantID: env.TenantID, ID: in.ShiftID})
	if err != nil {
		return notFoundIsPending(err, in.ShiftID)
	}
	if shift.OutletID != env.OutletID {
		return sync.Outcome{}, &bad{code: "wrong_outlet", detail: "the shift belongs to a different outlet"}
	}
	outlet, err := p.ten.OutletInTx(ctx, tx, env.TenantID, env.OutletID)
	if err != nil {
		return sync.Outcome{}, err
	}
	if err := q.InsertCashMovement(ctx, db.InsertCashMovementParams{
		ID: ev.ID, TenantID: env.TenantID, OutletID: env.OutletID, ShiftID: in.ShiftID, Kind: in.Kind, Amount: amount, Reason: reason,
		StaffID: ev.StaffID, DeviceTime: ev.DeviceTime, ReceivedAt: env.ReceivedAt, BusinessDate: pgDate(outlet.Settings.BusinessDate(ev.DeviceTime)),
	}); err != nil {
		return sync.Outcome{}, err
	}
	var flags []flagSpec
	if !env.Staff.Has(identity.PermDrawerOpenNoSale) {
		flags = append(flags, permissionFlag(env, identity.PermDrawerOpenNoSale))
	}
	if shift.DeviceID != env.DeviceID {
		flags = append(flags, flagSpec{Code: FlagShiftOtherDevice, Detail: map[string]any{"shift_id": in.ShiftID, "opened_on": shift.DeviceID}})
	}
	if shift.ClosedAt != nil && ev.DeviceTime.After(*shift.ClosedAt) {
		flags = append(flags, flagSpec{Code: FlagAfterShiftClose, Detail: map[string]any{"shift_id": in.ShiftID, "closed_at": shift.ClosedAt}})
	}
	flags = append(flags, clockFlags(env, ev)...)
	if err := raiseFlags(ctx, q, env, ev, "cash_movement", ev.ID, flags); err != nil {
		return sync.Outcome{}, err
	}
	return sync.Applied(ev.ID), nil
}

// clockFlags flags an event whose device clock was ahead of the server's by more than allowed. A
// clock that is behind only means the event waited to sync, which is normal.
func clockFlags(env sync.Env, ev sync.Event) []flagSpec {
	if ev.DeviceTime.After(env.ReceivedAt.Add(maxClockAhead)) {
		return []flagSpec{{Code: FlagDeviceTimeAhead, Detail: map[string]any{
			"device_time": ev.DeviceTime.UTC().Format(time.RFC3339), "received_at": env.ReceivedAt.UTC().Format(time.RFC3339),
		}}}
	}
	return nil
}
