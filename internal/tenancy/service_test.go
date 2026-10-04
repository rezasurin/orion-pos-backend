package tenancy_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func newTenant(slug, code string) tenancy.NewTenant {
	return tenancy.NewTenant{
		Name:   "Kopi " + slug,
		Slug:   slug,
		Outlet: tenancy.NewOutlet{Name: "Pusat", Code: code, Address: "Jl. Braga 1"},
	}
}

func TestCreateTenant(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()

	tenant, outlet, err := svc.CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))
	if err != nil {
		t.Fatal(err)
	}
	if tenant.SubscriptionStatus != "early_access" {
		t.Errorf("status = %q, want early_access", tenant.SubscriptionStatus)
	}
	if outlet.TenantID != tenant.ID || outlet.Code != "BDG1" {
		t.Errorf("outlet = %+v", outlet)
	}
	if outlet.Settings.Timezone != "Asia/Jakarta" || outlet.Settings.CashRoundingMode != "nearest" {
		t.Errorf("settings defaults = %+v", outlet.Settings)
	}

	var plan string
	if err := d.Owner.QueryRow(ctx,
		`SELECT p.code FROM tenant t JOIN plan p ON p.id = t.plan_id WHERE t.id = $1`, tenant.ID).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if plan != "early_access" {
		t.Errorf("plan = %q, want early_access", plan)
	}

	// The four system roles are seeded with the tenant, Owner holding every permission.
	var roles []string
	rows, err := d.Owner.Query(ctx, `SELECT name FROM role WHERE tenant_id = $1 AND is_system ORDER BY name`, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if roles, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Cashier", "Kitchen", "Manager", "Owner"}; !slices.Equal(roles, want) {
		t.Errorf("roles = %v, want %v", roles, want)
	}
	var ownerPerms int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM role_permission rp JOIN role r ON r.id = rp.role_id
		WHERE r.tenant_id = $1 AND r.name = 'Owner'`, tenant.ID).Scan(&ownerPerms); err != nil || ownerPerms != len(identity.AllPermissions()) {
		t.Errorf("Owner has %d permissions (%v), want %d", ownerPerms, err, len(identity.AllPermissions()))
	}

	// The outlet and its settings are in the change log, numbered 1 and 2.
	rows, err = d.Owner.Query(ctx,
		`SELECT seq, entity_type FROM change_log WHERE tenant_id = $1 ORDER BY seq`, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	type change struct {
		Seq    int64
		Entity string
	}
	changes, err := pgx.CollectRows(rows, pgx.RowToStructByPos[change])
	if err != nil {
		t.Fatal(err)
	}
	want := []change{{1, "outlet"}, {2, "outlet_settings"}}
	if len(changes) != len(want) || changes[0] != want[0] || changes[1] != want[1] {
		t.Errorf("changes = %v, want %v", changes, want)
	}
}

func TestCreateTenantValidationAndConflicts(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()

	invalid := []tenancy.NewTenant{
		newTenant("Kopi Senja", "BDG1"),     // slug with capitals and a space
		newTenant("kopi-senja", "bdg1"),     // lowercase outlet code
		newTenant("kopi-senja", "TOOLONG1"), // outlet code too long
		{Name: " ", Slug: "kopi", Outlet: tenancy.NewOutlet{Name: "Pusat", Code: "BDG1"}},
		{Name: "Kopi", Slug: "kopi", Outlet: tenancy.NewOutlet{Name: "Pusat", Code: "BDG1", Timezone: "Asia/Singapore"}},
	}
	for _, in := range invalid {
		if _, _, err := svc.CreateTenant(ctx, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("CreateTenant(%+v) err = %v, want ErrValidation", in, err)
		}
	}

	if _, _, err := svc.CreateTenant(ctx, newTenant("kopi-senja", "BDG1")); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.CreateTenant(ctx, newTenant("kopi-senja", "JKT1"))
	if !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate slug err = %v, want ErrConflict", err)
	}
}

func TestListAndGetOutlets(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()

	tenant, outlet, err := svc.CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))
	if err != nil {
		t.Fatal(err)
	}
	outlets, err := svc.ListOutlets(ctx, tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outlets) != 1 || outlets[0].ID != outlet.ID {
		t.Fatalf("outlets = %+v", outlets)
	}
	got, err := svc.GetOutlet(ctx, tenant.ID, outlet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings != outlet.Settings {
		t.Errorf("GetOutlet settings = %+v, want %+v", got.Settings, outlet.Settings)
	}
	if _, err := svc.GetOutlet(ctx, tenant.ID, kernel.NewID()); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown outlet err = %v, want ErrNotFound", err)
	}
}

// Row-level security keeps each tenant's rows invisible to every other tenant, even when the
// query names the other tenant's ids directly.
func TestTenantIsolation(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()

	a, outletA, err := svc.CreateTenant(ctx, newTenant("tenant-a", "AAA1"))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.CreateTenant(ctx, newTenant("tenant-b", "BBB1"))
	if err != nil {
		t.Fatal(err)
	}

	// Through the service: tenant B asking for tenant A's outlet finds nothing.
	if _, err := svc.GetOutlet(ctx, b.ID, outletA.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("cross-tenant GetOutlet err = %v, want ErrNotFound", err)
	}
	if _, err := svc.GetTenant(ctx, a.ID); err != nil {
		t.Errorf("own tenant: %v", err)
	}

	// Raw SQL as tenant B, with no WHERE clause at all, still sees only B.
	err = kernel.TenantTx(ctx, d.App, b.ID, func(tx pgx.Tx) error {
		for _, table := range []string{"tenant", "outlet", "outlet_settings", "change_log"} {
			col := "tenant_id"
			if table == "tenant" {
				col = "id"
			}
			var other int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+col+" <> $1", b.ID).Scan(&other); err != nil {
				return err
			}
			if other != 0 {
				t.Errorf("tenant B sees %d rows of other tenants in %s", other, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Tenant B cannot write a row that belongs to tenant A.
	err = kernel.TenantTx(ctx, d.App, b.ID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO outlet (id, tenant_id, name, code) VALUES ($1, $2, 'Sneaky', 'SNK1')`, kernel.NewID(), a.ID)
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("cross-tenant insert err = %v, want insufficient_privilege (42501)", err)
	}

	// Tenant B updating tenant A's outlet matches no rows.
	err = kernel.TenantTx(ctx, d.App, b.ID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE outlet SET name = 'Hijacked' WHERE id = $1`, outletA.ID)
		if err == nil && tag.RowsAffected() != 0 {
			t.Errorf("cross-tenant update affected %d rows", tag.RowsAffected())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Without a tenant in the transaction, the app role sees nothing: RLS fails closed.
func TestNoTenantContextSeesNothing(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, _, err := tenancy.NewService(d.App).CreateTenant(ctx, newTenant("kopi-senja", "BDG1")); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tenant", "outlet", "outlet_settings", "change_log"} {
		var n int
		if err := d.App.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s: %d rows visible without a tenant", table, n)
		}
	}
	if err := kernel.TenantTx(ctx, d.App, uuid.Nil, func(pgx.Tx) error { return nil }); !errors.Is(err, kernel.ErrNoTenant) {
		t.Errorf("TenantTx(nil tenant) err = %v, want ErrNoTenant", err)
	}
}

// The platform role sees every tenant; it is reserved for the platform module and named jobs.
func TestPlatformRoleSeesAllTenants(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	svc := tenancy.NewService(d.App)
	for _, s := range []string{"tenant-a", "tenant-b"} {
		if _, _, err := svc.CreateTenant(ctx, newTenant(s, "OUT1")); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := d.Platform.QueryRow(ctx, "SELECT count(*) FROM tenant").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("platform sees %d tenants, want 2", n)
	}
}

// The app role may not touch plan or billing fields, and may not delete anything.
func TestAppRoleGrants(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	tenant, outlet, err := tenancy.NewService(d.App).CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))
	if err != nil {
		t.Fatal(err)
	}
	denied := []string{
		`UPDATE tenant SET subscription_status = 'active'`,
		`UPDATE tenant SET paid_until = now()`,
		`UPDATE plan SET name = 'Free forever'`,
		`DELETE FROM outlet_settings`,
		`DELETE FROM outlet`,
		`DELETE FROM tenant`,
		`UPDATE change_log SET op = 'delete'`,
		`DELETE FROM change_log`,
	}
	for _, sql := range denied {
		err := kernel.TenantTx(ctx, d.App, tenant.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
		if !isPgCode(err, "42501") {
			t.Errorf("%s: err = %v, want insufficient_privilege (42501)", sql, err)
		}
	}

	// Allowed: renaming the tenant and editing an outlet.
	err = kernel.TenantTx(ctx, d.App, tenant.ID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE tenant SET name = 'Kopi Senja Baru'`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE outlet_settings SET tax_rate_bp = 1000 WHERE outlet_id = $1`, outlet.ID)
		return err
	})
	if err != nil {
		t.Errorf("allowed updates failed: %v", err)
	}
}

// change_log is append-only even for the schema owner, who bypasses grants.
func TestChangeLogAppendOnlyForOwner(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, _, err := tenancy.NewService(d.App).CreateTenant(ctx, newTenant("kopi-senja", "BDG1")); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`UPDATE change_log SET op = 'delete'`, `DELETE FROM change_log`} {
		if _, err := d.Owner.Exec(ctx, sql); !isPgCode(err, "42501") {
			t.Errorf("%s as owner: err = %v, want 42501 from the trigger", sql, err)
		}
	}
}

// Concurrent writers to one tenant queue on the tenant row (kernel.LockTenant, then
// record_change) and get gapless change numbers, with no deadlock.
func TestConcurrentChangesAreGaplessWithoutDeadlock(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	tenant, outlet, err := tenancy.NewService(d.App).CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))
	if err != nil {
		t.Fatal(err)
	}

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() {
			errs <- kernel.TenantTx(ctx, d.App, tenant.ID, func(tx pgx.Tx) error {
				if err := kernel.LockTenant(ctx, tx); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE outlet_settings SET receipt_footer = 'Terima kasih' WHERE outlet_id = $1`, outlet.ID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `SELECT record_change('outlet_settings', $1, 'upsert', $1)`, outlet.ID)
				return err
			})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	var count, maxSeq int64
	if err := d.Owner.QueryRow(ctx,
		`SELECT count(*), max(seq) FROM change_log WHERE tenant_id = $1`, tenant.ID).Scan(&count, &maxSeq); err != nil {
		t.Fatal(err)
	}
	if count != 2+writers || maxSeq != count {
		t.Errorf("count = %d, max seq = %d, want both %d", count, maxSeq, 2+writers)
	}
}

