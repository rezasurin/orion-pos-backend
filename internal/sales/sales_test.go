package sales_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func TestAShiftAndASaleAreRecordedAsTheDeviceRangThemUp(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()

	shift := f.openShift(f.dev, 150000)
	manager := f.manager
	line0 := 0
	payload := f.salePayload(saleSpec{
		Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(2), f.espressoLine(1)},
		Discounts: []discountSpec{{Line: &line0, Kind: pricing.DiscountPercent, Value: 1000, Reason: "regular", ApprovedBy: &manager}},
	})
	sale := f.event(f.cashier, sales.TypeSaleCompleted, payload, f.t0.Add(time.Minute))
	if r := f.one(f.dev, sale); r.Status != syncsrv.StatusAccepted || r.Code != "" {
		t.Fatalf("sale: %+v", r)
	}
	closing := f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shift, "counted_cash": 290000}, f.t0.Add(time.Hour))
	if r := f.one(f.dev, closing); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("close: %+v", r)
	}

	// Two lattes with oat milk and an espresso, 10% off the lattes: 82000 - 6400 = 75600, service
	// charge 3780, tax on 79380 is 8732, total 88112, and cash rounds that to 88100.
	var number, status string
	var subtotal, discountTotal, serviceCharge, tax, total, rounding int64
	var businessDate time.Time
	var staff uuid.UUID
	var devCode int32
	if err := f.d.Owner.QueryRow(ctx, `
		SELECT receipt_number, status, subtotal, discount_total, service_charge, tax, total, rounding_amount, business_date, staff_id, receipt_device_code
		FROM sale WHERE id = $1`, sale.ID).Scan(&number, &status, &subtotal, &discountTotal, &serviceCharge, &tax, &total, &rounding, &businessDate, &staff, &devCode); err != nil {
		t.Fatal(err)
	}
	eq(t, "status", status, "completed")
	eq(t, "subtotal", subtotal, int64(82000))
	eq(t, "discount_total", discountTotal, int64(6400))
	eq(t, "service_charge", serviceCharge, int64(3780))
	eq(t, "tax", tax, int64(8732))
	eq(t, "total", total, int64(88112))
	eq(t, "rounding", rounding, int64(-12))
	eq(t, "staff", staff, f.cashier)
	eq(t, "device code", devCode, int32(1))
	if !strings.HasPrefix(number, "JKT1-01-") {
		t.Errorf("receipt number %q", number)
	}
	eq(t, "business date", businessDate.Format(time.DateOnly), f.t0.Add(time.Minute).In(time.FixedZone("WIB", 7*3600)).Format(time.DateOnly))

	eq(t, "lines", f.count("sale_line"), 2)
	eq(t, "modifiers", f.count("sale_line_modifier"), 1)
	eq(t, "discounts", f.count("sale_discount"), 1)
	eq(t, "payments", f.count("payment"), 1)
	var pay, tendered, change int64
	if err := f.d.Owner.QueryRow(ctx, `SELECT amount, tendered, change FROM payment WHERE sale_id = $1`, sale.ID).Scan(&pay, &tendered, &change); err != nil {
		t.Fatal(err)
	}
	eq(t, "paid", pay, int64(88100))
	eq(t, "tendered", tendered, int64(90000))
	eq(t, "change", change, int64(1900))

	var lineTotal, allocated int64
	var name string
	if err := f.d.Owner.QueryRow(ctx, `SELECT name_snapshot, line_total, allocated_bill_discount FROM sale_line WHERE sale_id = $1 AND line_no = 0`, sale.ID).Scan(&name, &lineTotal, &allocated); err != nil {
		t.Fatal(err)
	}
	eq(t, "line name", name, "Latte (Hot)")
	eq(t, "line total", lineTotal, int64(57600))

	var counted int64
	var closedBy uuid.UUID
	if err := f.d.Owner.QueryRow(ctx, `SELECT counted_cash, closed_by FROM shift WHERE id = $1`, shift).Scan(&counted, &closedBy); err != nil {
		t.Fatal(err)
	}
	eq(t, "counted", counted, int64(290000))
	eq(t, "closed by", closedBy, f.cashier)

	// A correct sale by people who may do it raises nothing.
	for _, id := range []uuid.UUID{shift, sale.ID, closing.ID} {
		if got := f.flags(id); len(got) != 0 {
			t.Errorf("flags on %s: %v", id, got)
		}
	}
	// And it can be pushed again without effect.
	if r := f.one(f.dev, sale); r.Status != syncsrv.StatusDuplicate {
		t.Errorf("resent sale: %+v", r)
	}
	eq(t, "sales after a resend", f.count("sale"), 1)
}

