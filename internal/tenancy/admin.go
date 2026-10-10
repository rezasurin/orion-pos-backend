package tenancy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
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

// Find looks a tenant up by id, or by slug, inside the caller's transaction. A reference that parses
// as a UUID is tried as an id first, then as a slug, since a slug may look like one.
func (a *Admin) Find(ctx context.Context, tx pgx.Tx, ref string) (TenantRef, error) {
	if id, err := uuid.Parse(ref); err == nil {
		t, err := db.New(tx).GetTenant(ctx, id)
		if err == nil {
			return TenantRef{ID: t.ID, Name: t.Name, Slug: t.Slug, SuspendedAt: t.SuspendedAt}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return TenantRef{}, err
		}
	}
	return a.FindBySlug(ctx, tx, ref)
}

// PlanCodeTx returns the code of a tenant's plan.
func (a *Admin) PlanCodeTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (string, error) {
	code, err := db.New(tx).GetTenantPlanCode(ctx, id)
	return code, mapErr(err)
}

// SetPlanTx moves a tenant to another plan. Early access brings back its status; any other plan
// makes the subscription active (billing, Phase 5, will own that later).
func (a *Admin) SetPlanTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, planCode string) error {
	n, err := db.New(tx).SetTenantPlan(ctx, db.SetTenantPlanParams{ID: id, PlanCode: planCode})
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: no plan %q", kernel.ErrValidation, planCode)
	}
	return nil
}
