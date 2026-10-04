package sales_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// ring pushes a sale of n espressos at the given time and returns its id.
func (f *fx) ring(shift uuid.UUID, n int64, at time.Time) uuid.UUID {
	f.t.Helper()
	ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(n)}}), at)
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted {
		f.t.Fatalf("ring: %+v", r)
	}
	return ev.ID
}

func TestSalesListIsNewestFirstAndPaged(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	rd := sales.NewService(f.d.App)
	open := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, time.Now().UTC().Add(-100*time.Hour))
	f.one(f.dev, open)
	shift := open.ID
	day1 := time.Now().UTC().Add(-48 * time.Hour)
	day2 := time.Now().UTC().Add(-24 * time.Hour)

	var oldest, newest []uuid.UUID
	for i := 0; i < 5; i++ {
		oldest = append(oldest, f.ring(shift, int64(i+1), day1.Add(time.Duration(i)*time.Minute)))
	}
	for i := 0; i < 4; i++ {
		newest = append(newest, f.ring(shift, int64(i+1), day2.Add(time.Duration(i)*time.Minute)))
	}

	// Newest business day first, newest sale first within a day, whatever the page size.
	want := []uuid.UUID{}
	for i := len(newest) - 1; i >= 0; i-- {
		want = append(want, newest[i])
	}
	for i := len(oldest) - 1; i >= 0; i-- {
		want = append(want, oldest[i])
	}
	for _, size := range []int{1, 2, 4, 9, 50} {
		var got []uuid.UUID
		var cursor *sales.SaleCursor
		for pages := 0; ; pages++ {
			page, err := rd.ListSales(ctx, f.tenant.ID, sales.SaleFilter{}, cursor, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > size {
				t.Fatalf("page of %d for a limit of %d", len(page.Items), size)
			}
			for _, it := range page.Items {
				got = append(got, it.ID)
			}
			if page.Next == nil {
				break
			}
			// The cursor survives its string form.
			c, err := sales.ParseSaleCursor(page.Next.Encode())
			if err != nil || c.ID != page.Next.ID || !c.At.Equal(page.Next.At) || !c.Date.Equal(page.Next.Date) {
				t.Fatalf("cursor round trip: %+v %v", c, err)
			}
			cursor = &c
			if pages > 20 {
				t.Fatal("never finished")
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("page size %d:\n got  %v\n want %v", size, got, want)
		}
	}
}

func TestSalesListFilters(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	rd := sales.NewService(f.d.App)
	// A shift that opened before any of the sales, so the only flags are the ones the test makes.
	open := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, time.Now().UTC().Add(-100*time.Hour))
	f.one(f.dev, open)
	shift := open.ID
	wibNow := time.Now().UTC()
	d1, d2, d3 := wibNow.Add(-72*time.Hour), wibNow.Add(-48*time.Hour), wibNow.Add(-24*time.Hour)
	a, b, c := f.ring(shift, 1, d1), f.ring(shift, 2, d2), f.ring(shift, 3, d3)

	// Void one, and flag another (a kitchen account rings it up).
	manager := f.manager
	f.one(f.dev, f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": b, "reason": "oops", "approved_by": &manager}, d3))
	flagged := f.event(f.kitchen, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), d3.Add(time.Minute))
	f.one(f.dev, flagged)

	ids := func(f2 sales.SaleFilter) []uuid.UUID {
		t.Helper()
		page, err := rd.ListSales(ctx, f.tenant.ID, f2, nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []uuid.UUID
		for _, it := range page.Items {
			out = append(out, it.ID)
		}
		return out
	}
	has := func(got []uuid.UUID, want ...uuid.UUID) bool {
		if len(got) != len(want) {
			return false
		}
		for _, w := range want {
			found := false
			for _, g := range got {
				found = found || g == w
			}
			if !found {
				return false
			}
		}
		return true
	}

	if got := ids(sales.SaleFilter{Status: "voided"}); !has(got, b) {
		t.Errorf("voided: %v", got)
	}
	if got := ids(sales.SaleFilter{Status: "completed"}); !has(got, a, c, flagged.ID) {
		t.Errorf("completed: %v", got)
	}
	if got := ids(sales.SaleFilter{FlaggedOnly: true}); !has(got, flagged.ID) {
		t.Errorf("flagged: %v", got)
	}
	// A void by a manager without an approver would be flagged: the flag belongs to the sale it voids.
	f.one(f.dev, f.event(f.cashier, sales.TypeSaleVoided, map[string]any{"sale_id": a, "reason": "cashier voided"}, d3))
	if got := ids(sales.SaleFilter{FlaggedOnly: true}); !has(got, flagged.ID, a) {
		t.Errorf("flagged after a flagged void: %v", got)
	}
	if got := ids(sales.SaleFilter{StaffID: &f.kitchen}); !has(got, flagged.ID) {
		t.Errorf("by staff: %v", got)
	}
	var number string
	if err := f.d.Owner.QueryRow(ctx, `SELECT receipt_number FROM sale WHERE id = $1`, c).Scan(&number); err != nil {
		t.Fatal(err)
	}
	if got := ids(sales.SaleFilter{ReceiptNumber: number}); !has(got, c) {
		t.Errorf("by receipt number: %v", got)
	}
	if got := ids(sales.SaleFilter{ReceiptNumber: "JKT1-01-999999"}); len(got) != 0 {
		t.Errorf("unknown receipt number: %v", got)
	}

	// Business-date range, inclusive at both ends.
	day := func(t time.Time) *time.Time {
		d := t.In(time.FixedZone("WIB", 7*3600))
		v := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		return &v
	}
	if got := ids(sales.SaleFilter{From: day(d2), To: day(d2)}); !has(got, b) {
		t.Errorf("one day: %v", got)
	}
	if got := ids(sales.SaleFilter{From: day(d1), To: day(d2)}); !has(got, a, b) {
		t.Errorf("two days: %v", got)
	}
	if got := ids(sales.SaleFilter{From: day(d3)}); !has(got, c, flagged.ID) {
		t.Errorf("from: %v", got)
	}

	// Outlet filters: no outlets means nothing, another outlet means nothing here.
	if got := ids(sales.SaleFilter{OutletIDs: []uuid.UUID{}}); len(got) != 0 {
		t.Errorf("no outlets: %v", got)
	}
	if got := ids(sales.SaleFilter{OutletIDs: []uuid.UUID{kernel.NewID()}}); len(got) != 0 {
		t.Errorf("another outlet: %v", got)
	}
	if got := ids(sales.SaleFilter{OutletIDs: []uuid.UUID{f.outlet.ID}}); len(got) != 4 {
		t.Errorf("this outlet: %v", got)
	}
	if _, err := rd.ListSales(ctx, f.tenant.ID, sales.SaleFilter{Status: "bogus"}, nil, 10); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("a bad status: %v", err)
	}
	if _, err := sales.ParseSaleCursor("nonsense"); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("a bad cursor: %v", err)
	}
}