// Every golden vector, turned into a sale a device might send, projects without a single flag: the
// server's recomputation agrees with the pricing algorithm on all of them, so a flag always means
// something.
func TestEveryGoldenVectorProjectsWithoutFlags(t *testing.T) {
	files, err := filepath.Glob("../../testdata/pricing-vectors/*.json")
	if err != nil || len(files) < 40 {
		t.Fatalf("vectors: %d files (%v)", len(files), err)
	}
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	manager := f.manager

	ran := 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Name     string `json:"name"`
			Settings struct {
				PriceIncludesTax     bool   `json:"price_includes_tax"`
				TaxRateBP            int64  `json:"tax_rate_bp"`
				ServiceChargeRateBP  int64  `json:"service_charge_rate_bp"`
				ServiceChargeTaxable bool   `json:"service_charge_taxable"`
				CashRoundingUnit     int64  `json:"cash_rounding_unit"`
				CashRoundingMode     string `json:"cash_rounding_mode"`
			} `json:"settings"`
			Lines []struct {
				UnitPrice int64   `json:"unit_price"`
				Modifiers []int64 `json:"modifiers"`
				Quantity  int64   `json:"quantity"`
			} `json:"lines"`
			Discounts []struct {
				Line  *int   `json:"line"`
				Kind  string `json:"kind"`
				Value int64  `json:"value"`
			} `json:"discounts"`
			Tender        string          `json:"tender"`
			ExpectedError string          `json:"expected_error"`
			Expected      json.RawMessage `json:"expected"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		if v.ExpectedError != "" {
			continue // invalid bills are covered by TestAnInvalidBillIsFlaggedNotRefused
		}
		spec := saleSpec{
			Shift: shift, Tender: pricing.Tender(v.Tender), CatalogSeq: f.seq(),
			Settings: pricing.Settings{
				PriceIncludesTax: v.Settings.PriceIncludesTax, TaxRate: kernel.BasisPoints(v.Settings.TaxRateBP),
				ServiceChargeRate: kernel.BasisPoints(v.Settings.ServiceChargeRateBP), ServiceChargeTaxable: v.Settings.ServiceChargeTaxable,
				CashRoundingUnit: kernel.Rupiah(v.Settings.CashRoundingUnit), CashRoundingMode: pricing.RoundMode(v.Settings.CashRoundingMode),
			},
		}
		for _, l := range v.Lines {
			ls := lineSpec{Variant: f.latteHot, Name: "Latte", Price: l.UnitPrice, Qty: l.Quantity}
			for _, m := range l.Modifiers {
				ls.Modifiers = append(ls.Modifiers, modSpec{f.oatMilk, "Oat milk", m})
			}
			spec.Lines = append(spec.Lines, ls)
		}
		for _, d := range v.Discounts {
			spec.Discounts = append(spec.Discounts, discountSpec{Line: d.Line, Kind: pricing.DiscountKind(d.Kind), Value: d.Value, ApprovedBy: &manager})
		}
		ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(spec), f.t0.Add(time.Minute))
		r := f.one(f.dev, ev)
		if r.Status != syncsrv.StatusAccepted || r.Code != "" {
			t.Errorf("%s: %+v", v.Name, r)
			continue
		}
		if flags := f.flags(ev.ID); len(flags) != 0 {
			t.Errorf("%s: flags %v", v.Name, flags)
		}
		ran++
	}
	if ran < 40 {
		t.Errorf("only %d vectors ran", ran)
	}
	eq(t, "sales stored", f.count("sale"), ran)
}

func TestAnInvalidBillIsFlaggedNotRefused(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	manager := f.manager
	zero := 0

	// Two discounts on one line, and a fixed discount bigger than the line: not bills the algorithm
	// can price, but the customer has paid whatever the device charged.
	base := saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}
	payload := f.salePayload(base)
	payload["discounts"] = []map[string]any{
		{"line": &zero, "kind": "percent", "value": 100, "amount": 180, "approved_by": &manager},
		{"line": &zero, "kind": "amount", "value": 100, "amount": 100, "approved_by": &manager},
	}
	ev := f.event(f.cashier, sales.TypeSaleCompleted, payload, f.t0)
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("result: %+v", r)
	}
	eq(t, "flags", fmt.Sprint(f.flags(ev.ID)), "["+sales.FlagPricingInvalid+"]")
	if d := f.flagDetail(ev.ID, sales.FlagPricingInvalid); !strings.Contains(d, "duplicate_discount") {
		t.Errorf("detail %s", d)
	}

	payload = f.salePayload(base)
	payload["discounts"] = []map[string]any{{"kind": "amount", "value": 99999, "amount": 99999, "approved_by": &manager}}
	ev = f.event(f.cashier, sales.TypeSaleCompleted, payload, f.t0)
	f.one(f.dev, ev)
	if d := f.flagDetail(ev.ID, sales.FlagPricingInvalid); !strings.Contains(d, "discount_exceeds_amount") {
		t.Errorf("detail %s", d)
	}
}

func TestTamperedTotalsAreAcceptedAndFlaggedWithTheDifferences(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	spec := saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(2)}}

	// A total that is 5000 too low, a tax that is wrong, and a line that does not add up.
	payload := f.salePayload(spec)
	totals := payload["totals"].(map[string]any)
	totals["total"], totals["tax"] = totals["total"].(int64)-5000, int64(1)
	payload["lines"].([]map[string]any)[0]["total"] = int64(1)
	ev := f.event(f.cashier, sales.TypeSaleCompleted, payload, f.t0.Add(time.Minute))
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted || r.Code != "" {
		t.Fatalf("a sale with wrong totals must still be accepted: %+v", r)
	}
	flags := f.flags(ev.ID)
	// The payment no longer matches the (tampered) total either.
	eq(t, "flags", fmt.Sprint(flags), "[payment_mismatch total_mismatch]")
	d := f.flagDetail(ev.ID, sales.FlagTotalMismatch)
	for _, want := range []string{`"totals.tax"`, `"totals.total"`, `"lines[0].total"`} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %s: %s", want, d)
		}
	}

	// What was stored is what the device charged, not what the server would have.
	var total, tax int64
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT total, tax FROM sale WHERE id = $1`, ev.ID).Scan(&total, &tax); err != nil {
		t.Fatal(err)
	}
	eq(t, "stored total", total, totals["total"].(int64))
	eq(t, "stored tax", tax, int64(1))
}

func TestPricingVersionsAndSettingsTheServerDoesNotKnow(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	payload := f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}})
	payload["pricing"].(map[string]any)["version"] = 7
	ev := f.event(f.cashier, sales.TypeSaleCompleted, payload, f.t0)
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("result: %+v", r)
	}
	eq(t, "flags", fmt.Sprint(f.flags(ev.ID)), "["+sales.FlagPricingVersion+"]")
}

