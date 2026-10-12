package sync_test

// The pull tests (BACKEND_PLAN.md section 5.2, task B1.6): what a device downloads, as deltas from
// its cursor, and the property that matters most, that a device applying every delta ends up with
// exactly what a fresh snapshot would give it.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func (b *biz) pull(dev identity.Principal, cursor string, limit int) syncsrv.PullResponse {
	b.w.t.Helper()
	res, err := b.w.svc.Pull(context.Background(), dev, syncsrv.PullRequest{Cursor: cursor, Limit: limit})
	if err != nil {
		b.w.t.Fatalf("pull: %v", err)
	}
	return res
}

func (b *biz) cat() *catalog.Service { return catalog.NewService(b.w.d.App) }

func (b *biz) item(name string, price kernel.Rupiah) catalog.Item {
	b.w.t.Helper()
	it, err := b.cat().CreateItem(context.Background(), b.tenant.ID, catalog.NewItem{Name: name, Variants: []catalog.NewVariant{{BasePrice: price}}})
	if err != nil {
		b.w.t.Fatal(err)
	}
	return it
}

// secondOutlet adds another outlet to the business, with a device. Tenancy cannot create outlets
// yet (that arrives with signup), so it is arranged directly.
func (b *biz) secondOutlet(code string) (uuid.UUID, identity.Principal) {
	b.w.t.Helper()
	ctx := context.Background()
	id := kernel.NewID()
	if _, err := b.w.d.Owner.Exec(ctx, `INSERT INTO outlet (id, tenant_id, name, code) VALUES ($1, $2, 'Cabang', $3)`, id, b.tenant.ID, code); err != nil {
		b.w.t.Fatal(err)
	}
	if _, err := b.w.d.Owner.Exec(ctx, `INSERT INTO outlet_settings (outlet_id, tenant_id) VALUES ($1, $2)`, id, b.tenant.ID); err != nil {
		b.w.t.Fatal(err)
	}
	paired, err := b.w.ids.PairDevice(ctx, b.owner, identity.NewDevice{OutletID: id, Name: "Kasir cabang"})
	if err != nil {
		b.w.t.Fatal(err)
	}
	return id, identity.Principal{Type: identity.PrincipalDevice, TenantID: b.tenant.ID, DeviceID: paired.Device.ID, OutletID: id}
}

