package sales_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

// fx is a business with a menu, three kinds of staff and a paired device, behind the real push
// path and the real projectors.
type fx struct {
	t      *testing.T
	d      *testdb.DB
	ids    *identity.Service
	ten    *tenancy.Service
	cat    *catalog.Service
	svc    *syncsrv.Service
	tenant tenancy.Tenant
	outlet tenancy.Outlet
	owner  identity.Caller

	cashier, manager, kitchen uuid.UUID
	dev                       identity.Principal

	latteHot, latteIced, espresso uuid.UUID // variant ids
	oatMilk, sugarNormal          uuid.UUID // modifier ids
	latteItem                     uuid.UUID

	counter int64     // receipt counter
	t0      time.Time // device time of the events, an hour ago
}

// defaultSettings are the outlet's, and so what a well-behaved device prices with.
var defaultSettings = pricing.Settings{
	TaxRate: 1100, ServiceChargeRate: 500, ServiceChargeTaxable: true, CashRoundingUnit: 100, CashRoundingMode: pricing.RoundNearest,
}

func newFx(t *testing.T) *fx {
	t.Helper()
	ctx := context.Background()
	d := testdb.New(t)
	ids, err := identity.NewService(identity.Deps{
		Pool: d.App, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	ten, cat := tenancy.NewService(d.App), catalog.NewService(d.App)
	svc, err := syncsrv.NewService(syncsrv.Deps{
		Pool: d.App, Identity: ids,
		Projectors: []syncsrv.Projector{sales.NewProjector(sales.Deps{Identity: ids, Tenancy: ten, Catalog: cat})},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, d: d, ids: ids, ten: ten, cat: cat, svc: svc, t0: time.Now().UTC().Add(-time.Hour)}

	f.tenant, f.outlet, err = ten.CreateTenant(ctx, tenancy.NewTenant{Name: "Kopi", Slug: "kopi", Outlet: tenancy.NewOutlet{Name: "Pusat", Code: "JKT1"}})
	if err != nil {
		t.Fatal(err)
	}
	st := defaultSettings
	if f.outlet, err = ten.UpdateOutletSettings(ctx, f.tenant.ID, f.outlet.ID, tenancy.SettingsUpdate{
		TaxRate: &st.TaxRate, ServiceChargeRate: &st.ServiceChargeRate, CashRoundingUnit: &st.CashRoundingUnit,
	}); err != nil {
		t.Fatal(err)
	}
	u, err := ids.CreateMember(ctx, identity.NewMember{
		TenantID: f.tenant.ID, Email: "owner@kopi.test", Password: "correct horse battery", DisplayName: "Owner", IsOwner: true, EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.owner = identity.Caller{Principal: identity.Principal{Type: identity.PrincipalUser, TenantID: f.tenant.ID, UserID: u.ID}, Access: identity.Access{IsOwner: true}}

	roles, err := ids.ListRoles(ctx, f.tenant.ID)
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]uuid.UUID{}
	for _, r := range roles {
		roleID[r.Name] = r.ID
	}
	person := func(name, role string) uuid.UUID {
		s, err := ids.CreateStaff(ctx, f.owner, identity.NewStaff{
			DisplayName: name, OutletRoles: []identity.OutletRole{{OutletID: f.outlet.ID, RoleID: roleID[role]}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	f.cashier, f.manager, f.kitchen = person("Sari", "Cashier"), person("Budi", "Manager"), person("Dewi", "Kitchen")
	f.dev = f.device("Kasir 1")

	// A small menu.
	sugar, err := cat.CreateModifierGroup(ctx, f.tenant.ID, catalog.NewModifierGroup{Name: "Sugar", MinSelect: 1, MaxSelect: 1, Required: true, Modifiers: []catalog.NewModifier{{Name: "Normal"}}})
	if err != nil {
		t.Fatal(err)
	}
	extras, err := cat.CreateModifierGroup(ctx, f.tenant.ID, catalog.NewModifierGroup{Name: "Extras", MaxSelect: 2, Modifiers: []catalog.NewModifier{{Name: "Oat milk", PriceDelta: 4000}}})
	if err != nil {
		t.Fatal(err)
	}
	f.sugarNormal, f.oatMilk = sugar.Modifiers[0].ID, extras.Modifiers[0].ID
	latte, err := cat.CreateItem(ctx, f.tenant.ID, catalog.NewItem{
		Name: "Latte", Variants: []catalog.NewVariant{{Name: "Hot", BasePrice: 28000}, {Name: "Iced", BasePrice: 30000}},
		ModifierGroupIDs: []uuid.UUID{sugar.ID, extras.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.latteItem, f.latteHot, f.latteIced = latte.ID, latte.Variants[0].ID, latte.Variants[1].ID
	esp, err := cat.CreateItem(ctx, f.tenant.ID, catalog.NewItem{Name: "Espresso", Variants: []catalog.NewVariant{{BasePrice: 18000}}})
	if err != nil {
		t.Fatal(err)
	}
	f.espresso = esp.Variants[0].ID
	return f
}

func (f *fx) device(name string) identity.Principal {
	f.t.Helper()
	p, err := f.ids.PairDevice(context.Background(), f.owner, identity.NewDevice{OutletID: f.outlet.ID, Name: name})
	if err != nil {
		f.t.Fatal(err)
	}
	return identity.Principal{Type: identity.PrincipalDevice, TenantID: f.tenant.ID, DeviceID: p.Device.ID, OutletID: f.outlet.ID}
}

// seq is the tenant's current change number: what a device that has just pulled would hold.
func (f *fx) seq() int64 {
	f.t.Helper()
	var n int64
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT change_seq FROM tenant WHERE id = $1`, f.tenant.ID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// ---- events ----

func (f *fx) event(staff uuid.UUID, typ string, payload any, at time.Time) syncsrv.Event {
	return f.eventWithID(kernel.NewID(), staff, typ, payload, at)
}

func (f *fx) eventWithID(id, staff uuid.UUID, typ string, payload any, at time.Time) syncsrv.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	return syncsrv.Event{ID: id, IdempotencyKey: id, Type: typ, StaffID: staff, DeviceTime: at, SchemaVersion: 1, Payload: raw}
}

func (f *fx) push(dev identity.Principal, events ...syncsrv.Event) syncsrv.PushResponse {
	f.t.Helper()
	res, err := f.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: events})
	if err != nil {
		f.t.Fatalf("push: %v", err)
	}
	return res
}

// one pushes a single event and returns its result.
func (f *fx) one(dev identity.Principal, ev syncsrv.Event) syncsrv.Result {
	f.t.Helper()
	return f.push(dev, ev).Results[0]
}

// openShift pushes shift.opened by the cashier and returns the shift id.
func (f *fx) openShift(dev identity.Principal, cash int64) uuid.UUID {
	f.t.Helper()
	ev := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": cash}, f.t0)
	if r := f.one(dev, ev); r.Status != syncsrv.StatusAccepted || r.Code != "" {
		f.t.Fatalf("open shift: %+v", r)
	}
	return ev.ID
}

type lineSpec struct {
	Variant   uuid.UUID
	Name      string
	Price     int64
	Qty       int64
	Modifiers []modSpec
}

type modSpec struct {
	ID    uuid.UUID
	Name  string
	Delta int64
}

type discountSpec struct {
	Line       *int
	Kind       pricing.DiscountKind
	Value      int64
	Reason     string
	ApprovedBy *uuid.UUID
}

type saleSpec struct {
	Shift      uuid.UUID
	Dev        identity.Principal // the device's code goes in the receipt number
	Number     string             // overrides the generated receipt number
	Lines      []lineSpec
	Discounts  []discountSpec
	Settings   pricing.Settings
	Tender     pricing.Tender // default cash
	CatalogSeq int64
}

// salePayload builds a sale.completed payload whose numbers are exactly what the pricing algorithm
// gives, so a test starts from a correct sale and breaks only what it means to.
func (f *fx) salePayload(s saleSpec) map[string]any {
	f.t.Helper()
	if s.Tender == "" {
		s.Tender = pricing.TenderCash
	}
	if s.Settings == (pricing.Settings{}) {
		s.Settings = defaultSettings
	}
	bill := pricing.Bill{Settings: s.Settings, Tender: s.Tender}
	for _, l := range s.Lines {
		line := pricing.Line{UnitPrice: kernel.Rupiah(l.Price), Quantity: l.Qty}
		for _, m := range l.Modifiers {
			line.ModifierDeltas = append(line.ModifierDeltas, kernel.Rupiah(m.Delta))
		}
		bill.Lines = append(bill.Lines, line)
	}
	for _, d := range s.Discounts {
		bill.Discounts = append(bill.Discounts, pricing.Discount{Line: d.Line, Kind: d.Kind, Value: d.Value})
	}
	res, err := pricing.Calculate(bill)
	if err != nil {
		f.t.Fatalf("pricing the test sale: %v", err)
	}

	f.counter++
	number := s.Number
	if number == "" {
		code := 1
		if s.Dev.DeviceID != uuid.Nil {
			code = f.deviceCode(s.Dev)
		} else {
			code = f.deviceCode(f.dev)
		}
		number = fmt.Sprintf("JKT1-%02d-%06d", code, f.counter)
	}

	lines := make([]map[string]any, len(s.Lines))
	for i, l := range s.Lines {
		mods := make([]map[string]any, len(l.Modifiers))
		for j, m := range l.Modifiers {
			mods[j] = map[string]any{"modifier_id": m.ID, "name": m.Name, "price_delta": m.Delta}
		}
		lines[i] = map[string]any{
			"variant_id": l.Variant, "name": l.Name, "unit_price": l.Price, "quantity": l.Qty, "modifiers": mods,
			"discount": int64(res.Lines[i].Discount), "allocated_bill_discount": int64(res.Lines[i].AllocatedBillDiscount), "total": int64(res.Lines[i].Total),
		}
	}
	discounts := make([]map[string]any, len(s.Discounts))
	for i, d := range s.Discounts {
		discounts[i] = map[string]any{"line": d.Line, "kind": d.Kind, "value": d.Value, "amount": int64(res.Discounts[i].Amount), "reason": d.Reason, "approved_by": d.ApprovedBy}
	}

	var payments []map[string]any
	switch {
	case res.CashTotal == 0:
	case s.Tender == pricing.TenderCash:
		tendered := (int64(res.CashTotal)/5000 + 1) * 5000
		payments = []map[string]any{{"method": "cash", "amount": int64(res.CashTotal), "tendered": tendered, "change": tendered - int64(res.CashTotal)}}
	default:
		payments = []map[string]any{{"method": "qris_manual", "amount": int64(res.CashTotal), "reference": "QR-123"}}
	}

	return map[string]any{
		"shift_id": s.Shift, "receipt_number": number, "catalog_seq": s.CatalogSeq,
		"pricing": map[string]any{
			"version": pricing.Version, "price_includes_tax": s.Settings.PriceIncludesTax, "tax_rate_bp": int64(s.Settings.TaxRate),
			"service_charge_rate_bp": int64(s.Settings.ServiceChargeRate), "service_charge_taxable": s.Settings.ServiceChargeTaxable,
			"cash_rounding_unit": int64(s.Settings.CashRoundingUnit), "cash_rounding_mode": string(s.Settings.CashRoundingMode),
		},
		"lines": lines, "discounts": discounts, "payments": payments,
		"totals": map[string]any{
			"subtotal": int64(res.Subtotal), "discount_total": int64(res.DiscountTotal), "service_charge": int64(res.ServiceCharge),
			"tax": int64(res.Tax), "rounding_amount": int64(res.RoundingAmount), "total": int64(res.Total),
		},
	}
}

func (f *fx) deviceCode(dev identity.Principal) int {
	f.t.Helper()
	var code int
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT device_code FROM device WHERE id = $1`, dev.DeviceID).Scan(&code); err != nil {
		f.t.Fatal(err)
	}
	return code
}

// latte is a common line: two hot lattes with oat milk.
func (f *fx) latte(qty int64) lineSpec {
	return lineSpec{Variant: f.latteHot, Name: "Latte (Hot)", Price: 28000, Qty: qty, Modifiers: []modSpec{{f.oatMilk, "Oat milk", 4000}}}
}

func (f *fx) espressoLine(qty int64) lineSpec {
	return lineSpec{Variant: f.espresso, Name: "Espresso", Price: 18000, Qty: qty}
}

// ---- inspection ----

func (f *fx) flags(eventID uuid.UUID) []string {
	f.t.Helper()
	rows, err := f.d.Owner.Query(context.Background(), `SELECT code FROM flag WHERE event_id = $1 ORDER BY code`, eventID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func (f *fx) flagDetail(eventID uuid.UUID, code string) string {
	f.t.Helper()
	var d string
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT detail::text FROM flag WHERE event_id = $1 AND code = $2`, eventID, code).Scan(&d); err != nil {
		f.t.Fatalf("flag %s of %s: %v", code, eventID, err)
	}
	return d
}

func (f *fx) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM `+table+` WHERE tenant_id = $1`, f.tenant.ID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fx) inboxStatus(id uuid.UUID) string {
	f.t.Helper()
	var s string
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT status FROM sync_inbox WHERE id = $1`, id).Scan(&s); err != nil {
		f.t.Fatalf("inbox row %s: %v", id, err)
	}
	return s
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
