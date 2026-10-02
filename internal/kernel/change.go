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
