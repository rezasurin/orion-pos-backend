package sales_test

import (
	"context"
	"testing"
	"time"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// Sales count on their business date, sync figures on the day they arrived: a sale rung up three days
// ago on a tablet that was offline counts three days ago, its event today.
func TestDailyMetricsCountSalesByBusinessDateAndEventsByArrival(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	plat := newPlatformFor(t, f)
	// Both businesses date from well before the week the job recomputes.
	_, _, err := f.ten.CreateTenant(ctx, tenancy.NewTenant{Name: "Sepi", Slug: "sepi", Outlet: tenancy.NewOutlet{Name: "Main", Code: "MAIN"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Owner.Exec(ctx, `UPDATE tenant SET created_at = now() - interval '30 days'`); err != nil {
		t.Fatal(err)
	}

	shift := f.openShift(f.dev, 0)
	today := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	voided := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(2)}}), f.t0.Add(2*time.Minute))
	f.push(f.dev, today, voided)
	f.one(f.dev, f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": voided.ID, "reason": "x", "approved_by": f.manager}, f.t0.Add(3*time.Minute)))
	f.one(f.dev, f.event(f.cashier, "pos.mystery", map[string]any{}, f.t0))

	old := time.Now().Add(-72 * time.Hour)
	oldShift := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, old)
	late := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: oldShift.ID, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), old.Add(time.Minute))
	f.push(f.dev, oldShift, late)

	jobs := &platform.MetricsJobs{Platform: f.d.Platform, Clock: kernel.SystemClock{}}
	first, err := jobs.Compute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := jobs.Compute(ctx); err != nil || again != first || first != 14 {
		t.Fatalf("computed %d rows, then %d (%v); want 7 days for each of 2 businesses", first, again, err)
	}

	var lateDay, todayDay time.Time
	if err := f.d.Owner.QueryRow(ctx, `SELECT business_date FROM sale WHERE id = $1`, late.ID).Scan(&lateDay); err != nil {
		t.Fatal(err)
	}
	if err := f.d.Owner.QueryRow(ctx, `SELECT business_date FROM sale WHERE id = $1`, today.ID).Scan(&todayDay); err != nil {
		t.Fatal(err)
	}
	var events, flags int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM sync_inbox`).Scan(&events)
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM flag`).Scan(&flags)

	ms, err := plat.TenantMetrics(ctx, "kopi", 7)
	if err != nil {
		t.Fatal(err)
	}
	arrival := time.Now().In(platform.MetricsZone).Format(time.DateOnly)
	var sum platform.DailyMetrics
	for _, m := range ms {
		day := m.Day.Format(time.DateOnly)
		switch day {
		case lateDay.Format(time.DateOnly):
			if m.Sales != 1 || m.VoidedSales != 0 || m.Events != 0 {
				t.Errorf("the late sale's day %s: %+v", day, m)
			}
		case todayDay.Format(time.DateOnly):
			if m.Sales != 2 || m.VoidedSales != 1 {
				t.Errorf("today's sales on %s: %+v", day, m)
			}
		}
		if day == arrival && (m.Events != events || m.RejectedEvents != 1 || m.DevicesSynced != 1 || m.Flags != flags) {
			t.Errorf("arrivals on %s: %+v, want %d events, 1 rejected, 1 tablet, %d flags", day, m, events, flags)
		}
		sum.Sales += m.Sales
		sum.Events += m.Events
	}
	if len(ms) != 7 || sum.Sales != 3 || sum.Events != events {
		t.Errorf("%d days, %d sales, %d events; want 7, 3 and %d", len(ms), sum.Sales, sum.Events, events)
	}

	quiet, err := plat.TenantMetrics(ctx, "sepi", 7)
	if err != nil || len(quiet) != 7 || quiet[6].Sales != 0 || quiet[6].Events != 0 {
		t.Errorf("an idle business: %+v (%v)", quiet, err)
	}

	// The app role cannot read the table at all, not even its own business's rows.
	if _, err := f.d.App.Exec(ctx, `SELECT * FROM tenant_daily_metrics`); err == nil {
		t.Error("orion_app can read tenant_daily_metrics")
	}
}

func TestStoppedSyncingListsTabletsThatWentQuiet(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	plat := newPlatformFor(t, f)
	f.openShift(f.dev, 0) // Kasir 1 syncs now

	quiet := f.device("Kasir 2")
	never := f.device("Kasir 3")
	gone := f.device("Kasir 4")
	ancient := f.device("Kasir 5")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.d.Owner.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE device SET last_sync_at = now() - interval '2 days', last_seen_at = now() - interval '1 hour' WHERE id = $1`, quiet.DeviceID)
	exec(`UPDATE device SET paired_at = now() - interval '3 days' WHERE id = $1`, never.DeviceID)
	exec(`UPDATE device SET last_sync_at = now() - interval '2 days', revoked_at = now(), revoked_by = paired_by WHERE id = $1`, gone.DeviceID)
	exec(`UPDATE device SET last_sync_at = now() - interval '40 days' WHERE id = $1`, ancient.DeviceID)

	list, err := plat.StoppedSyncing(ctx, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range list {
		names = append(names, d.Name)
	}
	// Never synced counts from its pairing, three days ago: the longest silent comes first.
	if len(list) != 2 || list[0].Name != "Kasir 3" || list[0].LastSyncAt != nil || list[1].Name != "Kasir 2" ||
		list[1].LastSeenAt == nil || list[1].TenantSlug != "kopi" || list[1].OutletCode != "JKT1" {
		t.Fatalf("stopped syncing = %v", names)
	}
	// A shorter quiet period finds nothing more: Kasir 1 synced a moment ago.
	if l, _ := plat.StoppedSyncing(ctx, time.Minute); len(l) != 2 {
		t.Errorf("quiet a minute: %d tablets", len(l))
	}
	exec(`UPDATE tenant SET suspended_at = now()`)
	if l, _ := plat.StoppedSyncing(ctx, 24*time.Hour); len(l) != 0 {
		t.Errorf("a suspended business is listed: %d tablets", len(l))
	}
	if _, err := plat.StoppedSyncing(ctx, 0); err == nil {
		t.Error("a zero quiet period is accepted")
	}
}