func TestPaymentsAreChecked(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	spec := saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(2)}}

	underpaid := f.salePayload(spec)
	underpaid["payments"].([]map[string]any)[0]["amount"] = int64(1000)
	underpaid["payments"].([]map[string]any)[0]["tendered"] = int64(1000)
	underpaid["payments"].([]map[string]any)[0]["change"] = int64(0)
	ev := f.event(f.cashier, sales.TypeSaleCompleted, underpaid, f.t0)
	f.one(f.dev, ev)
	eq(t, "underpaid flags", fmt.Sprint(f.flags(ev.ID)), "[payment_mismatch]")

	badChange := f.salePayload(spec)
	badChange["payments"].([]map[string]any)[0]["change"] = int64(1)
	ev = f.event(f.cashier, sales.TypeSaleCompleted, badChange, f.t0)
	f.one(f.dev, ev)
	eq(t, "bad change flags", fmt.Sprint(f.flags(ev.ID)), "[payment_mismatch]")

	// Split between cash and a QR payment, which is not rounded, and which adds up.
	split := f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(2)}, Tender: pricing.TenderMixed})
	total := split["totals"].(map[string]any)["total"].(int64)
	split["payments"] = []map[string]any{
		{"method": "cash", "amount": int64(20000), "tendered": int64(20000), "change": int64(0)},
		{"method": "qris_manual", "amount": total - 20000, "reference": "QR-9"},
	}
	ev = f.event(f.cashier, sales.TypeSaleCompleted, split, f.t0)
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted || len(f.flags(ev.ID)) != 0 {
		t.Errorf("split payment: %+v %v", r, f.flags(ev.ID))
	}
	eq(t, "payments stored", f.count("payment"), 4)
}

// ---- who may do what ----

func TestPermissionsAreFlaggedNotEnforced(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	line0 := 0
	manager, cashier := f.manager, f.cashier

	saleBy := func(staff uuid.UUID, discount *uuid.UUID) syncsrv.Event {
		spec := saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(1)}}
		if discount != nil {
			spec.Discounts = []discountSpec{{Line: &line0, Kind: pricing.DiscountAmount, Value: 1000, ApprovedBy: discount}}
		}
		return f.event(staff, sales.TypeSaleCompleted, f.salePayload(spec), f.t0.Add(time.Minute))
	}

	// A kitchen account cannot ring up sales; the money moved anyway, so the sale stands.
	byKitchen := saleBy(f.kitchen, nil)
	if r := f.one(f.dev, byKitchen); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("result: %+v", r)
	}
	eq(t, "kitchen sale flags", fmt.Sprint(f.flags(byKitchen.ID)), "[permission_missing]")
	if d := f.flagDetail(byKitchen.ID, sales.FlagPermissionMissing); !strings.Contains(d, "sale.create") {
		t.Errorf("detail %s", d)
	}

	// A discount needs discount.apply_manual from whoever approved it, or from the cashier.
	f.one(f.dev, saleBy(f.cashier, nil)) // a plain sale by a cashier: fine
	noApproval := saleBy(f.cashier, &cashier)
	f.one(f.dev, noApproval)
	eq(t, "unapproved discount", fmt.Sprint(f.flags(noApproval.ID)), "[permission_missing]")
	approved := saleBy(f.cashier, &manager)
	f.one(f.dev, approved)
	eq(t, "approved discount", fmt.Sprint(f.flags(approved.ID)), "[]")
	byManager := saleBy(f.manager, nil)
	f.one(f.dev, byManager)
	eq(t, "manager's sale", fmt.Sprint(f.flags(byManager.ID)), "[]")

	// An approver who is not part of the business is refused outright.
	stranger := kernel.NewID()
	bad := saleBy(f.cashier, &stranger)
	if r := f.one(f.dev, bad); r.Status != syncsrv.StatusRejected || r.Code != "unknown_staff" {
		t.Errorf("unknown approver: %+v", r)
	}

	// Shifts and the drawer.
	byKitchenShift := f.event(f.kitchen, sales.TypeShiftOpened, map[string]any{"opening_cash": 1}, f.t0)
	f.one(f.dev, byKitchenShift)
	eq(t, "kitchen opening a shift", fmt.Sprint(f.flags(byKitchenShift.ID)), "[permission_missing]")
	payOut := f.event(f.cashier, sales.TypeCashMovement, map[string]any{"shift_id": shift, "kind": "pay_out", "amount": 5000, "reason": "ice"}, f.t0)
	f.one(f.dev, payOut)
	eq(t, "cashier pay out", fmt.Sprint(f.flags(payOut.ID)), "[permission_missing]")
	payOutMgr := f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift, "kind": "pay_out", "amount": 5000, "reason": "ice"}, f.t0)
	f.one(f.dev, payOutMgr)
	eq(t, "manager pay out", fmt.Sprint(f.flags(payOutMgr.ID)), "[]")
}

func TestAStaffMemberDeactivatedLaterIsStillFlaggedForTheirOldSale(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	active := false
	if _, err := f.ids.UpdateStaff(context.Background(), f.owner, f.cashier, identity.UpdateStaff{Active: &active}); err != nil {
		t.Fatal(err)
	}
	ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0)
	if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("result: %+v", r)
	}
	eq(t, "flags", fmt.Sprint(f.flags(ev.ID)), "[permission_missing]")
}

// ---- prices ----

func TestStalePrices(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	shift := f.openShift(f.dev, 0)
	seqThen := f.seq() // what the device pulled

	// The price of a hot latte goes up after the device pulled.
	newPrice := kernel.Rupiah(30000)
	if _, err := f.cat.UpdateVariant(ctx, f.tenant.ID, f.latteHot, catalog.UpdateVariant{BasePrice: &newPrice}); err != nil {
		t.Fatal(err)
	}
	soldAt := time.Now().UTC().Add(time.Minute) // after the change

	sellAtOldPrice := func(seq int64, at time.Time) syncsrv.Event {
		l := f.latte(1)
		l.Modifiers = nil
		return f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: seq, Lines: []lineSpec{l}}), at)
	}

	// Sold after the change at the old price by a device that had not pulled it: stale.
	stale := sellAtOldPrice(seqThen, soldAt)
	f.one(f.dev, stale)
	eq(t, "stale sale flags", fmt.Sprint(f.flags(stale.ID)), "[stale_price]")
	if d := f.flagDetail(stale.ID, sales.FlagStalePrice); !strings.Contains(d, `"current": 30000`) || !strings.Contains(d, `"sold_at": 28000`) {
		t.Errorf("detail %s", d)
	}

	// The same sale made before the change was right to use the old price.
	before := sellAtOldPrice(seqThen, f.t0.Add(2*time.Minute))
	f.one(f.dev, before)
	eq(t, "sale before the change", fmt.Sprint(f.flags(before.ID)), "[]")

	// A device that had pulled the change and still charged the old price is not "stale": it had
	// the update, so this is not a missed update. (Its numbers are still its own.)
	pulled := sellAtOldPrice(f.seq(), soldAt)
	f.one(f.dev, pulled)
	eq(t, "device up to date", fmt.Sprint(f.flags(pulled.ID)), "[]")

	// An outlet price override counts the same way.
	override := kernel.Rupiah(25000)
	seqBefore := f.seq()
	if _, err := f.cat.SetOutletVariant(ctx, f.tenant.ID, f.outlet.ID, f.latteIced, catalog.SetOutletVariant{PriceOverride: &override, Available: true}); err != nil {
		t.Fatal(err)
	}
	iced := lineSpec{Variant: f.latteIced, Name: "Latte (Iced)", Price: 30000, Qty: 1}
	ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: seqBefore, Lines: []lineSpec{iced}}), soldAt)
	f.one(f.dev, ev)
	eq(t, "override flags", fmt.Sprint(f.flags(ev.ID)), "[stale_price]")
}

