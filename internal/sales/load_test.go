package sales_test

// The load sanity check (BACKEND_PLAN.md task B1.11): one week of a busy cafe, 600 sales a day on
// three tablets, pushed in bursts the way tablets reconnect, with the p95 latency of a push held
// under 300 ms, and the reports and lists still reading through indexes afterwards.
//
// It takes tens of seconds, so it runs unless -short is given. `make load` runs it alone and prints
// the numbers.

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/reporting"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
)

const (
	loadDays          = 7
	loadSalesPerDay   = 600
	loadDevices       = 3
	loadP95           = 300 * time.Millisecond
	loadMaxBurst      = 25
	loadMinSalesTotal = loadDays * loadSalesPerDay
)

func TestAWeekOfABusyCafeStaysFast(t *testing.T) {
	if testing.Short() {
		t.Skip("load check skipped in -short mode")
	}
	f := newFx(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(99))
	devices := []identity.Principal{f.dev}
	for i := 1; i < loadDevices; i++ {
		devices = append(devices, f.device(fmt.Sprint("Kasir ", i+1)))
	}
	manager := f.manager
	menu := []lineSpec{f.espressoLine(1), f.latte(1), {Variant: f.latteIced, Name: "Latte (Iced)", Price: 30000, Qty: 1}}
	monday := time.Date(2026, 9, 7, 0, 0, 0, 0, time.FixedZone("WIB", 7*3600))

	// Build the whole week's events first: the builder is not safe for concurrent use, and building
	// is not what is being measured.
	type stream struct {
		dev    identity.Principal
		events []syncsrv.Event
	}
	streams := make([]*stream, loadDevices)
	for i := range streams {
		streams[i] = &stream{dev: devices[i]}
	}
	for day := 0; day < loadDays; day++ {
		start := monday.AddDate(0, 0, day)
		for di, st := range streams {
			open := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 100000}, start.Add(7*time.Hour+time.Duration(di)*time.Minute))
			st.events = append(st.events, open)
			n := loadSalesPerDay / loadDevices
			for i := 0; i < n; i++ {
				spec := saleSpec{Shift: open.ID, Dev: st.dev, CatalogSeq: f.seq()}
				for k := 1 + rng.Intn(3); k > 0; k-- {
					l := menu[rng.Intn(len(menu))]
					l.Qty = int64(1 + rng.Intn(3))
					spec.Lines = append(spec.Lines, l)
				}
				if rng.Intn(6) == 0 {
					spec.Discounts = []discountSpec{{Kind: pricing.DiscountAmount, Value: 1000, ApprovedBy: &manager}}
				}
				if rng.Intn(3) == 0 {
					spec.Tender = pricing.TenderNonCash
				}
				at := start.Add(8*time.Hour + time.Duration(i)*(14*time.Hour/time.Duration(n)))
				st.events = append(st.events, f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(spec), at))
			}
			st.events = append(st.events, f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": open.ID, "counted_cash": 1}, start.Add(23*time.Hour)))
		}
	}

	// Each tablet pushes its own week in bursts of 1 to 25 events, all three at once.
	var mu sync.Mutex
	var lat []time.Duration
	var burstSizes []int
	var wg sync.WaitGroup
	began := time.Now()
	for _, st := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(len(st.events))))
			rest := st.events
			for len(rest) > 0 {
				n := 1 + r.Intn(loadMaxBurst)
				if n > len(rest) {
					n = len(rest)
				}
				t0 := time.Now()
				res, err := f.svc.Push(ctx, st.dev, syncsrv.PushRequest{Events: rest[:n]})
				took := time.Since(t0)
				if err != nil {
					t.Error(err)
					return
				}
				for _, x := range res.Results {
					if x.Status != syncsrv.StatusAccepted || x.Code != "" {
						t.Errorf("event not accepted: %+v", x)
						return
					}
				}
				mu.Lock()
				lat, burstSizes = append(lat, took), append(burstSizes, n)
				mu.Unlock()
				rest = rest[n:]
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(began)
	if t.Failed() {
		return
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration { return lat[int(float64(len(lat)-1)*p)] }
	var events int
	for _, n := range burstSizes {
		events += n
	}
	t.Logf("%d events in %d pushes over %v (%.0f events/s): p50 %v, p95 %v, p99 %v, max %v",
		events, len(lat), elapsed.Round(time.Millisecond), float64(events)/elapsed.Seconds(),
		pct(0.50).Round(time.Millisecond), pct(0.95).Round(time.Millisecond), pct(0.99).Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond))
	if p95 := pct(0.95); p95 > loadP95 {
		if raceEnabled {
			// The race detector multiplies run time, so the bar means nothing here. Everything below
			// still runs; the bar itself is enforced by the CI job that runs this test without
			// -race (make load).
			t.Logf("p95 push latency %v is over %v; not enforced under the race detector", p95, loadP95)
		} else {
			t.Errorf("p95 push latency %v is over %v", p95, loadP95)
		}
	}

	// Nothing lost, nothing duplicated, nothing flagged.
	eq(t, "sales stored", f.count("sale"), loadMinSalesTotal)
	eq(t, "flags", f.count("flag"), 0)
	var received, inbox int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM sync_inbox WHERE status = 'accepted'`).Scan(&inbox)
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM sync_inbox WHERE status <> 'accepted'`).Scan(&received)
	eq(t, "settled events", received, 0)
	eq(t, "accepted events", inbox, loadMinSalesTotal+2*loadDays*loadDevices)

	// The reports and lists after a week of data.
	rep := reporting.NewService(f.d.App)
	rd := sales.NewService(f.d.App)
	timeIt := func(name string, fn func() error) {
		t0 := time.Now()
		if err := fn(); err != nil {
			t.Fatal(name, err)
		}
		took := time.Since(t0)
		t.Logf("%s: %v", name, took.Round(time.Millisecond))
		if took > 500*time.Millisecond {
			t.Errorf("%s took %v", name, took)
		}
	}
	timeIt("end-of-day report", func() error {
		r, err := rep.DayReport(ctx, f.tenant.ID, f.outlet.ID, monday.AddDate(0, 0, 3))
		if err == nil && r.Sales.Count != int64(loadSalesPerDay) {
			err = fmt.Errorf("%d sales that day", r.Sales.Count)
		}
		return err
	})
	timeIt("sales list, first page", func() error {
		_, err := rd.ListSales(ctx, f.tenant.ID, sales.SaleFilter{}, nil, 50)
		return err
	})
	timeIt("sales list, one day", func() error {
		d := monday.AddDate(0, 0, 2)
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		_, err := rd.ListSales(ctx, f.tenant.ID, sales.SaleFilter{From: &day, To: &day}, nil, 50)
		return err
	})
	var oneShift uuid.UUID
	_ = f.d.Owner.QueryRow(ctx, `SELECT id FROM shift ORDER BY opened_at LIMIT 1`).Scan(&oneShift)
	timeIt("end-of-shift report", func() error {
		_, err := rep.ShiftReport(ctx, f.tenant.ID, oneShift)
		return err
	})
}

