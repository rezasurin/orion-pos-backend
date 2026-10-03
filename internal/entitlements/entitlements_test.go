package entitlements_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type env struct {
	d      *testdb.DB
	r      *entitlements.Resolver
	admin  *entitlements.Admin
	clock  *clock
	tenant uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	e := &env{d: d, r: entitlements.NewResolver(d.App, c), admin: entitlements.NewAdmin(d.Platform), clock: c, tenant: kernel.NewID()}
	d.Exec(t, `INSERT INTO tenant (id, name, slug, plan_id) SELECT $1, 'Kopi', 'kopi', id FROM plan WHERE code = 'early_access'`, e.tenant)
	return e
}

// restrict moves the tenant to a plan with real limits, as a paid plan would have.
func (e *env) restrict(t *testing.T) {
	t.Helper()
	plan := kernel.NewID()
	e.d.Exec(t, `INSERT INTO plan (id, code, name) VALUES ($1, 'starter', 'Starter')`, plan)
	e.d.Exec(t, `INSERT INTO plan_entitlement (plan_id, key, value) VALUES ($1, 'limit.devices', 3), ($1, 'module.inventory', 0)`, plan)
	e.d.Exec(t, `UPDATE tenant SET plan_id = $1 WHERE id = $2`, plan, e.tenant)
	e.r.Invalidate(e.tenant)
}

func (e *env) get(t *testing.T, key entitlements.Key) entitlements.Entitlement {
	t.Helper()
	got, err := e.r.Get(context.Background(), e.tenant, key)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEarlyAccessIsUnrestricted(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	snap, err := e.r.Snapshot(ctx, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 5 {
		t.Fatalf("snapshot has %d entitlements, want the 5 seeded keys", len(snap.Items))
	}
	for _, en := range snap.Items {
		want := int64(1)
		if en.Kind == "int" {
			want = entitlements.Unlimited
		}
		if en.Value != want || en.Source != entitlements.SourcePlan {
			t.Errorf("%s = %d from %s, want %d from the plan", en.Key, en.Value, en.Source, want)
		}
	}
	if err := e.r.RequireModule(ctx, e.tenant, entitlements.ModuleInventory); err != nil {
		t.Errorf("early access lacks inventory: %v", err)
	}
	if _, err := e.r.Snapshot(ctx, uuid.New()); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown tenant: %v, want ErrNotFound", err)
	}
	if _, err := e.r.Get(ctx, e.tenant, "limit.nope"); !errors.Is(err, entitlements.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestResolutionOrder(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.restrict(t)

	// plan value, then default where the plan is silent.
	if g := e.get(t, entitlements.LimitDevices); g.Value != 3 || g.Source != entitlements.SourcePlan {
		t.Errorf("devices = %+v, want 3 from the plan", g)
	}
	if g := e.get(t, entitlements.LimitStaff); g.Value != 5 || g.Source != entitlements.SourceDefault {
		t.Errorf("staff = %+v, want the default 5", g)
	}
	if err := e.r.RequireModule(ctx, e.tenant, entitlements.ModuleInventory); !errors.Is(err, entitlements.ErrModuleDisabled) {
		t.Errorf("inventory on the starter plan: %v, want ErrModuleDisabled", err)
	}

	// An override beats the plan, until it expires.
	exp := e.clock.Now().Add(time.Hour)
	if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: e.tenant, Key: entitlements.LimitDevices, Value: 10, Reason: "pilot", ExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	e.r.Invalidate(e.tenant)
	if g := e.get(t, entitlements.LimitDevices); g.Value != 10 || g.Source != entitlements.SourceOverride {
		t.Errorf("devices = %+v, want 10 from the override", g)
	}
	e.clock.Advance(2 * time.Hour)
	if g := e.get(t, entitlements.LimitDevices); g.Value != 3 || g.Source != entitlements.SourcePlan {
		t.Errorf("devices after expiry = %+v, want the plan's 3", g)
	}

	// A permanent override can switch a module on, and clearing it restores the plan.
	if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: e.tenant, Key: entitlements.ModuleInventory, Value: 1, Reason: "beta"}); err != nil {
		t.Fatal(err)
	}
	e.r.Invalidate(e.tenant)
	if err := e.r.RequireModule(ctx, e.tenant, entitlements.ModuleInventory); err != nil {
		t.Errorf("override should enable the module: %v", err)
	}
	if ok, err := e.admin.ClearOverride(ctx, e.tenant, entitlements.ModuleInventory); err != nil || !ok {
		t.Fatalf("clear: %v %v", ok, err)
	}
	if ok, _ := e.admin.ClearOverride(ctx, e.tenant, entitlements.ModuleInventory); ok {
		t.Error("clearing twice reported a removal")
	}
	e.r.Invalidate(e.tenant)
	if err := e.r.RequireModule(ctx, e.tenant, entitlements.ModuleInventory); !errors.Is(err, entitlements.ErrModuleDisabled) {
		t.Errorf("after clearing: %v, want ErrModuleDisabled", err)
	}
}

func TestSnapshotIsCachedFor30Seconds(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	if _, err := e.r.Snapshot(ctx, e.tenant); err != nil {
		t.Fatal(err)
	}
	if n := e.d.Queries.During(func() { _, _ = e.r.Snapshot(ctx, e.tenant) }); n != 0 {
		t.Errorf("a cached snapshot sent %d queries", n)
	}
	e.clock.Advance(31 * time.Second)
	if n := e.d.Queries.During(func() { _, _ = e.r.Snapshot(ctx, e.tenant) }); n == 0 {
		t.Error("an expired snapshot was not reloaded")
	}
	e.r.Invalidate(e.tenant)
	if n := e.d.Queries.During(func() { _, _ = e.r.Snapshot(ctx, e.tenant) }); n == 0 {
		t.Error("Invalidate did not drop the entry")
	}
	// One query loads the whole snapshot (plus transaction control), however many keys exist.
	e.r.Invalidate(e.tenant)
	few := e.d.Queries.During(func() { _, _ = e.r.Snapshot(ctx, e.tenant) })
	e.d.Exec(t, `INSERT INTO entitlement_key (key, kind, category, description, owner, default_value)
		SELECT 'flag.f' || g, 'bool', 'flag', 'x', 'dev', 0 FROM generate_series(1, 30) g`)
	e.r.Invalidate(e.tenant)
	if many := e.d.Queries.During(func() { _, _ = e.r.Snapshot(ctx, e.tenant) }); many != few {
		t.Errorf("queries with 35 keys = %d, with 5 = %d", many, few)
	}
}

func TestCheckLimit(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.restrict(t)
	check := func(key entitlements.Key, used int64) error {
		return kernel.TenantTx(ctx, e.d.App, e.tenant, func(tx pgx.Tx) error {
			return e.r.CheckLimit(ctx, tx, e.tenant, key, used)
		})
	}
	for used, want := range map[int64]bool{0: true, 2: true, 3: false, 7: false} {
		err := check(entitlements.LimitDevices, used)
		if (err == nil) != want {
			t.Errorf("used %d of 3: err = %v, want allowed=%v", used, err, want)
		}
	}
	var le *entitlements.LimitError
	if err := check(entitlements.LimitDevices, 3); !errors.As(err, &le) || le.Limit != 3 || !errors.Is(err, entitlements.ErrLimitReached) {
		t.Errorf("err = %v, want a LimitError for 3", err)
	}
	if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: e.tenant, Key: entitlements.LimitDevices, Value: entitlements.Unlimited, Reason: "vip"}); err != nil {
		t.Fatal(err)
	}
	if err := check(entitlements.LimitDevices, 1_000_000); err != nil {
		t.Errorf("unlimited refused: %v", err)
	}
	if err := check(entitlements.ModuleInventory, 0); !errors.Is(err, entitlements.ErrUnknownKey) {
		t.Errorf("a module is not a limit: %v", err)
	}
	if err := check("limit.nope", 0); !errors.Is(err, entitlements.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestAdminValidation(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	for name, o := range map[string]entitlements.Override{
		"no reason": {TenantID: e.tenant, Key: entitlements.LimitDevices, Value: 1},
		"bool of 2": {TenantID: e.tenant, Key: entitlements.ModuleInventory, Value: 2, Reason: "x"},
		"below -1":  {TenantID: e.tenant, Key: entitlements.LimitDevices, Value: -2, Reason: "x"},
	} {
		if err := e.admin.SetOverride(ctx, o); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: %v, want ErrValidation", name, err)
		}
	}
	if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: e.tenant, Key: "limit.nope", Value: 1, Reason: "x"}); !errors.Is(err, entitlements.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}
	// Replacing an override updates it in place.
	for _, v := range []int64{4, 6} {
		if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: e.tenant, Key: entitlements.LimitDevices, Value: v, Reason: "again"}); err != nil {
			t.Fatal(err)
		}
	}
	e.r.Invalidate(e.tenant)
	if g := e.get(t, entitlements.LimitDevices); g.Value != 6 {
		t.Errorf("devices = %d, want the latest 6", g.Value)
	}
}