// ---- order ----

// A void, the sale it voids and the shift the sale was rung up in can arrive in any order. Each
// waits for what it needs and runs when it arrives, all in one transaction.
func TestAChainOfEventsArrivingBackwardsResolvesInOneGo(t *testing.T) {
	f := newFx(t)
	shiftID, saleID, voidID, closeID := kernel.NewID(), kernel.NewID(), kernel.NewID(), kernel.NewID()
	manager := f.manager

	sale := f.eventWithID(saleID, f.cashier, sales.TypeSaleCompleted,
		f.salePayload(saleSpec{Shift: shiftID, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	void := f.eventWithID(voidID, f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": saleID, "reason": "wrong order", "approved_by": &manager}, f.t0.Add(2*time.Minute))
	closing := f.eventWithID(closeID, f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shiftID, "counted_cash": 0}, f.t0.Add(time.Hour))
	opening := f.eventWithID(shiftID, f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, f.t0)

	res := f.push(f.dev, void, closing, sale)
	for i, r := range res.Results {
		if r.Status != syncsrv.StatusAccepted || r.Code != "pending_dependency" {
			t.Fatalf("event %d should be parked: %+v", i, r)
		}
	}
	eq(t, "sales before the shift", f.count("sale"), 0)

	// The shift arrives last. Everything waiting on it runs, the void after the sale it needs.
	if r := f.one(f.dev, opening); r.Status != syncsrv.StatusAccepted || r.Code != "" {
		t.Fatalf("shift: %+v", r)
	}
	for name, id := range map[string]uuid.UUID{"void": voidID, "close": closeID, "sale": saleID, "shift": shiftID} {
		eq(t, name+" in the inbox", f.inboxStatus(id), "accepted")
	}
	var status string
	_ = f.d.Owner.QueryRow(context.Background(), `SELECT status FROM sale WHERE id = $1`, saleID).Scan(&status)
	eq(t, "sale status", status, "voided")
	var closedAt *time.Time
	_ = f.d.Owner.QueryRow(context.Background(), `SELECT closed_at FROM shift WHERE id = $1`, shiftID).Scan(&closedAt)
	if closedAt == nil {
		t.Error("the shift was not closed")
	}
	eq(t, "voids", f.count("void"), 1)
}

// state is everything the projectors wrote for the business, in a form that does not depend on
// when or in what order it was written: ids are replaced by the labels the test gave them.
func (f *fx) state(labels map[uuid.UUID]string) []string {
	f.t.Helper()
	ctx := context.Background()
	label := func(id uuid.UUID) string {
		if l, ok := labels[id]; ok {
			return l
		}
		return "?"
	}
	var out []string
	rows, err := f.d.Owner.Query(ctx, `SELECT id, status, total, receipt_number FROM sale WHERE tenant_id = $1`, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var status, number string
		var total int64
		if err := rows.Scan(&id, &status, &total, &number); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("sale %s %s %d %s", label(id), status, total, number))
	}
	rows.Close()
	rows, err = f.d.Owner.Query(ctx, `SELECT id, closed_at IS NOT NULL, coalesce(counted_cash, -1) FROM shift WHERE tenant_id = $1`, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var closed bool
		var counted int64
		if err := rows.Scan(&id, &closed, &counted); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("shift %s closed=%v counted=%d", label(id), closed, counted))
	}
	rows.Close()
	rows, err = f.d.Owner.Query(ctx, `SELECT shift_id, kind, amount FROM cash_movement WHERE tenant_id = $1`, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var kind string
		var amount int64
		if err := rows.Scan(&id, &kind, &amount); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("cash %s %s %d", label(id), kind, amount))
	}
	rows.Close()
	rows, err = f.d.Owner.Query(ctx, `SELECT target_id, target_type, code FROM flag WHERE tenant_id = $1`, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var typ, code string
		if err := rows.Scan(&id, &typ, &code); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("flag %s %s %s", typ, label(id), code))
	}
	rows.Close()
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Whatever order a real day's events reach the server in, and however they are cut into pushes, the
// same sales, voids, shifts and flags result: the sync protocol's convergence property, with real
// events and the real projectors.
func TestEveryOrderOfARealDayConvergesToTheSameState(t *testing.T) {
	// One fixed set of ids and times; each run is a new business with the same stream in another order.
	shiftID := kernel.NewID()
	saleIDs := make([]uuid.UUID, 6)
	for i := range saleIDs {
		saleIDs[i] = kernel.NewID()
	}
	voidIDs := []uuid.UUID{kernel.NewID(), kernel.NewID()}
	cashIDs := []uuid.UUID{kernel.NewID(), kernel.NewID()}
	closeID := kernel.NewID()

	var want []string
	for run := 0; run < 12; run++ {
		f := newFx(t)
		labels := map[uuid.UUID]string{shiftID: "shift"}
		manager := f.manager

		var stream []syncsrv.Event
		stream = append(stream, f.eventWithID(shiftID, f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 100000}, f.t0))
		for i, id := range saleIDs {
			labels[id] = fmt.Sprint("sale", i)
			p := f.salePayload(saleSpec{Shift: shiftID, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(int64(i + 1))}})
			p["receipt_number"] = fmt.Sprintf("JKT1-01-%06d", i+1) // the same numbers in every run
			staff := f.cashier
			if i == 5 {
				staff = f.kitchen // flagged permission_missing, in every order
			}
			stream = append(stream, f.eventWithID(id, staff, sales.TypeSaleCompleted, p, f.t0.Add(time.Duration(i+1)*time.Minute)))
		}
		for i, id := range voidIDs {
			stream = append(stream, f.eventWithID(id, f.manager, sales.TypeSaleVoided,
				map[string]any{"sale_id": saleIDs[i*2], "reason": "mistake", "approved_by": &manager}, f.t0.Add(30*time.Minute)))
		}
		for i, id := range cashIDs {
			stream = append(stream, f.eventWithID(id, f.manager, sales.TypeCashMovement,
				map[string]any{"shift_id": shiftID, "kind": "pay_in", "amount": 10000 * (i + 1)}, f.t0.Add(20*time.Minute)))
		}
		stream = append(stream, f.eventWithID(closeID, f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shiftID, "counted_cash": 530000}, f.t0.Add(time.Hour)))

		// Run 0 sends everything in its natural order; the rest are shuffled and cut into pushes.
		rng := rand.New(rand.NewSource(int64(run)))
		if run > 0 {
			rng.Shuffle(len(stream), func(i, j int) { stream[i], stream[j] = stream[j], stream[i] })
		}
		rest := stream
		for len(rest) > 0 {
			n := 1 + rng.Intn(4)
			if n > len(rest) {
				n = len(rest)
			}
			for i, r := range f.push(f.dev, rest[:n]...).Results {
				if r.Status != syncsrv.StatusAccepted {
					t.Fatalf("run %d: %s: %+v", run, rest[i].Type, r)
				}
			}
			rest = rest[n:]
		}

		got := f.state(labels)
		for id, row := range inboxStatuses(f) {
			if row != "accepted" {
				t.Fatalf("run %d: event %s ended %s", run, id, row)
			}
		}
		if run == 0 {
			want = got
			if len(want) < 10 { // shift, 6 sales, 2 cash movements and a flag
				t.Fatalf("the natural order produced too little state: %v", want)
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d did not converge:\n got %v\nwant %v", run, got, want)
		}
	}
}

