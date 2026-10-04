package kernel

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoTenant is returned when a tenant transaction is started without a tenant.
var ErrNoTenant = errors.New("kernel: no tenant in context")

const (
	// lockTimeout bounds how long any statement waits for a row lock. A transaction stuck
	// behind another fails fast with 55P03 instead of piling up connections.
	lockTimeout = "5s"
	// txAttempts is how many times a transaction is tried when Postgres aborts it with a
	// deadlock or serialization failure.
	txAttempts = 3
)

// TenantTx runs fn in a transaction scoped to tenantID. It sets app.tenant_id for the transaction
// only, so row-level security confines every query in fn to that tenant, and the setting cannot
// leak to the next user of the pooled connection.
//
// If Postgres aborts the transaction with a deadlock (40P01) or a serialization failure (40001),
// TenantTx runs fn again, up to three attempts in total. fn must therefore do nothing outside the
// transaction: no HTTP calls, no emails. Side effects go through jobs inserted in the same
// transaction.
//
// Modules that must write atomically together (for example a sale and its stock consumption)
// share the pgx.Tx that fn receives; each still writes only its own tables.
func TenantTx(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return ErrNoTenant
	}
	var err error
	for attempt := 1; attempt <= txAttempts; attempt++ {
		err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				"SELECT set_config('app.tenant_id', $1, true), set_config('lock_timeout', $2, true)",
				tenantID.String(), lockTimeout,
			); err != nil {
				return err
			}
			return fn(tx)
		})
		if !isRetryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(attempt*attempt) * 10 * time.Millisecond):
		}
	}
	return err
}

// LockTenant takes the tenant row lock that record_change also takes. Lock order rule: a
// transaction that will record a change, or that checks a per-tenant limit, calls this before it
// locks or updates any other row. Every writer then queues on the same first lock, so two
// transactions cannot each hold a row the other needs.
//
// It uses FOR NO KEY UPDATE, which does not block inserts whose foreign keys point at the tenant.
func LockTenant(ctx context.Context, tx pgx.Tx) error {
	var one int
	err := tx.QueryRow(ctx,
		"SELECT 1 FROM tenant WHERE id = current_tenant_id() FOR NO KEY UPDATE").Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoTenant
	}
	return err
}

func isRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40P01" || pgErr.Code == "40001"
}