// Each way the reports, the sales list and the monitor read the sales tables has an index built for
// it. PostgreSQL does not index the referencing side of a foreign key, so without these a report
// would scan whole tables once there is a week of data. (The planner rightly prefers a scan on a
// small table, so this checks the indexes exist, not what a plan picks on a toy database; the load
// check above times the reports on a week of data.)
func TestTheReportsHaveTheIndexesTheyReadThrough(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	want := []struct{ table, columns, why string }{
		{"sale", "(tenant_id, outlet_id, business_date)", "the end-of-day report and the sales list"},
		{"sale", "(tenant_id, shift_id)", "the end-of-shift report"},
		{"payment", "(tenant_id, sale_id)", "the payments of a sale or of a day's sales"},
		{"sale_discount", "(tenant_id, sale_id)", "the discounts of a sale or of a day"},
		{"sale_line", "(tenant_id, sale_id, line_no)", "the lines of a sale"},
		{"void", "(tenant_id, shift_id)", "the voids made in a shift"},
		{"void", "(tenant_id, sale_id)", "the void of a sale"},
		{"cash_movement", "(tenant_id, shift_id)", "the cash movements of a shift"},
		{"cash_movement", "(tenant_id, outlet_id, business_date)", "the cash movements of a day"},
		{"shift", "(tenant_id, outlet_id, business_date)", "the shifts of a day"},
		{"shift", "WHERE (closed_at IS NULL)", "the monitor's open shifts"},
		{"flag", "(tenant_id, target_id)", "the flags of a sale"},
		{"sync_inbox", "(tenant_id, depends_on)", "releasing parked events"},
	}
	for _, w := range want {
		var n int
		if err := f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1 AND indexdef LIKE '%' || $2 || '%'`, w.table, w.columns).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Errorf("%s has no index on %s, which %s reads through", w.table, w.columns, w.why)
		}
	}
}