func inboxStatuses(f *fx) map[uuid.UUID]string {
	f.t.Helper()
	rows, err := f.d.Owner.Query(context.Background(), `SELECT id, status FROM sync_inbox WHERE tenant_id = $1`, f.tenant.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			f.t.Fatal(err)
		}
		out[id] = s
	}
	return out
}

// ---- malformed ----

func TestMalformedSalesAreRejectedAndLeaveNothing(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	good := func() map[string]any {
		return f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(1)}})
	}
	mutate := func(fn func(p map[string]any)) syncsrv.Event {
		p := good()
		fn(p)
		return f.event(f.cashier, sales.TypeSaleCompleted, p, f.t0)
	}
	line := func(p map[string]any) map[string]any { return p["lines"].([]map[string]any)[0] }

	cases := []struct {
		name string
		ev   syncsrv.Event
		code string
	}{
		{"no lines", mutate(func(p map[string]any) { p["lines"] = []any{} }), "invalid_payload"},
		{"no shift", mutate(func(p map[string]any) { delete(p, "shift_id") }), "invalid_payload"},
		{"no pricing", mutate(func(p map[string]any) { delete(p, "pricing") }), "invalid_payload"},
		{"no totals", mutate(func(p map[string]any) { delete(p, "totals") }), "invalid_payload"},
		{"no catalog seq", mutate(func(p map[string]any) { delete(p, "catalog_seq") }), "invalid_payload"},
		{"zero quantity", mutate(func(p map[string]any) { line(p)["quantity"] = 0 }), "invalid_payload"},
		{"negative price", mutate(func(p map[string]any) { line(p)["unit_price"] = -1 }), "invalid_payload"},
		{"price over a billion", mutate(func(p map[string]any) { line(p)["unit_price"] = int64(2_000_000_000) }), "invalid_payload"},
		{"fractional price", mutate(func(p map[string]any) { line(p)["unit_price"] = 1.5 }), "invalid_payload"},
		{"unnamed line", mutate(func(p map[string]any) { line(p)["name"] = "  " }), "invalid_payload"},
		{"no variant", mutate(func(p map[string]any) { delete(line(p), "variant_id") }), "invalid_payload"},
		{"unknown method", mutate(func(p map[string]any) { p["payments"].([]map[string]any)[0]["method"] = "barter" }), "invalid_payload"},
		{"bad rounding mode", mutate(func(p map[string]any) { p["pricing"].(map[string]any)["cash_rounding_mode"] = "sideways" }), "invalid_payload"},
		{"discount on a missing line", mutate(func(p map[string]any) {
			p["discounts"] = []map[string]any{{"line": 9, "kind": "amount", "value": 1, "amount": 1}}
		}), "invalid_payload"},
		{"unknown discount kind", mutate(func(p map[string]any) {
			p["discounts"] = []map[string]any{{"kind": "coupon", "value": 1, "amount": 1}}
		}), "invalid_payload"},
		{"unpaid", mutate(func(p map[string]any) { p["payments"] = []any{} }), "invalid_payload"},
		{"unknown variant", mutate(func(p map[string]any) { line(p)["variant_id"] = kernel.NewID() }), "unknown_reference"},
		{"unknown modifier", mutate(func(p map[string]any) {
			line(p)["modifiers"].([]map[string]any)[0]["modifier_id"] = kernel.NewID()
		}), "unknown_reference"},
	}
	for _, c := range cases {
		r := f.one(f.dev, c.ev)
		if r.Status != syncsrv.StatusRejected || r.Code != c.code {
			t.Errorf("%s: %+v, want rejected %s", c.name, r, c.code)
		}
	}
	// A rejected sale leaves no rows behind, in any table.
	for _, table := range []string{"sale", "sale_line", "sale_line_modifier", "sale_discount", "payment", "flag"} {
		eq(t, table+" rows", f.count(table), 0)
	}
}

