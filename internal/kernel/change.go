package kernel

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RecordChange appends a change to the current tenant's change log and returns its sequence
// number, for the POS pull (BACKEND_PLAN.md section 5.2). op is "upsert" or "delete". outletID is
// nil when the change applies to every outlet.
//
// It takes the tenant row lock, so call LockTenant first in the transaction (lock order, section
// 4.12). Record the change in the same transaction as the write it describes.
func RecordChange(ctx context.Context, tx pgx.Tx, entityType string, entityID uuid.UUID, op string, outletID *uuid.UUID) (int64, error) {
	var seq int64
	err := tx.QueryRow(ctx, "SELECT record_change($1, $2, $3, $4)", entityType, entityID, op, outletID).Scan(&seq)
	return seq, err
}

// RecordChanges is RecordChange for many entities of one type in one round trip, for bulk writes
// such as the catalog import.
func RecordChanges(ctx context.Context, tx pgx.Tx, entityType string, ids []uuid.UUID) error {
	_, err := tx.Exec(ctx, "SELECT record_change($1, id, 'upsert', NULL) FROM unnest($2::uuid[]) AS id", entityType, ids)
	return err
}

// Change is one entry of a tenant's change log.
type Change struct {
	Seq        int64
	EntityType string
	EntityID   uuid.UUID
	Op         string // "upsert" or "delete"
	OutletID   *uuid.UUID
}

// ChangeHead returns the current tenant's latest change number. Read it before reading any state
// that a pull will send: everything numbered up to it is already committed and visible, and
// anything committed later has a higher number and arrives in the next pull.
func ChangeHead(ctx context.Context, tx pgx.Tx) (int64, error) {
	var head int64
	err := tx.QueryRow(ctx, "SELECT change_seq FROM tenant WHERE id = current_tenant_id()").Scan(&head)
	return head, err
}

// ChangesSince returns the current tenant's changes numbered after and up to upTo, in order, for
// one outlet's device: changes that apply to every outlet and those for outletID. It returns at
// most limit rows.
func ChangesSince(ctx context.Context, tx pgx.Tx, after, upTo int64, outletID uuid.UUID, limit int) ([]Change, error) {
	rows, err := tx.Query(ctx, `
		SELECT seq, entity_type, entity_id, op, outlet_id FROM change_log
		WHERE tenant_id = current_tenant_id() AND seq > $1 AND seq <= $2 AND (outlet_id IS NULL OR outlet_id = $3)
		ORDER BY seq LIMIT $4`, after, upTo, outletID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Change, error) {
		var c Change
		err := r.Scan(&c.Seq, &c.EntityType, &c.EntityID, &c.Op, &c.OutletID)
		return c, err
	})
}
