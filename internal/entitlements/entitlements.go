// Package entitlements decides what a tenant may use: modules, limits and feature flags
// (BACKEND_PLAN.md section 4.5). One resolution rule serves handlers, jobs and the POS payload:
// a tenant override that has not expired, else the tenant's plan, else the key's default.
package entitlements

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Key names an entitlement. The keys themselves are rows in entitlement_key.
type Key string

const (
	ModuleInventory  Key = "module.inventory"
	ModuleRestaurant Key = "module.restaurant"
	LimitOutlets     Key = "limit.outlets"
	LimitDevices     Key = "limit.devices"
	LimitStaff       Key = "limit.staff"
)

// Unlimited is the value of a limit that does not apply.
const Unlimited = -1

// Source says which layer a value came from.
type Source string

const (
	SourceDefault  Source = "default"
	SourcePlan     Source = "plan"
	SourceOverride Source = "override"
)

// Entitlement is one resolved value. A bool is 0 or 1; a limit is a count or Unlimited.
type Entitlement struct {
	Key      Key
	Kind     string // "bool" or "int"
	Category string // "module", "limit" or "flag"
	Value    int64
	Source   Source
}

// Enabled reports whether a bool entitlement is on, or a limit allows anything at all.
func (e Entitlement) Enabled() bool { return e.Value != 0 }

// Snapshot is every entitlement of a tenant at one moment.
type Snapshot struct {
	Items       []Entitlement // ordered by key
	GeneratedAt time.Time
}

// Get returns one entitlement from the snapshot.
func (s Snapshot) Get(k Key) (Entitlement, bool) {
	for _, e := range s.Items {
		if e.Key == k {
			return e, true
		}
	}
	return Entitlement{}, false
}

var (
	// ErrModuleDisabled means the tenant's plan does not include a module. The API turns it into
	// 403 module_disabled; the module's data stays, only access is hidden.
	ErrModuleDisabled = errors.New("entitlements: module is not enabled")
	// ErrLimitReached means creating one more would exceed a limit.
	ErrLimitReached = errors.New("entitlements: limit reached")
	// ErrUnknownKey means no such entitlement exists.
	ErrUnknownKey = errors.New("entitlements: unknown key")
)

// LimitError says which limit was hit.
type LimitError struct {
	Key   Key
	Limit int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("entitlements: %s limit of %d reached", e.Key, e.Limit)
}
func (e *LimitError) Unwrap() error { return ErrLimitReached }

// cacheTTL bounds how stale a snapshot can be. Overrides are written by operator commands in
// another process, so a TTL (not invalidation) is what bounds staleness across processes.
const (
	cacheTTL     = 30 * time.Second
	cacheMaxSize = 10_000
)

// Resolver resolves entitlements. Snapshots are cached in process for 30 seconds; Invalidate drops
// one tenant's entry (for writes made in this process).
type Resolver struct {
	pool  *pgxpool.Pool
	clock kernel.Clock

	mu    sync.Mutex
	cache map[uuid.UUID]cached
}

type cached struct {
	snap    Snapshot
	expires time.Time
}

// NewResolver returns a Resolver reading through pool, a member of orion_app.
func NewResolver(pool *pgxpool.Pool, clock kernel.Clock) *Resolver {
	if clock == nil {
		clock = kernel.SystemClock{}
	}
	return &Resolver{pool: pool, clock: clock, cache: map[uuid.UUID]cached{}}
}