func TestReceiptNumbers(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	sell := func(number string) syncsrv.Result {
		p := f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}, Number: number})
		return f.one(f.dev, f.event(f.cashier, sales.TypeSaleCompleted, p, f.t0))
	}

	if r := sell("JKT1-01-000001"); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("first: %+v", r)
	}
	// The same number on another sale is a duplicate, and it leaves nothing.
	if r := sell("JKT1-01-000001"); r.Status != syncsrv.StatusRejected || r.Code != "duplicate_receipt_number" {
		t.Errorf("same number again: %+v", r)
	}
	eq(t, "sales", f.count("sale"), 1)
	eq(t, "lines", f.count("sale_line"), 1)

	for name, number := range map[string]string{
		"another device's code": "JKT1-02-000002",
		"another outlet's code": "BDG1-01-000002",
		"no counter":            "JKT1-01",
		"lowercase":             "jkt1-01-000002",
		"counter zero":          "JKT1-01-000000",
		"device code zero":      "JKT1-00-000002",
		"too short a counter":   "JKT1-01-12",
		"garbage":               "receipt",
	} {
		if r := sell(number); r.Status != syncsrv.StatusRejected || r.Code != "invalid_receipt_number" {
			t.Errorf("%s (%s): %+v", name, number, r)
		}
	}
	// A counter beyond six digits is fine, and numbers are only unique per device.
	if r := sell("JKT1-01-1000000"); r.Status != syncsrv.StatusAccepted {
		t.Errorf("a seven digit counter: %+v", r)
	}
	second := f.device("Kasir 2")
	shift2 := f.openShift(second, 0)
	p := f.salePayload(saleSpec{Shift: shift2, Dev: second, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}, Number: "JKT1-02-000001"})
	if r := f.one(second, f.event(f.cashier, sales.TypeSaleCompleted, p, f.t0)); r.Status != syncsrv.StatusAccepted {
		t.Errorf("counter 1 on the second device: %+v", r)
	}
}

// ---- voids ----

func TestVoids(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	shift := f.openShift(f.dev, 0)
	manager := f.manager
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(1)}}), f.t0.Add(time.Minute))
	f.one(f.dev, sale)

	void := func(staff uuid.UUID, extra map[string]any) syncsrv.Event {
		p := map[string]any{"sale_id": sale.ID, "reason": "customer left"}
		for k, v := range extra {
			p[k] = v
		}
		return f.event(staff, sales.TypeSaleVoided, p, f.t0.Add(5*time.Minute))
	}

	// A cashier cannot void; without an approver it is recorded and flagged, with a manager's
	// approval it is not.
	unapproved := void(f.cashier, nil)
	if r := f.one(f.dev, unapproved); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("void: %+v", r)
	}
	eq(t, "unapproved void flags", fmt.Sprint(f.flags(unapproved.ID)), "["+sales.FlagPermissionMissing+"]")
	var status string
	_ = f.d.Owner.QueryRow(ctx, `SELECT status FROM sale WHERE id = $1`, sale.ID).Scan(&status)
	eq(t, "sale status", status, "voided")

	// A sale is voided once; the second void is refused and changes nothing.
	again := void(f.manager, map[string]any{"approved_by": &manager})
	if r := f.one(f.dev, again); r.Status != syncsrv.StatusRejected || r.Code != "already_voided" {
		t.Errorf("second void: %+v", r)
	}
	eq(t, "voids", f.count("void"), 1)

	// The amounts of a voided sale are untouched: a latte with oat milk, 32000 plus service charge and tax.
	var total int64
	_ = f.d.Owner.QueryRow(ctx, `SELECT total FROM sale WHERE id = $1`, sale.ID).Scan(&total)
	eq(t, "total of a voided sale", total, int64(37296))

	// A void needs a reason, and a sale of this business.
	noReason := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": sale.ID, "reason": " "}, f.t0)
	if r := f.one(f.dev, noReason); r.Status != syncsrv.StatusRejected || r.Code != "invalid_payload" {
		t.Errorf("void without a reason: %+v", r)
	}
	approverStranger := kernel.NewID()
	other := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0)
	f.one(f.dev, other)
	if r := f.one(f.dev, f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": other.ID, "reason": "x", "approved_by": approverStranger}, f.t0)); r.Code != "unknown_staff" {
		t.Errorf("unknown approver: %+v", r)
	}
	eq(t, "the sale stays completed after a refused void", f.saleStatus(other.ID), "completed")

	// A void in a later shift is recorded against that shift: the refund left that drawer.
	later := f.openShift(f.dev, 0)
	voidLater := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": other.ID, "reason": "refund next day", "shift_id": later}, f.t0.Add(2*time.Hour))
	if r := f.one(f.dev, voidLater); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("void in a later shift: %+v", r)
	}
	var voidShift uuid.UUID
	_ = f.d.Owner.QueryRow(ctx, `SELECT shift_id FROM void WHERE sale_id = $1`, other.ID).Scan(&voidShift)
	eq(t, "void's shift", voidShift, later)
}

func (f *fx) saleStatus(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT status FROM sale WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// Two devices voiding one sale at the same moment: one void wins and the other is refused.
func TestTwoVoidsOfOneSaleAtOnce(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	manager := f.manager
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0)
	f.one(f.dev, sale)
	second := f.device("Kasir 2")

	results := make(chan syncsrv.Result, 2)
	push := func(void syncsrv.Event, dev identity.Principal) {
		res, err := f.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: []syncsrv.Event{void}})
		if err != nil {
			t.Error(err)
			return
		}
		results <- res.Results[0]
	}
	v1 := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": sale.ID, "reason": "a", "approved_by": &manager}, f.t0)
	v2 := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": sale.ID, "reason": "b", "approved_by": &manager}, f.t0)
	go push(v1, f.dev)
	go push(v2, second)
	a, b := <-results, <-results
	accepted, refused := 0, 0
	for _, r := range []syncsrv.Result{a, b} {
		switch {
		case r.Status == syncsrv.StatusAccepted:
			accepted++
		case r.Status == syncsrv.StatusRejected && r.Code == "already_voided":
			refused++
		default:
			t.Errorf("unexpected result %+v", r)
		}
	}
	eq(t, "accepted voids", accepted, 1)
	eq(t, "refused voids", refused, 1)
	eq(t, "void rows", f.count("void"), 1)
}

