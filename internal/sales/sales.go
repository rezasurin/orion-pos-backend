// Package sales owns shifts, cash movements, sales, payments, voids, refunds and the review flags
// raised about them (BACKEND_PLAN.md sections 5.1 and 6.4). It writes them only by projecting the
// events a device pushes, through the sync module's Projector interface; nothing else creates a
// sale.
//
// Two rules shape everything here. A sale is recorded as the device rang it up: the amounts are
// what the customer was charged, and the server never rewrites them. And only malformed events are
// rejected; a sale whose totals do not recompute, made by someone without the permission, or
// priced with an out-of-date catalog is accepted and flagged, because the money has already
// changed hands.
package sales

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// Flag codes. A flag says what a reviewer should look at; the event it is about stands.
const (
	FlagTotalMismatch     = "total_mismatch"     // the amounts do not recompute from the lines
	FlagPricingInvalid    = "pricing_invalid"    // the lines and discounts are not a bill the algorithm can price
	FlagPricingVersion    = "pricing_version"    // priced with an algorithm version this server does not know
	FlagPaymentMismatch   = "payment_mismatch"   // the payments do not add up to what was due
	FlagPermissionMissing = "permission_missing" // done by someone who lacked the permission
	FlagStalePrice        = "stale_price"        // a price from before an update the device could have had
	FlagSaleOutsideShift  = "sale_outside_shift" // rung up before the shift opened or after it closed
	FlagShiftOtherDevice  = "shift_other_device" // on a shift another device opened
	FlagDeviceTimeAhead   = "device_time_ahead"  // the device's clock was ahead of the server's
	FlagAfterShiftClose   = "after_shift_close"  // a cash movement after the shift closed
	// A refund that gives back more of a line than was sold, or more money than the sale took, over
	// all of the sale's refunds.
	FlagRefundOverQuantity = "refund_over_quantity"
	FlagRefundOverPaid     = "refund_over_paid"
)

// maxClockAhead is how far a device's clock may be ahead of the server's when the event arrives
// before the event is flagged. Behind is normal: that is just an event that waited to sync.
const maxClockAhead = 10 * time.Minute

// maxMismatchesListed caps how many differing fields a total_mismatch flag lists.
const maxMismatchesListed = 20

// Deps are what the projector needs from other modules.
type Deps struct {
	Identity *identity.Service
	Tenancy  *tenancy.Service
	Catalog  *catalog.Service
}

// Projector projects the event types in Handles. It implements sync.Projector.
type Projector struct {
	ids *identity.Service
	ten *tenancy.Service
	cat *catalog.Service
}

var _ sync.Projector = (*Projector)(nil)

// NewProjector returns the sales projector.
func NewProjector(d Deps) *Projector {
	return &Projector{ids: d.Identity, ten: d.Tenancy, cat: d.Catalog}
}

// Handles implements sync.Projector.
func (p *Projector) Handles() map[string]int {
	return map[string]int{TypeShiftOpened: 1, TypeShiftClosed: 1, TypeCashMovement: 1, TypeSaleCompleted: 1, TypeSaleVoided: 1, TypeRefundIssued: 1}
}

// Project implements sync.Projector.
func (p *Projector) Project(ctx context.Context, tx pgx.Tx, env sync.Env, ev sync.Event) (sync.Outcome, error) {
	var (
		out sync.Outcome
		err error
	)
	switch ev.Type {
	case TypeShiftOpened:
		out, err = p.shiftOpened(ctx, tx, env, ev)
	case TypeShiftClosed:
		out, err = p.shiftClosed(ctx, tx, env, ev)
	case TypeCashMovement:
		out, err = p.cashMovement(ctx, tx, env, ev)
	case TypeSaleCompleted:
		out, err = p.saleCompleted(ctx, tx, env, ev)
	case TypeRefundIssued:
		out, err = p.refundIssued(ctx, tx, env, ev)
	case TypeSaleVoided:
		out, err = p.saleVoided(ctx, tx, env, ev)
	default:
		return sync.Outcome{}, fmt.Errorf("sales: no handler for %q", ev.Type)
	}
	var b *bad
	if asBad(err, &b) {
		return sync.Rejected(b.code, b.detail), nil
	}
	return out, err
}

func asBad(err error, target **bad) bool {
	b, ok := err.(*bad) //nolint:errorlint // *bad is only ever returned directly, never wrapped
	if ok {
		*target = b
	}
	return ok
}

// flagSpec is a flag to raise for the event being projected.
type flagSpec struct {
	Code   string
	Detail map[string]any
}

// raiseFlags writes the flags for a target in one round trip.
func raiseFlags(ctx context.Context, q *db.Queries, env sync.Env, ev sync.Event, targetType string, targetID uuid.UUID, flags []flagSpec) error {
	if len(flags) == 0 {
		return nil
	}
	params := make([]db.InsertFlagParams, len(flags))
	for i, f := range flags {
		detail := f.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		raw, err := json.Marshal(detail)
		if err != nil {
			return err
		}
		params[i] = db.InsertFlagParams{
			ID: kernel.NewID(), TenantID: env.TenantID, OutletID: env.OutletID, DeviceID: env.DeviceID, EventID: ev.ID,
			TargetType: targetType, TargetID: targetID, Code: f.Code, Detail: raw,
		}
	}
	return runBatch(q.InsertFlag(ctx, params).Exec)
}

// runBatch runs a pgx batch and returns its first error. The error is returned as it is, so the
// sync core can tell a constraint violation (a rejection) from a server fault.
func runBatch(exec func(func(int, error))) error {
	var first error
	exec(func(_ int, err error) {
		if err != nil && first == nil {
			first = err
		}
	})
	return first
}

// pgDate is a business date (midnight UTC of the date) as a database date.
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }

// notFoundIsPending turns "no such row" into a wait for the record to arrive: events may reach the
// server out of order, and the one they refer to may simply not have been pushed yet.
func notFoundIsPending(err error, id uuid.UUID) (sync.Outcome, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return sync.PendingOn(id), nil
	}
	return sync.Outcome{}, err
}

// constraintOf returns the name of the database constraint an error violated, or "".
func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