// Snapshot returns every entitlement of the tenant, from cache when fresh.
func (r *Resolver) Snapshot(ctx context.Context, tenantID uuid.UUID) (Snapshot, error) {
	now := r.clock.Now()
	r.mu.Lock()
	c, ok := r.cache[tenantID]
	r.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.snap, nil
	}

	var snap Snapshot
	err := kernel.TenantTx(ctx, r.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListEntitlements(ctx, db.ListEntitlementsParams{TenantID: tenantID, Now: &now})
		if err != nil {
			return err
		}
		snap = Snapshot{GeneratedAt: now, Items: make([]Entitlement, len(rows))}
		for i, row := range rows {
			snap.Items[i] = resolve(row.Key, row.Kind, row.Category, row.DefaultValue, row.PlanValue, row.OverrideValue)
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if len(snap.Items) == 0 {
		return Snapshot{}, kernel.ErrNotFound // the tenant does not exist
	}

	r.mu.Lock()
	if len(r.cache) >= cacheMaxSize {
		clear(r.cache)
	}
	r.cache[tenantID] = cached{snap: snap, expires: now.Add(cacheTTL)}
	r.mu.Unlock()
	return snap, nil
}

// Invalidate drops the cached snapshot of a tenant.
func (r *Resolver) Invalidate(tenantID uuid.UUID) {
	r.mu.Lock()
	delete(r.cache, tenantID)
	r.mu.Unlock()
}

// Get resolves one key.
func (r *Resolver) Get(ctx context.Context, tenantID uuid.UUID, key Key) (Entitlement, error) {
	snap, err := r.Snapshot(ctx, tenantID)
	if err != nil {
		return Entitlement{}, err
	}
	e, ok := snap.Get(key)
	if !ok {
		return Entitlement{}, fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	return e, nil
}

// RequireModule returns ErrModuleDisabled unless the module is on for the tenant.
func (r *Resolver) RequireModule(ctx context.Context, tenantID uuid.UUID, key Key) error {
	e, err := r.Get(ctx, tenantID, key)
	if err != nil {
		return err
	}
	if !e.Enabled() {
		return fmt.Errorf("%w: %s", ErrModuleDisabled, key)
	}
	return nil
}

// CheckLimit says whether the tenant of tx may create one more of something, given how many it
// has now. Call it inside the creating transaction, after kernel.LockTenant, so two requests
// cannot both take the last slot. It reads inside tx and bypasses the cache, for that reason.
func (r *Resolver) CheckLimit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, key Key, used int64) error {
	now := r.clock.Now()
	row, err := db.New(tx).GetEntitlement(ctx, db.GetEntitlementParams{TenantID: tenantID, Key: string(key), Now: &now})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}
	if err != nil {
		return err
	}
	e := resolve(row.Key, row.Kind, row.Category, row.DefaultValue, row.PlanValue, row.OverrideValue)
	if e.Kind != "int" {
		return fmt.Errorf("%w: %s is not a limit", ErrUnknownKey, key)
	}
	if e.Value != Unlimited && used+1 > e.Value {
		return &LimitError{Key: key, Limit: e.Value}
	}
	return nil
}

func resolve(key, kind, category string, def int64, plan, override *int64) Entitlement {
	e := Entitlement{Key: Key(key), Kind: kind, Category: category, Value: def, Source: SourceDefault}
	if plan != nil {
		e.Value, e.Source = *plan, SourcePlan
	}
	if override != nil {
		e.Value, e.Source = *override, SourceOverride
	}
	return e
}

// Admin writes overrides. It connects as orion_platform and belongs to operator tooling.
type Admin struct {
	pool *pgxpool.Pool
}

// NewAdmin returns an Admin using pool, a member of orion_platform.
func NewAdmin(pool *pgxpool.Pool) *Admin { return &Admin{pool: pool} }

// Override is a per-tenant exception to the plan.
type Override struct {
	TenantID   uuid.UUID
	Key        Key
	Value      int64
	Reason     string // required: why an operator did this
	ExpiresAt  *time.Time
	OperatorID *uuid.UUID
}

// SetOverride creates or replaces an override.
func (a *Admin) SetOverride(ctx context.Context, o Override) error {
	return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error { return a.SetOverrideTx(ctx, tx, o) })
}

// SetOverrideTx is SetOverride inside the caller's transaction, so operator tooling can write the
// platform audit entry atomically with the change.
func (a *Admin) SetOverrideTx(ctx context.Context, tx pgx.Tx, o Override) error {
	if o.Reason == "" {
		return fmt.Errorf("%w: a reason is required", kernel.ErrValidation)
	}
	q := db.New(tx)
	kind, err := q.GetKeyKind(ctx, string(o.Key))
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownKey, o.Key)
	}
	if err != nil {
		return err
	}
	if o.Value < Unlimited || (kind == "bool" && o.Value > 1) {
		return fmt.Errorf("%w: %s takes %s", kernel.ErrValidation, o.Key, map[string]string{"bool": "0 or 1", "int": "a count or -1 for unlimited"}[kind])
	}
	return q.UpsertOverride(ctx, db.UpsertOverrideParams{
		TenantID: o.TenantID, Key: string(o.Key), Value: o.Value, Reason: o.Reason,
		ExpiresAt: o.ExpiresAt, SetByOperatorID: o.OperatorID,
	})
}

// GetOverrideTx returns the current override, or nil if there is none.
func (a *Admin) GetOverrideTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, key Key) (*Override, error) {
	row, err := db.New(tx).GetOverride(ctx, db.GetOverrideParams{TenantID: tenantID, Key: string(key)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Override{TenantID: tenantID, Key: key, Value: row.Value, Reason: row.Reason, ExpiresAt: row.ExpiresAt}, nil
}

// ClearOverride removes an override and reports whether there was one.
func (a *Admin) ClearOverride(ctx context.Context, tenantID uuid.UUID, key Key) (bool, error) {
	var ok bool
	err := pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) (err error) { ok, err = a.ClearOverrideTx(ctx, tx, tenantID, key); return })
	return ok, err
}

// ClearOverrideTx is ClearOverride inside the caller's transaction.
func (a *Admin) ClearOverrideTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, key Key) (bool, error) {
	n, err := db.New(tx).DeleteOverride(ctx, db.DeleteOverrideParams{TenantID: tenantID, Key: string(key)})
	return n > 0, err
}