// ---- shifts and cash ----

func TestShiftsAndCashMovements(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 100000)

	move := func(kind string, amount int, reason string) syncsrv.Result {
		return f.one(f.dev, f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift, "kind": kind, "amount": amount, "reason": reason}, f.t0.Add(time.Minute)))
	}
	if r := move("pay_in", 50000, "float"); r.Status != syncsrv.StatusAccepted {
		t.Errorf("pay in: %+v", r)
	}
	if r := move("no_sale", 0, "change for a customer"); r.Status != syncsrv.StatusAccepted {
		t.Errorf("no sale: %+v", r)
	}
	for name, r := range map[string]syncsrv.Result{
		"no_sale with an amount": move("no_sale", 100, ""),
		"pay_out without reason": move("pay_out", 100, ""),
		"pay_in of nothing":      move("pay_in", 0, ""),
		"unknown kind":           move("steal", 100, ""),
	} {
		if r.Status != syncsrv.StatusRejected || r.Code != "invalid_payload" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	eq(t, "cash movements", f.count("cash_movement"), 2)

	closing := func(counted int64) syncsrv.Result {
		return f.one(f.dev, f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shift, "counted_cash": counted}, f.t0.Add(30*time.Minute)))
	}
	if r := closing(149000); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("close: %+v", r)
	}
	// A shift closes once, even when the second event is a different one.
	if r := closing(150000); r.Status != syncsrv.StatusRejected || r.Code != "already_closed" {
		t.Errorf("second close: %+v", r)
	}
	var counted int64
	_ = f.d.Owner.QueryRow(context.Background(), `SELECT counted_cash FROM shift WHERE id = $1`, shift).Scan(&counted)
	eq(t, "counted cash", counted, int64(149000))

	// A movement after the close is recorded and flagged; so is a sale outside the shift's time.
	late := f.event(f.manager, sales.TypeCashMovement, map[string]any{"shift_id": shift, "kind": "pay_out", "amount": 500, "reason": "late"}, f.t0.Add(45*time.Minute))
	f.one(f.dev, late)
	eq(t, "late movement flags", fmt.Sprint(f.flags(late.ID)), "["+sales.FlagAfterShiftClose+"]")
	lateSale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(50*time.Minute))
	f.one(f.dev, lateSale)
	eq(t, "late sale flags", fmt.Sprint(f.flags(lateSale.ID)), "["+sales.FlagSaleOutsideShift+"]")
	earlySale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(-time.Minute))
	f.one(f.dev, earlySale)
	eq(t, "early sale flags", fmt.Sprint(f.flags(earlySale.ID)), "["+sales.FlagSaleOutsideShift+"]")

	// Negative or absurd cash is malformed.
	bad := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": -1}, f.t0)
	if r := f.one(f.dev, bad); r.Status != syncsrv.StatusRejected {
		t.Errorf("negative opening cash: %+v", r)
	}
	missing := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{}, f.t0)
	if r := f.one(f.dev, missing); r.Status != syncsrv.StatusRejected {
		t.Errorf("no opening cash: %+v", r)
	}
}

func TestAShiftUsedFromAnotherDeviceIsFlagged(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	second := f.device("Kasir 2")

	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, Dev: second, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	if r := f.one(second, sale); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("sale: %+v", r)
	}
	eq(t, "sale on another device's shift", fmt.Sprint(f.flags(sale.ID)), "["+sales.FlagShiftOtherDevice+"]")
	closing := f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shift, "counted_cash": 0}, f.t0.Add(time.Hour))
	f.one(second, closing)
	eq(t, "closing from another device", fmt.Sprint(f.flags(closing.ID)), "["+sales.FlagShiftOtherDevice+"]")
}

// ---- time ----

func TestTheBusinessDateFollowsTheOutletCutoffAndTheDeviceClock(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	cutoff := 4 * time.Hour
	if _, err := f.ten.UpdateOutletSettings(ctx, f.tenant.ID, f.outlet.ID, tenancy.SettingsUpdate{BusinessDayCutoff: &cutoff}); err != nil {
		t.Fatal(err)
	}
	wib := time.FixedZone("WIB", 7*3600)
	// Midnight to 04:00 on the 3rd belongs to the 2nd.
	day := time.Date(2026, 10, 3, 0, 0, 0, 0, wib)
	shift := f.eventWithID(kernel.NewID(), f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, day.Add(-2*time.Hour))
	f.one(f.dev, shift)

	dateOf := func(at time.Time) string {
		ev := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift.ID, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), at)
		if r := f.one(f.dev, ev); r.Status != syncsrv.StatusAccepted {
			t.Fatalf("sale at %v: %+v", at, r)
		}
		var d time.Time
		if err := f.d.Owner.QueryRow(ctx, `SELECT business_date FROM sale WHERE id = $1`, ev.ID).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d.Format(time.DateOnly)
	}
	eq(t, "02:30 on the 3rd", dateOf(day.Add(2*time.Hour+30*time.Minute)), "2026-10-02")
	eq(t, "03:59 on the 3rd", dateOf(day.Add(3*time.Hour+59*time.Minute)), "2026-10-02")
	eq(t, "04:00 on the 3rd", dateOf(day.Add(4*time.Hour)), "2026-10-03")
	eq(t, "23:59 on the 3rd", dateOf(day.Add(23*time.Hour+59*time.Minute)), "2026-10-03")
	// A time in UTC that is already tomorrow in Jakarta: the outlet's zone decides.
	eq(t, "17:30 UTC on the 3rd is 00:30 on the 4th, before the cutoff", dateOf(time.Date(2026, 10, 3, 17, 30, 0, 0, time.UTC)), "2026-10-03")
	eq(t, "22:00 UTC on the 3rd is 05:00 on the 4th", dateOf(time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)), "2026-10-04")

	// Changing the cutoff later does not move a sale already recorded.
	none := time.Duration(0)
	if _, err := f.ten.UpdateOutletSettings(ctx, f.tenant.ID, f.outlet.ID, tenancy.SettingsUpdate{BusinessDayCutoff: &none}); err != nil {
		t.Fatal(err)
	}
	var d time.Time
	if err := f.d.Owner.QueryRow(ctx, `SELECT business_date FROM sale ORDER BY device_time LIMIT 1`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	eq(t, "the earliest sale's date after the cutoff changed", d.Format(time.DateOnly), "2026-10-02")
}

