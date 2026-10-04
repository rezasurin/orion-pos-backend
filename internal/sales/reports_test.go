package sales_test

// The end-of-day reconciliation (BACKEND_PLAN.md section 5.3, the Phase 1 exit test in miniature): a
// simulated week of a busy cafe is pushed through the real sync path, and the server's end-of-shift
// and end-of-day reports must equal, to the rupiah, what the test's own bookkeeping says the
// receipts add up to. The bookkeeping below is the "client": it never reads the database.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/reporting"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

var wib = time.FixedZone("WIB", 7*3600)

// The client's own record of what happened.
type bkShift struct {
	id      uuid.UUID
	day     int // business day index
	opening int64
	payIn   int64
	payOut  int64
	noSale  int64
	refunds int64 // cash refunded for voids made in this shift
	counted int64
}

type bkSale struct {
	id        uuid.UUID
	day       int
	shift     *bkShift
	res       pricing.Result
	cash      int64            // cash applied to the bill
	methods   map[string]int64 // amount per payment method
	discounts int64            // number of discounts
	voided    bool
}

func TestAWeekOfSalesReconcilesToTheRupiah(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	cutoff := 3 * time.Hour
	if _, err := f.ten.UpdateOutletSettings(ctx, f.tenant.ID, f.outlet.ID, tenancy.SettingsUpdate{BusinessDayCutoff: &cutoff}); err != nil {
		t.Fatal(err)
	}
	rep := reporting.NewService(f.d.App)
	rng := rand.New(rand.NewSource(2026))
	manager := f.manager
	devices := []identity.Principal{f.dev, f.device("Kasir 2")}

	monday := time.Date(2026, 9, 14, 0, 0, 0, 0, wib) // far enough in the past that no device clock is "ahead"
	var shifts []*bkShift
	var all []*bkSale
	byDay := make([][]*bkSale, 7)

	voidsMade, crossDayVoids := 0, 0
	menu := []lineSpec{f.espressoLine(1), f.latte(1), {Variant: f.latteIced, Name: "Latte (Iced)", Price: 30000, Qty: 1}}
	randomSale := func(dev identity.Principal, shift *bkShift, day int, at time.Time) (syncsrv.Event, *bkSale) {
		spec := saleSpec{Shift: shift.id, Dev: dev, CatalogSeq: f.seq()}
		for n := 1 + rng.Intn(4); n > 0; n-- {
			l := menu[rng.Intn(len(menu))]
			l.Qty = int64(1 + rng.Intn(3))
			spec.Lines = append(spec.Lines, l)
		}
		switch rng.Intn(5) {
		case 0:
			line := rng.Intn(len(spec.Lines))
			spec.Discounts = append(spec.Discounts, discountSpec{Line: &line, Kind: pricing.DiscountPercent, Value: int64(500 * (1 + rng.Intn(4))), ApprovedBy: &manager})
		case 1:
			spec.Discounts = append(spec.Discounts, discountSpec{Kind: pricing.DiscountAmount, Value: int64(1000 * (1 + rng.Intn(5))), ApprovedBy: &manager})
		}
		kind := rng.Intn(4) // 0 and 1 cash, 2 QRIS, 3 split between cash and card
		switch kind {
		case 2:
			spec.Tender = pricing.TenderNonCash
		case 3:
			spec.Tender = pricing.TenderMixed
		}
		payload := f.salePayload(spec)
		sale := &bkSale{day: day, shift: shift, res: f.lastRes, methods: map[string]int64{}, discounts: int64(len(spec.Discounts))}
		due := int64(sale.res.CashTotal)
		switch kind {
		case 0, 1:
			sale.cash, sale.methods["cash"] = due, due
		case 2:
			sale.methods["qris_manual"] = due
		case 3:
			cash := (due / 2 / 100) * 100
			sale.cash, sale.methods["cash"], sale.methods["card_manual"] = cash, cash, due-cash
			payload["payments"] = []map[string]any{
				{"method": "cash", "amount": cash, "tendered": cash, "change": int64(0)},
				{"method": "card_manual", "amount": due - cash, "reference": "EDC-1"},
			}
		}
		ev := f.event(f.cashier, sales.TypeSaleCompleted, payload, at)
		sale.id = ev.ID
		return ev, sale
	}

	for day := 0; day < 7; day++ {
		start := monday.AddDate(0, 0, day)
		dayShifts := make([]*bkShift, len(devices))

		for di, dev := range devices {
			s := &bkShift{day: day, opening: int64(100000 + 50000*di)}
			open := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": s.opening}, start.Add(7*time.Hour+30*time.Minute+time.Duration(di)*time.Minute))
			s.id = open.ID
			if r := f.one(dev, open); r.Status != syncsrv.StatusAccepted || r.Code != "" {
				t.Fatalf("open shift: %+v", r)
			}
			shifts, dayShifts[di] = append(shifts, s), s
		}

		for di, dev := range devices {
			shift := dayShifts[di]
			var batch []syncsrv.Event
			n := 12 + rng.Intn(10)
			for i := 0; i < n; i++ {
				at := start.Add(8*time.Hour + time.Duration(i)*35*time.Minute)
				if i == n-1 {
					at = start.Add(25*time.Hour + 15*time.Minute) // 01:15 the next calendar day: still this business day
				}
				ev, sale := randomSale(dev, shift, day, at)
				batch = append(batch, ev)
				all, byDay[day] = append(all, sale), append(byDay[day], sale)
			}
			for _, res := range f.push(dev, batch...).Results {
				if res.Status != syncsrv.StatusAccepted || res.Code != "" {
					t.Fatalf("day %d: %+v", day, res)
				}
			}

			// Cash comes in and goes out, and the drawer is opened.
			if rng.Intn(2) == 0 {
				amt := int64(10000 * (1 + rng.Intn(5)))
				f.one(dev, f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift.id, "kind": "pay_in", "amount": amt}, start.Add(12*time.Hour)))
				shift.payIn += amt
			}
			if rng.Intn(2) == 0 {
				amt := int64(5000 * (1 + rng.Intn(4)))
				f.one(dev, f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift.id, "kind": "pay_out", "amount": amt, "reason": "supplies"}, start.Add(13*time.Hour)))
				shift.payOut += amt
			}
			f.one(dev, f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift.id, "kind": "no_sale", "amount": 0}, start.Add(14*time.Hour)))
			shift.noSale++
		}

		// Voids, of today's sales and of earlier days': the refund leaves today's drawer.
		for di, dev := range devices {
			shift := dayShifts[di]
			candidates := append([]*bkSale{}, byDay[day]...)
			if day > 0 {
				candidates = append(candidates, byDay[day-1]...)
			}
			for k := 0; k < 2; k++ {
				target := candidates[rng.Intn(len(candidates))]
				if target.voided {
					continue
				}
				ev := f.event(f.manager, sales.TypeSaleVoided, map[string]any{
					"sale_id": target.id, "reason": "customer changed their mind", "approved_by": &manager, "shift_id": shift.id,
				}, start.Add(16*time.Hour))
				if res := f.one(dev, ev); res.Status != syncsrv.StatusAccepted || res.Code != "" {
					t.Fatalf("void: %+v", res)
				}
				target.voided = true
				shift.refunds += target.cash
				voidsMade++
				if target.day != day {
					crossDayVoids++
				}
			}
		}

		// Close both shifts. The cashier counts the drawer: what it should hold, give or take.
		for di, dev := range devices {
			shift := dayShifts[di]
			var received int64
			for _, s := range all {
				if s.shift == shift {
					received += s.cash
				}
			}
			expected := shift.opening + received - shift.refunds + shift.payIn - shift.payOut
			shift.counted = expected + []int64{0, 0, -5000, 2000}[rng.Intn(4)]
			ev := f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shift.id, "counted_cash": shift.counted}, start.Add(26*time.Hour))
			if res := f.one(dev, ev); res.Status != syncsrv.StatusAccepted || res.Code != "" {
				t.Fatalf("close: %+v", res)
			}
		}
	}

	// The week has to be big enough to mean something.
	if len(all) < 150 || voidsMade < 15 || crossDayVoids < 3 {
		t.Fatalf("a thin week: %d sales, %d voids (%d of an earlier day's sale)", len(all), voidsMade, crossDayVoids)
	}

	// A void made on a later day of an earlier day's sale refunds from the later drawer, so the
	// earlier shift's expected cash is final once it closed: the figures counted above are the ones
	// the reports must agree with now that the whole week is in.
	for day := 0; day < 7; day++ {
		date := monday.AddDate(0, 0, day)
		got, err := rep.DayReport(ctx, f.tenant.ID, f.outlet.ID, date)
		if err != nil {
			t.Fatal(err)
		}

		var want reporting.Totals
		var voided reporting.Voided
		var discounts reporting.DiscountTotals
		methods := map[string]int64{}
		for _, s := range all {
			if s.day != day {
				continue
			}
			if s.voided {
				voided.Count++
				voided.Total += int64(s.res.Total)
				continue
			}
			want.Count++
			want.Subtotal += int64(s.res.Subtotal)
			want.Discounts += int64(s.res.DiscountTotal)
			want.ServiceCharge += int64(s.res.ServiceCharge)
			want.Tax += int64(s.res.Tax)
			want.Total += int64(s.res.Total)
			want.Rounding += int64(s.res.RoundingAmount)
			for m, a := range s.methods {
				methods[m] += a
			}
			discounts.Count += s.discounts
			discounts.Amount += int64(s.res.DiscountTotal)
		}
		want.Net = want.Subtotal - want.Discounts

		if got.Sales != want {
			t.Errorf("day %d sales:\n got  %+v\n want %+v", day, got.Sales, want)
		}
		if got.VoidedSales != voided {
			t.Errorf("day %d voided: got %+v, want %+v", day, got.VoidedSales, voided)
		}
		if got.Discounts != discounts {
			t.Errorf("day %d discounts: got %+v, want %+v", day, got.Discounts, discounts)
		}
		gotMethods := map[string]int64{}
		var paid int64
		for _, m := range got.PaymentMethods {
			gotMethods[m.Method] = m.Amount
			paid += m.Amount
		}
		if fmt.Sprint(gotMethods) != fmt.Sprint(methods) {
			t.Errorf("day %d payment methods: got %v, want %v", day, gotMethods, methods)
		}
		// What the tills took is what the bills came to plus the cash rounding.
		if paid != want.Total+want.Rounding {
			t.Errorf("day %d: payments %d != total %d + rounding %d", day, paid, want.Total, want.Rounding)
		}
		if len(got.Flags) != 0 {
			t.Errorf("day %d: unexpected flags %v", day, got.Flags)
		}

		var expectedAll, countedAll, payIn, payOut, noSale int64
		if len(got.Shifts) != 2 || got.OpenShifts != 0 {
			t.Errorf("day %d: %d shifts, %d open", day, len(got.Shifts), got.OpenShifts)
		}
		for _, sh := range got.Shifts {
			var m *bkShift
			for _, b := range shifts {
				if b.id == sh.Shift.ID {
					m = b
				}
			}
			if m == nil || m.day != day {
				t.Fatalf("day %d: the report lists a shift that is not this day's: %s", day, sh.Shift.ID)
			}
			var received int64
			for _, s := range all {
				if s.shift == m {
					received += s.cash
				}
			}
			wantExpected := m.opening + received - m.refunds + m.payIn - m.payOut
			if sh.Cash.Expected != wantExpected || sh.Cash.Counted == nil || *sh.Cash.Counted != m.counted || *sh.Cash.Difference != m.counted-wantExpected {
				t.Errorf("day %d shift cash: got %+v, want expected %d counted %d", day, sh.Cash, wantExpected, m.counted)
			}
			expectedAll += wantExpected
			countedAll += m.counted
			payIn += m.payIn
			payOut += m.payOut
			noSale += m.noSale
		}
		if got.Cash.Expected != expectedAll || got.Cash.Counted == nil || *got.Cash.Counted != countedAll || *got.Cash.Difference != countedAll-expectedAll {
			t.Errorf("day %d cash: got %+v, want expected %d counted %d", day, got.Cash, expectedAll, countedAll)
		}
		if got.CashMovements.PayIn != payIn || got.CashMovements.PayOut != payOut || got.CashMovements.NoSale != noSale {
			t.Errorf("day %d cash movements: got %+v, want in %d out %d no-sale %d", day, got.CashMovements, payIn, payOut, noSale)
		}
	}

	// Each shift's own report agrees.
	for _, s := range shifts {
		got, err := rep.ShiftReport(ctx, f.tenant.ID, s.id)
		if err != nil {
			t.Fatal(err)
		}
		var count, total, voidedCount int64
		for _, sale := range all {
			if sale.shift != s {
				continue
			}
			if sale.voided {
				voidedCount++
				continue
			}
			count++
			total += int64(sale.res.Total)
		}
		if got.Sales.Count != count || got.Sales.Total != total || got.VoidedSales.Count != voidedCount {
			t.Errorf("shift report: %d sales totalling %d (%d voided), want %d, %d and %d", got.Sales.Count, got.Sales.Total, got.VoidedSales.Count, count, total, voidedCount)
		}
		if got.NoSaleOpenings != s.noSale {
			t.Errorf("no-sale openings %d, want %d", got.NoSaleOpenings, s.noSale)
		}
		if got.Cash.Counted == nil || *got.Cash.Counted != s.counted || got.Shift.ClosedAt == nil {
			t.Errorf("shift cash: %+v", got.Cash)
		}
	}
}