func TestOverridesAreIsolatedAndReadOnlyForTheService(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	other := kernel.NewID()
	e.d.Exec(t, `INSERT INTO tenant (id, name, slug, plan_id) SELECT $1, 'Tea', 'tea', id FROM plan WHERE code = 'early_access'`, other)
	if err := e.admin.SetOverride(ctx, entitlements.Override{TenantID: other, Key: entitlements.LimitStaff, Value: 99, Reason: "theirs"}); err != nil {
		t.Fatal(err)
	}
	if g := e.get(t, entitlements.LimitStaff); g.Source == entitlements.SourceOverride {
		t.Error("another tenant's override applies here")
	}
	var n int
	_ = kernel.TenantTx(ctx, e.d.App, e.tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM tenant_entitlement_override`).Scan(&n)
	})
	if n != 0 {
		t.Errorf("the service sees %d overrides of other tenants", n)
	}
	for _, sql := range []string{
		`INSERT INTO tenant_entitlement_override (tenant_id, key, value, reason) VALUES (current_tenant_id(), 'limit.staff', 99, 'self-service')`,
		`UPDATE plan_entitlement SET value = -1`,
		`UPDATE entitlement_key SET default_value = 100`,
	} {
		err := kernel.TenantTx(ctx, e.d.App, e.tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: err = %v, want 42501", sql, err)
		}
	}
}