func TestSaleDetail(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	rd := sales.NewService(f.d.App)
	shift := f.openShift(f.dev, 0)
	manager := f.manager
	line0 := 0
	payload := f.salePayload(saleSpec{
		Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(2), f.espressoLine(1)},
		Discounts: []discountSpec{
			{Line: &line0, Kind: pricing.DiscountPercent, Value: 1000, Reason: "regular", ApprovedBy: &manager},
			{Kind: pricing.DiscountAmount, Value: 1000, Reason: "birthday", ApprovedBy: &manager},
		},
	})
	sale := f.event(f.kitchen, sales.TypeSaleCompleted, payload, f.t0.Add(time.Minute)) // flagged: a kitchen account sold it
	f.one(f.dev, sale)
	f.one(f.dev, f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": sale.ID, "reason": "wrong table", "approved_by": &manager}, f.t0.Add(2*time.Minute)))

	d, err := rd.GetSale(ctx, f.tenant.ID, sale.ID)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "status", d.Status, "voided")
	eq(t, "lines", len(d.Lines), 2)
	eq(t, "line 0 name", d.Lines[0].Name, "Latte (Hot)")
	eq(t, "line 0 modifiers", len(d.Lines[0].Modifiers), 1)
	eq(t, "modifier", d.Lines[0].Modifiers[0].Name, "Oat milk")
	eq(t, "line 1 has no modifiers, not null", d.Lines[1].Modifiers != nil && len(d.Lines[1].Modifiers) == 0, true)
	eq(t, "discounts", len(d.Discounts), 2)
	if d.Discounts[0].LineNo == nil || *d.Discounts[0].LineNo != 0 || d.Discounts[1].LineNo != nil {
		t.Errorf("discount lines: %+v", d.Discounts)
	}
	eq(t, "payments", len(d.Payments), 1)
	if d.Void == nil || d.Void.Reason != "wrong table" || d.Void.ApprovedBy == nil {
		t.Errorf("void: %+v", d.Void)
	}
	eq(t, "pricing version", d.PricingVersion, 1)
	if !strings.Contains(string(d.Pricing), `"tax_rate_bp": 1100`) {
		t.Errorf("pricing snapshot: %s", d.Pricing)
	}
	// The kitchen account's sale is flagged; the flag detail names the permission.
	if len(d.Flags) != 1 || d.Flags[0].Code != "permission_missing" || !strings.Contains(string(d.Flags[0].Detail), "sale.create") {
		t.Errorf("flags: %+v", d.Flags)
	}
	if fmt.Sprint(d.FlagCodes) != "[permission_missing]" {
		t.Errorf("flag codes: %v", d.FlagCodes)
	}

	if _, err := rd.GetSale(ctx, f.tenant.ID, kernel.NewID()); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown sale: %v", err)
	}
	other, _, err := tenancy.NewService(f.d.App).CreateTenant(ctx, tenancy.NewTenant{Name: "Teh", Slug: "teh", Outlet: tenancy.NewOutlet{Name: "Main", Code: "BDG1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.GetSale(ctx, other.ID, sale.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("another business's sale: %v", err)
	}
	if page, _ := rd.ListSales(ctx, other.ID, sales.SaleFilter{}, nil, 50); len(page.Items) != 0 {
		t.Errorf("another business lists %d sales", len(page.Items))
	}
}

// The list makes the same number of queries for 2 sales as for 60.
func TestSalesListDoesNotQueryPerSale(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	rd := sales.NewService(f.d.App)
	shift := f.openShift(f.dev, 0)
	at := time.Now().UTC().Add(-time.Hour)
	f.ring(shift, 1, at)
	f.ring(shift, 2, at)
	list := func() { _, _ = rd.ListSales(ctx, f.tenant.ID, sales.SaleFilter{}, nil, 100) }
	few := f.d.Queries.During(list)
	for i := 0; i < 58; i++ {
		f.ring(shift, 1, at)
	}
	many := f.d.Queries.During(list)
	if few != many {
		t.Errorf("%d queries for 2 sales, %d for 60", few, many)
	}
}
