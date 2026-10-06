package tenancy

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/tenancy/db"
)

// Admin is tenancy's operator-facing side. It connects as orion_platform, so it sees every tenant,
// and belongs to the platform module's tooling.
type Admin struct{ pool *pgxpool.Pool }

// NewAdmin returns an Admin using pool, a member of orion_platform.
func NewAdmin(pool *pgxpool.Pool) *Admin { return &Admin{pool: pool} }

// TenantRef is what operator tooling needs to know about a tenant.
type TenantRef struct {
	ID          uuid.UUID
	Name        string
	Slug        string
	SuspendedAt *time.Time
}

// FindBySlug looks a tenant up by its slug inside the caller's transaction.
func (a *Admin) FindBySlug(ctx context.Context, tx pgx.Tx, slug string) (TenantRef, error) {
	t, err := db.New(tx).GetTenantBySlug(ctx, slug)
	if err != nil {
		return TenantRef{}, mapErr(err)
	}
	return TenantRef{ID: t.ID, Name: t.Name, Slug: t.Slug, SuspendedAt: t.SuspendedAt}, nil
}

// SetSuspended suspends (at now) or reinstates a tenant inside the caller's transaction. While
// suspended, its people can read but not change anything, and its tablets keep syncing; its data is
// untouched.
func (a *Admin) SetSuspended(ctx context.Context, tx pgx.Tx, id uuid.UUID, suspended bool, now time.Time) error {
	var at *time.Time
	if suspended {
		at = &now
	}
	return db.New(tx).SetTenantSuspended(ctx, db.SetTenantSuspendedParams{ID: id, SuspendedAt: at})
}