func TestReportsOfAnOpenShiftAndAnEmptyDay(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	rep := reporting.NewService(f.d.App)
	shift := f.openShift(f.dev, 80000)
	manager := f.manager
	_ = manager

	ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(2)}}), f.t0.Add(time.Minute))
	f.one(f.dev, ev)

	got, err := rep.ShiftReport(ctx, f.tenant.ID, shift)
	if err != nil {
		t.Fatal(err)
	}
	// A shift still open has its expected cash but nothing counted yet.
	eq(t, "open shift counted", got.Cash.Counted == nil && got.Cash.Difference == nil, true)
	eq(t, "opening", got.Cash.OpeningCash, int64(80000))
	eq(t, "cash received", got.Cash.Received, got.Sales.Total+got.Sales.Rounding)
	eq(t, "expected", got.Cash.Expected, 80000+got.Sales.Total+got.Sales.Rounding)
	eq(t, "sales", got.Sales.Count, int64(1))

	// The day it belongs to shows the open shift and no counted cash.
	date := f.t0.Add(time.Minute).In(wib)
	day, err := rep.DayReport(ctx, f.tenant.ID, f.outlet.ID, date)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "open shifts", day.OpenShifts, 1)
	eq(t, "day cash counted", day.Cash.Counted == nil, true)

	// A day with nothing in it is all zeros, not an error.
	empty, err := rep.DayReport(ctx, f.tenant.ID, f.outlet.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "empty day sales", empty.Sales.Count, int64(0))
	eq(t, "empty day shifts", len(empty.Shifts), 0)
	if _, err := rep.ShiftReport(ctx, f.tenant.ID, uuid.New()); err == nil {
		t.Error("a report for a shift that does not exist should fail")
	}
}