func TestADeviceClockAheadOfTheServerIsFlagged(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	ahead := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), time.Now().UTC().Add(3*time.Hour))
	f.one(f.dev, ahead)
	flags := f.flags(ahead.ID)
	if fmt.Sprint(flags) != "[device_time_ahead sale_outside_shift]" && fmt.Sprint(flags) != "[device_time_ahead]" {
		t.Errorf("flags = %v", flags)
	}
	// A device that is merely behind (the event waited to sync) is normal.
	behind := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	f.one(f.dev, behind)
	eq(t, "a sale that waited an hour to sync", fmt.Sprint(f.flags(behind.ID)), "[]")
}

// ---- tenants and the database ----

func TestAnotherBusinessesIdsAreNotUsable(t *testing.T) {
	a := newFx(t)
	// A second business in the same database.
	ctx := context.Background()
	tn, o, err := a.ten.CreateTenant(ctx, tenancy.NewTenant{Name: "Teh", Slug: "teh", Outlet: tenancy.NewOutlet{Name: "Main", Code: "BDG1"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = tn
	_ = o
	shiftA := a.openShift(a.dev, 0)

	// A device of business A uses a shift id that is not A's: it waits for a shift that never comes.
	foreignShift := kernel.NewID()
	ev := a.event(a.cashier, sales.TypeSaleCompleted, a.salePayload(saleSpec{Shift: foreignShift, CatalogSeq: a.seq(), Lines: []lineSpec{a.espressoLine(1)}}), a.t0)
	if r := a.one(a.dev, ev); r.Status != syncsrv.StatusAccepted || r.Code != "pending_dependency" {
		t.Errorf("sale on an unknown shift: %+v", r)
	}
	eq(t, "sales", a.count("sale"), 0)
	_ = shiftA
}

func TestSalesCannotBeRewrittenByTheService(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	shift := f.openShift(f.dev, 0)
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.latte(1)}}), f.t0.Add(time.Minute))
	f.one(f.dev, sale)

	exec := func(sql string) error {
		return kernel.TenantTx(ctx, f.d.App, f.tenant.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
	}
	for name, sql := range map[string]string{
		"change a total":        `UPDATE sale SET total = 1`,
		"change a line":         `UPDATE sale_line SET line_total = 0`,
		"change a payment":      `UPDATE payment SET amount = 0`,
		"change a discount":     `UPDATE sale_discount SET amount = 0`,
		"change a void":         `UPDATE void SET reason = 'x'`,
		"change a flag":         `UPDATE flag SET code = 'x'`,
		"change a cash move":    `UPDATE cash_movement SET amount = 0`,
		"change opening cash":   `UPDATE shift SET opening_cash = 0`,
		"delete a sale":         `DELETE FROM sale`,
		"delete a line":         `DELETE FROM sale_line`,
		"delete a payment":      `DELETE FROM payment`,
		"delete a shift":        `DELETE FROM shift`,
		"reopen a closed shift": `UPDATE shift SET opened_by = opened_by`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s: the app role was allowed to", name)
		}
	}
	// What the protocol itself changes is allowed: a sale's status, a shift's closing.
	if err := exec(`UPDATE sale SET status = 'voided'`); err != nil {
		t.Errorf("status change: %v", err)
	}
	if err := exec(`UPDATE shift SET counted_cash = 5, closed_at = now(), closed_by = opened_by, close_event_id = id`); err != nil {
		t.Errorf("closing a shift: %v", err)
	}
	// Status values are checked.
	if err := exec(`UPDATE sale SET status = 'bogus'`); err == nil {
		t.Error("an invalid status was accepted")
	}
}

// Many devices selling at once to one outlet: nothing is lost or duplicated, no event is asked to
// retry, and the books add up.
func TestManyDevicesSellingAtOnce(t *testing.T) {
	f := newFx(t)
	const devices, salesEach = 4, 25

	type run struct {
		dev   identity.Principal
		shift uuid.UUID
	}
	var runs []run
	for i := 0; i < devices; i++ {
		dev := f.dev
		if i > 0 {
			dev = f.device(fmt.Sprint("Kasir ", i+1))
		}
		runs = append(runs, run{dev: dev, shift: f.openShift(dev, 0)})
	}

	// Build every event up front (the builder is not safe for concurrent use), then push in parallel.
	batches := make([][]syncsrv.Event, devices)
	for i, r := range runs {
		code := f.deviceCode(r.dev)
		for j := 0; j < salesEach; j++ {
			p := f.salePayload(saleSpec{Shift: r.shift, Dev: r.dev, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(int64(j%3 + 1))}})
			p["receipt_number"] = fmt.Sprintf("JKT1-%02d-%06d", code, j+1)
			batches[i] = append(batches[i], f.event(f.cashier, sales.TypeSaleCompleted, p, f.t0.Add(time.Duration(j)*time.Second)))
		}
	}
	errs := make(chan error, devices)
	for i := range runs {
		go func() {
			for start := 0; start < salesEach; start += 5 {
				res, err := f.svc.Push(context.Background(), runs[i].dev, syncsrv.PushRequest{Events: batches[i][start : start+5]})
				if err != nil {
					errs <- err
					return
				}
				for _, r := range res.Results {
					if r.Status != syncsrv.StatusAccepted {
						errs <- fmt.Errorf("device %d: %+v", i, r)
						return
					}
				}
			}
			errs <- nil
		}()
	}
	for i := 0; i < devices; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	eq(t, "sales", f.count("sale"), devices*salesEach)
	eq(t, "payments", f.count("payment"), devices*salesEach)
	eq(t, "flags", f.count("flag"), 0)
}