// The service refuses to run as a role that would bypass row-level security.
func TestCheckAppRole(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if err := database.CheckAppRole(ctx, d.App); err != nil {
		t.Errorf("app role: %v", err)
	}
	if err := database.CheckAppRole(ctx, d.Owner); err == nil {
		t.Error("superuser owner: want error")
	}
	if err := database.CheckAppRole(ctx, d.Platform); err == nil {
		t.Error("platform role is not an orion_app member: want error")
	}
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func ptrTo[T any](v T) *T { return &v }

func TestUpdateOutletSettings(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()
	tenant, outlet, err := svc.CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))
	if err != nil {
		t.Fatal(err)
	}
	var seqBefore int64
	_ = d.Owner.QueryRow(ctx, `SELECT change_seq FROM tenant WHERE id = $1`, tenant.ID).Scan(&seqBefore)

	cutoff := 4 * time.Hour
	got, err := svc.UpdateOutletSettings(ctx, tenant.ID, outlet.ID, tenancy.SettingsUpdate{
		Timezone: ptrTo("Asia/Makassar"), BusinessDayCutoff: &cutoff, PriceIncludesTax: ptrTo(true),
		TaxRate: ptrTo(kernel.BasisPoints(1100)), ServiceChargeRate: ptrTo(kernel.BasisPoints(500)),
		ServiceChargeTaxable: ptrTo(false), CashRoundingUnit: ptrTo(kernel.Rupiah(500)), CashRoundingMode: ptrTo("down"),
		ReceiptHeader: ptrTo("Kopi Senja\nJl. Braga 1"), ReceiptFooter: ptrTo("Terima kasih"),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := got.Settings
	if s.Timezone != "Asia/Makassar" || s.BusinessDayCutoff != cutoff || !s.PriceIncludesTax || s.TaxRate != 1100 ||
		s.ServiceChargeRate != 500 || s.ServiceChargeTaxable || s.CashRoundingUnit != 500 || s.CashRoundingMode != "down" ||
		s.ReceiptHeader != "Kopi Senja\nJl. Braga 1" || s.ReceiptFooter != "Terima kasih" {
		t.Errorf("settings = %+v", s)
	}

	// Recorded for this outlet only, and audited with the old and new values.
	var entity string
	var outletID uuid.UUID
	if err := d.Owner.QueryRow(ctx, `SELECT entity_type, outlet_id FROM change_log WHERE tenant_id = $1 AND seq = $2`, tenant.ID, seqBefore+1).Scan(&entity, &outletID); err != nil {
		t.Fatal(err)
	}
	if entity != "outlet_settings" || outletID != outlet.ID {
		t.Errorf("change = %s for %s", entity, outletID)
	}
	var detail string
	if err := d.Owner.QueryRow(ctx, `SELECT detail::text FROM tenant_audit_log WHERE action = 'settings.updated'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"timezone": {"after": "Asia/Makassar", "before": "Asia/Jakarta"}`, `"business_day_cutoff": {"after": "04:00", "before": "00:00"}`, `"tax_rate_bp": {"after": 1100, "before": 0}`} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail lacks %s:\n%s", want, detail)
		}
	}

	// Fields left out stay; an update that changes nothing writes nothing.
	again, err := svc.UpdateOutletSettings(ctx, tenant.ID, outlet.ID, tenancy.SettingsUpdate{TaxRate: ptrTo(kernel.BasisPoints(1100))})
	if err != nil || again.Settings != got.Settings {
		t.Errorf("no-op update: %+v, %v", again.Settings, err)
	}
	var seqAfter int64
	_ = d.Owner.QueryRow(ctx, `SELECT change_seq FROM tenant WHERE id = $1`, tenant.ID).Scan(&seqAfter)
	if seqAfter != seqBefore+1 {
		t.Errorf("change_seq moved from %d to %d; only the real change should count", seqBefore, seqAfter)
	}

	// Reading it back shows the same, including through the list.
	list, err := svc.ListOutlets(ctx, tenant.ID)
	if err != nil || list[0].Settings != got.Settings {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestUpdateOutletSettingsValidation(t *testing.T) {
	d := testdb.New(t)
	svc := tenancy.NewService(d.App)
	ctx := context.Background()
	tenant, outlet, _ := svc.CreateTenant(ctx, newTenant("kopi-senja", "BDG1"))

	hours := func(h int) *time.Duration { v := time.Duration(h) * time.Hour; return &v }
	odd := 90*time.Minute + time.Second
	bad := map[string]tenancy.SettingsUpdate{
		"unknown timezone":   {Timezone: ptrTo("Europe/Paris")},
		"cutoff at noon":     {BusinessDayCutoff: hours(12)},
		"negative cutoff":    {BusinessDayCutoff: hours(-1)},
		"cutoff in seconds":  {BusinessDayCutoff: &odd},
		"tax over 100%":      {TaxRate: ptrTo(kernel.BasisPoints(10001))},
		"negative tax":       {TaxRate: ptrTo(kernel.BasisPoints(-1))},
		"service over 100%":  {ServiceChargeRate: ptrTo(kernel.BasisPoints(10001))},
		"huge rounding unit": {CashRoundingUnit: ptrTo(kernel.Rupiah(1_000_000))},
		"unknown mode":       {CashRoundingMode: ptrTo("sideways")},
		"endless header":     {ReceiptHeader: ptrTo(strings.Repeat("x", 501))},
		"endless footer":     {ReceiptFooter: ptrTo(strings.Repeat("é", 501))},
	}
	for name, in := range bad {
		if _, err := svc.UpdateOutletSettings(ctx, tenant.ID, outlet.ID, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	if _, err := svc.UpdateOutletSettings(ctx, tenant.ID, kernel.NewID(), tenancy.SettingsUpdate{}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown outlet: err = %v", err)
	}
	// Another business's outlet is not found either.
	other, _, _ := svc.CreateTenant(ctx, newTenant("teh-manis", "SBY1"))
	if _, err := svc.UpdateOutletSettings(ctx, other.ID, outlet.ID, tenancy.SettingsUpdate{TaxRate: ptrTo(kernel.BasisPoints(1))}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("another business's outlet: err = %v", err)
	}
	if got, _ := svc.GetOutlet(ctx, tenant.ID, outlet.ID); got.Settings.TaxRate != 0 {
		t.Errorf("a refused update changed the outlet: %+v", got.Settings)
	}
}

func TestSettingsGiveTheBusinessDate(t *testing.T) {
	s := tenancy.OutletSettings{Timezone: "Asia/Jakarta", BusinessDayCutoff: 4 * time.Hour}
	// 02:30 on 3 October in Jakarta is 19:30 UTC on 2 October; with a 04:00 cutoff the business
	// day is the 2nd either way, and 05:00 is the 3rd.
	if got := s.BusinessDate(time.Date(2026, 10, 2, 19, 30, 0, 0, time.UTC)); got.Format(time.DateOnly) != "2026-10-02" {
		t.Errorf("02:30 local = %s", got.Format(time.DateOnly))
	}
	if got := s.BusinessDate(time.Date(2026, 10, 2, 22, 0, 0, 0, time.UTC)); got.Format(time.DateOnly) != "2026-10-03" {
		t.Errorf("05:00 local = %s", got.Format(time.DateOnly))
	}
	s.Timezone = "Asia/Jayapura" // two hours ahead of Jakarta: the same instants fall on later local hours
	if got := s.BusinessDate(time.Date(2026, 10, 2, 19, 30, 0, 0, time.UTC)); got.Format(time.DateOnly) != "2026-10-03" {
		t.Errorf("04:30 local in WIT = %s", got.Format(time.DateOnly))
	}
}