func TestAFirstPullIsAFullSnapshot(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ctx := context.Background()

	cat, _ := b.cat().CreateCategory(ctx, b.tenant.ID, catalog.NewCategory{Name: "Coffee"})
	group, _ := b.cat().CreateModifierGroup(ctx, b.tenant.ID, catalog.NewModifierGroup{Name: "Sugar", MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: "Less"}, {Name: "Normal"}}})
	latte, err := b.cat().CreateItem(ctx, b.tenant.ID, catalog.NewItem{
		Name: "Latte", CategoryID: &cat.ID, ModifierGroupIDs: []uuid.UUID{group.ID},
		Variants: []catalog.NewVariant{{Name: "Hot", BasePrice: 28000}, {Name: "Iced", BasePrice: 30000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	old := b.item("Old special", 10000)
	archived := true
	if _, err := b.cat().UpdateItem(ctx, b.tenant.ID, old.ID, catalog.UpdateItem{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	override := kernel.Rupiah(26000)
	if _, err := b.cat().SetOutletVariant(ctx, b.tenant.ID, b.outlet.ID, latte.Variants[0].ID, catalog.SetOutletVariant{PriceOverride: &override, Available: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.ids.SetPIN(ctx, b.owner, b.staff, "4821"); err != nil {
		t.Fatal(err)
	}

	res := b.pull(dev, "", 0)
	if !res.Snapshot || res.HasMore || res.Cursor == "" {
		t.Fatalf("snapshot = %v, has_more = %v, cursor = %q", res.Snapshot, res.HasMore, res.Cursor)
	}
	if len(res.Catalog.Categories) != 1 || len(res.Catalog.Groups) != 1 || len(res.Catalog.Groups[0].Modifiers) != 2 {
		t.Errorf("categories %d, groups %+v", len(res.Catalog.Categories), res.Catalog.Groups)
	}
	if len(res.Catalog.Items) != 2 {
		t.Fatalf("items: %+v", res.Catalog.Items)
	}
	names := map[string]catalog.Item{}
	for _, it := range res.Catalog.Items {
		names[it.Name] = it
	}
	if got := names["Latte"]; len(got.Variants) != 2 || len(got.ModifierGroupIDs) != 1 || got.CategoryID == nil {
		t.Errorf("latte = %+v", got)
	}
	if got := names["Old special"]; got.ArchivedAt == nil {
		t.Errorf("archived items are sent, marked archived: %+v", got)
	}
	if len(res.Catalog.OutletVariants) != 1 || *res.Catalog.OutletVariants[0].PriceOverride != 26000 {
		t.Errorf("overrides: %+v", res.Catalog.OutletVariants)
	}
	if res.Outlet == nil || res.Outlet.Code != "JKT1" || res.Outlet.Settings.Timezone != "Asia/Jakarta" {
		t.Errorf("outlet: %+v", res.Outlet)
	}
	if len(res.Staff) != 2 { // the cashier and the owner
		t.Errorf("roster: %+v", res.Staff)
	}
	if len(res.Entitlements.Items) == 0 || !res.EntitlementsExpiresAt.Equal(res.ServerTime.Add(syncsrv.EntitlementsTTL)) {
		t.Errorf("entitlements: %d items, expires %v (server time %v)", len(res.Entitlements.Items), res.EntitlementsExpiresAt, res.ServerTime)
	}
	if len(res.RemovedStaff) != 0 || len(res.Deleted) != 0 {
		t.Errorf("a snapshot removes nothing: %v %v", res.RemovedStaff, res.Deleted)
	}
}

func TestDeltasCarryOnlyWhatChangedAndAnEmptyPullChangesNothing(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ctx := context.Background()
	a, other := b.item("Latte", 28000), b.item("Tea", 12000)

	first := b.pull(dev, "", 0)
	// Nothing happened since: no entities, and the same cursor comes back.
	quiet := b.pull(dev, first.Cursor, 0)
	if quiet.Snapshot || quiet.HasMore || quiet.Cursor != first.Cursor {
		t.Errorf("quiet pull: snapshot %v, has_more %v, cursor %q vs %q", quiet.Snapshot, quiet.HasMore, quiet.Cursor, first.Cursor)
	}
	if len(quiet.Catalog.Items)+len(quiet.Catalog.Categories)+len(quiet.Catalog.Groups)+len(quiet.Staff)+len(quiet.RemovedStaff) != 0 || quiet.Outlet != nil {
		t.Errorf("a quiet pull carried data: %+v", quiet)
	}
	if len(quiet.Entitlements.Items) == 0 {
		t.Error("entitlements ride on every pull")
	}

	// One price changes: one item comes back, with all its variants, and nothing else.
	price := kernel.Rupiah(30000)
	if _, err := b.cat().UpdateVariant(ctx, b.tenant.ID, a.Variants[0].ID, catalog.UpdateVariant{BasePrice: &price}); err != nil {
		t.Fatal(err)
	}
	next := b.pull(dev, first.Cursor, 0)
	if len(next.Catalog.Items) != 1 || next.Catalog.Items[0].ID != a.ID || next.Catalog.Items[0].Variants[0].BasePrice != 30000 {
		t.Fatalf("delta items: %+v", next.Catalog.Items)
	}
	if next.Cursor == first.Cursor || next.Snapshot || len(next.Catalog.Categories) != 0 || next.Outlet != nil {
		t.Errorf("delta: %+v", next)
	}
	_ = other

	// Several edits to one item in a row arrive as one entity, in its latest state.
	for _, name := range []string{"Latte 2", "Latte 3", "Latte 4"} {
		n := name
		if _, err := b.cat().UpdateItem(ctx, b.tenant.ID, a.ID, catalog.UpdateItem{Name: &n}); err != nil {
			t.Fatal(err)
		}
	}
	last := b.pull(dev, next.Cursor, 0)
	if len(last.Catalog.Items) != 1 || last.Catalog.Items[0].Name != "Latte 4" {
		t.Errorf("collapsed edits: %+v", last.Catalog.Items)
	}
}

func TestPullPagesThroughAnyNumberOfChanges(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	start := b.pull(dev, "", 0)

	want := map[uuid.UUID]bool{}
	for i := 0; i < 25; i++ {
		want[b.item(fmt.Sprint("Item ", i), 1000).ID] = true
	}
	got := map[uuid.UUID]bool{}
	cursor, pages := start.Cursor, 0
	for {
		res := b.pull(dev, cursor, 10)
		pages++
		if len(res.Catalog.Items) > 10 {
			t.Fatalf("page %d has %d items for a limit of 10", pages, len(res.Catalog.Items))
		}
		for _, it := range res.Catalog.Items {
			got[it.ID] = true
		}
		cursor = res.Cursor
		if !res.HasMore {
			break
		}
		if pages > 10 {
			t.Fatal("never finished")
		}
	}
	if pages != 3 || len(got) != 25 {
		t.Errorf("%d pages, %d items; want 3 and 25", pages, len(got))
	}
	for id := range want {
		if !got[id] {
			t.Errorf("item %s never arrived", id)
		}
	}
	// And the device is now up to date.
	if res := b.pull(dev, cursor, 10); len(res.Catalog.Items) != 0 || res.HasMore {
		t.Errorf("after paging: %+v", res.Catalog.Items)
	}
}

func TestACursorTheServerDoesNotKnowGivesASnapshot(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	b.item("Latte", 28000)
	good := b.pull(dev, "", 0)

	for name, cursor := range map[string]string{
		"garbage":           "not a cursor",
		"another encoding":  "Yw", // valid base64url, wrong content
		"from the future":   syncCursorAhead(t, w, b),
		"cut off":           good.Cursor[:len(good.Cursor)-2],
		"empty after valid": "",
	} {
		res := b.pull(dev, cursor, 0)
		if !res.Snapshot || len(res.Catalog.Items) != 1 {
			t.Errorf("%s: snapshot %v with %d items", name, res.Snapshot, len(res.Catalog.Items))
		}
	}
}

// syncCursorAhead is a cursor for a change number the server has not reached, as a restored
// database would produce.
func syncCursorAhead(t *testing.T, w *world, b *biz) string {
	t.Helper()
	var head int64
	if err := w.d.Owner.QueryRow(context.Background(), `SELECT change_seq FROM tenant WHERE id = $1`, b.tenant.ID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	// Build it the way the server does: pull from a copy of the data one change number ahead.
	if _, err := w.d.Owner.Exec(context.Background(), `UPDATE tenant SET change_seq = change_seq + 1000 WHERE id = $1`, b.tenant.ID); err != nil {
		t.Fatal(err)
	}
	res := b.pull(b.device("temp"), "", 0)
	if _, err := w.d.Owner.Exec(context.Background(), `UPDATE tenant SET change_seq = $2 WHERE id = $1`, b.tenant.ID, head); err != nil {
		t.Fatal(err)
	}
	return res.Cursor
}

func TestADeviceHearsOnlyAboutItsOwnOutlet(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	otherOutlet, otherDev := b.secondOutlet("BDG1")
	ctx := context.Background()
	it := b.item("Latte", 28000)
	v := it.Variants[0].ID

	startHere, startThere := b.pull(dev, "", 0), b.pull(otherDev, "", 0)

	// A price override and a settings change at the other outlet.
	price := kernel.Rupiah(20000)
	if _, err := b.cat().SetOutletVariant(ctx, b.tenant.ID, otherOutlet, v, catalog.SetOutletVariant{PriceOverride: &price, Available: false}); err != nil {
		t.Fatal(err)
	}
	rate := kernel.BasisPoints(1100)
	if _, err := tenancy.NewService(w.d.App).UpdateOutletSettings(ctx, b.tenant.ID, otherOutlet, tenancy.SettingsUpdate{TaxRate: &rate}); err != nil {
		t.Fatal(err)
	}

	here := b.pull(dev, startHere.Cursor, 0)
	if len(here.Catalog.OutletVariants) != 0 || here.Outlet != nil {
		t.Errorf("outlet A heard about outlet B: %+v / %+v", here.Catalog.OutletVariants, here.Outlet)
	}
	// The cursor moves past what was not for this outlet, so it is not scanned again.
	if again := b.pull(dev, here.Cursor, 0); again.Cursor != here.Cursor {
		t.Errorf("cursor %q then %q", here.Cursor, again.Cursor)
	}

	there := b.pull(otherDev, startThere.Cursor, 0)
	if len(there.Catalog.OutletVariants) != 1 || *there.Catalog.OutletVariants[0].PriceOverride != 20000 || there.Catalog.OutletVariants[0].Available {
		t.Errorf("outlet B: %+v", there.Catalog.OutletVariants)
	}
	if there.Outlet == nil || there.Outlet.Settings.TaxRate != 1100 {
		t.Errorf("outlet B settings: %+v", there.Outlet)
	}
	// A snapshot at outlet A holds none of B's overrides either.
	if snap := b.pull(dev, "", 0); len(snap.Catalog.OutletVariants) != 0 {
		t.Errorf("snapshot at A: %+v", snap.Catalog.OutletVariants)
	}
}

func TestSettingsChangesReachTheDevice(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	start := b.pull(dev, "", 0)
	rate, cutoff := kernel.BasisPoints(1000), 4*time.Hour
	if _, err := tenancy.NewService(w.d.App).UpdateOutletSettings(context.Background(), b.tenant.ID, b.outlet.ID, tenancy.SettingsUpdate{TaxRate: &rate, BusinessDayCutoff: &cutoff}); err != nil {
		t.Fatal(err)
	}
	res := b.pull(dev, start.Cursor, 0)
	if res.Outlet == nil || res.Outlet.Settings.TaxRate != 1000 || res.Outlet.Settings.BusinessDayCutoff != cutoff {
		t.Errorf("outlet: %+v", res.Outlet)
	}
}

func TestStaffChangesReachTheDevice(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ctx := context.Background()
	start := b.pull(dev, "", 0)

	// A new PIN for the cashier.
	if err := w.ids.SetPIN(ctx, b.owner, b.staff, "9071"); err != nil {
		t.Fatal(err)
	}
	res := b.pull(dev, start.Cursor, 0)
	if len(res.Staff) != 1 || res.Staff[0].StaffID != b.staff || res.Staff[0].PINHash == nil || len(res.Staff[0].Permissions) == 0 {
		t.Fatalf("after a PIN change: %+v", res.Staff)
	}
	if *res.Staff[0].PINHash == "" {
		t.Error("no hash")
	}

	// A new cashier appears; then is deactivated, and goes away again.
	roles, _ := w.ids.ListRoles(ctx, b.tenant.ID)
	var cashierRole uuid.UUID
	for _, r := range roles {
		if r.Name == "Cashier" {
			cashierRole = r.ID
		}
	}
	nu, err := w.ids.CreateStaff(ctx, b.owner, identity.NewStaff{DisplayName: "Budi", PIN: "7413", OutletRoles: []identity.OutletRole{{OutletID: b.outlet.ID, RoleID: cashierRole}}})
	if err != nil {
		t.Fatal(err)
	}
	res2 := b.pull(dev, res.Cursor, 0)
	if len(res2.Staff) != 1 || res2.Staff[0].StaffID != nu.ID || res2.Staff[0].DisplayName != "Budi" {
		t.Fatalf("new cashier: %+v", res2.Staff)
	}
	inactive := false
	if _, err := w.ids.UpdateStaff(ctx, b.owner, nu.ID, identity.UpdateStaff{Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	res3 := b.pull(dev, res2.Cursor, 0)
	if len(res3.Staff) != 0 || len(res3.RemovedStaff) != 1 || res3.RemovedStaff[0] != nu.ID {
		t.Errorf("deactivated: staff %+v removed %v", res3.Staff, res3.RemovedStaff)
	}

	// Moved to another outlet only: gone from this roster, though still active.
	otherOutlet, _ := b.secondOutlet("BDG1")
	moved, err := w.ids.CreateStaff(ctx, b.owner, identity.NewStaff{DisplayName: "Dewi", OutletRoles: []identity.OutletRole{{OutletID: b.outlet.ID, RoleID: cashierRole}}})
	if err != nil {
		t.Fatal(err)
	}
	cur := b.pull(dev, res3.Cursor, 0).Cursor
	to := []identity.OutletRole{{OutletID: otherOutlet, RoleID: cashierRole}}
	if _, err := w.ids.UpdateStaff(ctx, b.owner, moved.ID, identity.UpdateStaff{OutletRoles: &to}); err != nil {
		t.Fatal(err)
	}
	res4 := b.pull(dev, cur, 0)
	if len(res4.RemovedStaff) != 1 || res4.RemovedStaff[0] != moved.ID {
		t.Errorf("moved to another outlet: staff %+v removed %v", res4.Staff, res4.RemovedStaff)
	}
	// A snapshot lists exactly who is on the roster now.
	snap := b.pull(dev, "", 0)
	var ids []uuid.UUID
	for _, s := range snap.Staff {
		ids = append(ids, s.StaffID)
	}
	if len(ids) != 2 { // the owner and the first cashier
		t.Errorf("roster in the snapshot: %v", ids)
	}
}

// A fixed number of queries serves a pull however much changed.
func TestPullDoesNotQueryPerChange(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	cursor := b.pull(dev, "", 0).Cursor

	b.item("One", 1000)
	oneCount := w.d.Queries.During(func() { b.pull(dev, cursor, 0) })

	for i := 0; i < 40; i++ {
		b.item(fmt.Sprint("Item ", i), 1000)
	}
	manyCount := w.d.Queries.During(func() { b.pull(dev, cursor, 0) })
	if oneCount != manyCount {
		t.Errorf("%d queries for 1 change, %d for 41", oneCount, manyCount)
	}
}

// The property the whole design rests on: a device that applies every delta holds exactly what a
// fresh snapshot would give it, however the changes were cut into pulls.
func TestApplyingDeltasEqualsASnapshot(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	otherOutlet, _ := b.secondOutlet("BDG1")
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))

	roles, _ := w.ids.ListRoles(ctx, b.tenant.ID)
	roleOf := map[string]uuid.UUID{}
	for _, r := range roles {
		roleOf[r.Name] = r.ID
	}

	local := newLocalCopy()
	cursor := ""
	pull := func(limit int) {
		for {
			res := b.pull(dev, cursor, limit)
			local.apply(res)
			cursor = res.Cursor
			if !res.HasMore {
				return
			}
		}
	}
	pull(0)

	var items []catalog.Item
	var cats []uuid.UUID
	var groups []catalog.ModifierGroup
	var staff []uuid.UUID
	c := b.cat()
	for step := 0; step < 150; step++ {
		switch rng.Intn(11) {
		case 0:
			cat, err := c.CreateCategory(ctx, b.tenant.ID, catalog.NewCategory{Name: fmt.Sprint("Cat ", step)})
			if err != nil {
				t.Fatal(err)
			}
			cats = append(cats, cat.ID)
		case 1:
			g, err := c.CreateModifierGroup(ctx, b.tenant.ID, catalog.NewModifierGroup{Name: fmt.Sprint("Group ", step), MaxSelect: 2, Modifiers: []catalog.NewModifier{{Name: "a"}, {Name: "b", PriceDelta: 500}}})
			if err != nil {
				t.Fatal(err)
			}
			groups = append(groups, g)
		case 2, 3:
			ni := catalog.NewItem{Name: fmt.Sprint("Item ", step), Variants: []catalog.NewVariant{{Name: "A", BasePrice: kernel.Rupiah(1000 + step)}, {Name: "B", BasePrice: 2000}}}
			if len(cats) > 0 && rng.Intn(2) == 0 {
				ni.CategoryID = &cats[rng.Intn(len(cats))]
			}
			if len(groups) > 0 && rng.Intn(2) == 0 {
				ni.ModifierGroupIDs = []uuid.UUID{groups[rng.Intn(len(groups))].ID}
			}
			it, err := c.CreateItem(ctx, b.tenant.ID, ni)
			if err != nil {
				t.Fatal(err)
			}
			items = append(items, it)
		case 4:
			if len(items) > 0 {
				it := items[rng.Intn(len(items))]
				p := kernel.Rupiah(500 + rng.Intn(9000))
				if _, err := c.UpdateVariant(ctx, b.tenant.ID, it.Variants[rng.Intn(len(it.Variants))].ID, catalog.UpdateVariant{BasePrice: &p}); err != nil {
					t.Fatal(err)
				}
			}
		case 5:
			if len(items) > 0 {
				arch := rng.Intn(2) == 0
				if _, err := c.UpdateItem(ctx, b.tenant.ID, items[rng.Intn(len(items))].ID, catalog.UpdateItem{Archived: &arch}); err != nil {
					t.Fatal(err)
				}
			}
		case 6:
			if len(items) > 0 {
				p := kernel.Rupiah(100 * (1 + rng.Intn(50)))
				outlet := b.outlet.ID
				if rng.Intn(3) == 0 {
					outlet = otherOutlet
				}
				if _, err := c.SetOutletVariant(ctx, b.tenant.ID, outlet, items[rng.Intn(len(items))].Variants[0].ID, catalog.SetOutletVariant{PriceOverride: &p, Available: rng.Intn(2) == 0}); err != nil {
					t.Fatal(err)
				}
			}
		case 7:
			if len(groups) > 0 {
				if _, err := c.AddModifier(ctx, b.tenant.ID, groups[rng.Intn(len(groups))].ID, catalog.NewModifier{Name: fmt.Sprint("m", step)}); err != nil {
					t.Fatal(err)
				}
			}
		case 8:
			st, err := w.ids.CreateStaff(ctx, b.owner, identity.NewStaff{DisplayName: fmt.Sprint("Staff ", step), OutletRoles: []identity.OutletRole{{OutletID: b.outlet.ID, RoleID: roleOf["Cashier"]}}})
			if err != nil {
				t.Fatal(err)
			}
			staff = append(staff, st.ID)
		case 9:
			if len(staff) > 0 {
				id := staff[rng.Intn(len(staff))]
				switch rng.Intn(3) {
				case 0:
					if err := w.ids.SetPIN(ctx, b.owner, id, fmt.Sprint(2000+rng.Intn(7000))); err != nil {
						t.Fatal(err)
					}
				case 1:
					active := rng.Intn(2) == 0
					if _, err := w.ids.UpdateStaff(ctx, b.owner, id, identity.UpdateStaff{Active: &active}); err != nil {
						t.Fatal(err)
					}
				default:
					role := []string{"Cashier", "Manager", "Kitchen"}[rng.Intn(3)]
					outlet := b.outlet.ID
					if rng.Intn(4) == 0 {
						outlet = otherOutlet
					}
					rs := []identity.OutletRole{{OutletID: outlet, RoleID: roleOf[role]}}
					if _, err := w.ids.UpdateStaff(ctx, b.owner, id, identity.UpdateStaff{OutletRoles: &rs}); err != nil {
						t.Fatal(err)
					}
				}
			}
		case 10:
			rate := kernel.BasisPoints(rng.Intn(1200))
			if _, err := tenancy.NewService(w.d.App).UpdateOutletSettings(ctx, b.tenant.ID, b.outlet.ID, tenancy.SettingsUpdate{TaxRate: &rate}); err != nil {
				t.Fatal(err)
			}
		}
		// Pull now and then, with a small page size so paging is exercised, not after every change.
		if rng.Intn(6) == 0 {
			pull(1 + rng.Intn(7))
		}
	}
	pull(3)

	truth := newLocalCopy()
	truth.apply(b.pull(dev, "", 0))
	if got, want := local.fingerprint(), truth.fingerprint(); got != want {
		t.Fatalf("a device applying every delta diverged from a snapshot:\n got  %s\n want %s", got, want)
	}
	if len(truth.items) < 20 || len(truth.staff) < 2 {
		t.Errorf("the scenario was too small to mean anything: %d items, %d staff", len(truth.items), len(truth.staff))
	}
}

// localCopy is what a POS keeps: the last state it was told about each entity.
type localCopy struct {
	categories map[uuid.UUID]catalog.Category
	items      map[uuid.UUID]catalog.Item
	groups     map[uuid.UUID]catalog.ModifierGroup
	overrides  map[uuid.UUID]catalog.OutletVariant
	staff      map[uuid.UUID]identity.RosterStaff
	outlet     *tenancy.Outlet
}

func newLocalCopy() *localCopy {
	return &localCopy{
		categories: map[uuid.UUID]catalog.Category{}, items: map[uuid.UUID]catalog.Item{}, groups: map[uuid.UUID]catalog.ModifierGroup{},
		overrides: map[uuid.UUID]catalog.OutletVariant{}, staff: map[uuid.UUID]identity.RosterStaff{},
	}
}

func (l *localCopy) apply(r syncsrv.PullResponse) {
	if r.Snapshot { // replace, do not merge
		*l = *newLocalCopy()
	}
	for _, c := range r.Catalog.Categories {
		l.categories[c.ID] = c
	}
	for _, it := range r.Catalog.Items {
		l.items[it.ID] = it
	}
	for _, g := range r.Catalog.Groups {
		l.groups[g.ID] = g
	}
	for _, o := range r.Catalog.OutletVariants {
		l.overrides[o.VariantID] = o
	}
	for _, s := range r.Staff {
		l.staff[s.StaffID] = s
	}
	for _, id := range r.RemovedStaff {
		delete(l.staff, id)
	}
	if r.Outlet != nil {
		o := *r.Outlet
		l.outlet = &o
	}
}

func (l *localCopy) fingerprint() string {
	sorted := func(v any) string {
		raw, _ := json.Marshal(v)
		return string(raw)
	}
	flat := func(m any) string { return sorted(m) } // encoding/json sorts map keys
	return flat(l.categories) + "|" + flat(l.items) + "|" + flat(l.groups) + "|" + flat(l.overrides) + "|" + flat(l.staff) + "|" + sorted(l.outlet)
}
