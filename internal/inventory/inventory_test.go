package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

// Rule ids (BR-...) refer to the back office's docs/BUSINESS_RULES.md.

func TestMain(m *testing.M) { testdb.Main(m) }

type fixture struct {
	d      *testdb.DB
	svc    *inventory.Service
	tenant uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	d := testdb.New(t)
	tn, _, err := tenancy.NewService(d.App).CreateTenant(context.Background(), tenancy.NewTenant{
		Name: "Kopi", Slug: "kopi", Outlet: tenancy.NewOutlet{Name: "Pusat", Code: "JKT1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{d: d, svc: inventory.NewService(d.App), tenant: tn.ID}
}

func ptr[T any](v T) *T { return &v }

func isValidation(err error) bool { return errors.Is(err, kernel.ErrValidation) }

func unit(name, symbol string, num, den int64) inventory.UnitInput {
	return inventory.UnitInput{Name: name, Symbol: symbol, RatioNum: num, RatioDen: den, RoundingScaled: 10, Active: true}
}

func ref(name, symbol string) inventory.UnitInput {
	u := unit(name, symbol, 1, 1)
	u.IsReference = true
	return u
}

// asInput is a unit's current state as a client would send it back.
func asInput(u inventory.Uom) inventory.UnitInput {
	return inventory.UnitInput{ID: &u.ID, Name: u.Name, Symbol: u.Symbol, RatioNum: u.RatioNum, RatioDen: u.RatioDen, RoundingScaled: u.RoundingScaled, Active: u.Active}
}

// standard returns the seeded categories by name and units by symbol.
func (f fixture) standard(t *testing.T) (map[string]inventory.UomCategory, map[string]inventory.Uom) {
	t.Helper()
	page, err := f.svc.ListUomCategories(context.Background(), f.tenant, kernel.Page{}, false)
	if err != nil {
		t.Fatal(err)
	}
	cats, units := map[string]inventory.UomCategory{}, map[string]inventory.Uom{}
	for _, c := range page.Items {
		cats[c.Name] = c
		for _, u := range c.Units {
			units[u.Symbol] = u
		}
	}
	return cats, units
}

// category creates a stock category to put items in.
func (f fixture) category(t *testing.T, name string) inventory.StockCategory {
	t.Helper()
	c, err := f.svc.CreateStockCategory(context.Background(), f.tenant, inventory.NewStockCategory{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestEveryBusinessStartsWithTheStandardUnits(t *testing.T) {
	f := newFixture(t)
	cats, _ := f.standard(t)
	var got []string
	for _, name := range []string{"Berat", "Volume", "Jumlah"} {
		c, ok := cats[name]
		if !ok || !c.IsStandard {
			t.Fatalf("standard category %s missing: %+v", name, cats)
		}
		for _, u := range c.Units {
			got = append(got, fmt.Sprintf("%s=%d/%d step=%d ref=%v std=%v", u.Symbol, u.RatioNum, u.RatioDen, u.RoundingScaled, u.IsReference, u.IsStandard))
		}
	}
	// BR-UOM-03, with the steps of BR-UOM-06: 0.01 kg and L, whole g, ml and pieces.
	want := []string{
		"g=1/1 step=1000 ref=true std=true", "kg=1000/1 step=10 ref=false std=true",
		"ml=1/1 step=1000 ref=true std=true", "L=1000/1 step=10 ref=false std=true",
		"pcs=1/1 step=1000 ref=true std=true", "lusin=12/1 step=1000 ref=false std=true", "kodi=20/1 step=1000 ref=false std=true",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("standard units =\n%v\nwant\n%v", got, want)
	}
}

func TestStandardUnitsKeepTheirMeaning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cats, units := f.standard(t)
	weight := cats["Berat"]

	kgRatio, kgSymbol, kgOff := asInput(units["kg"]), asInput(units["kg"]), asInput(units["kg"])
	kgRatio.RatioNum = 1001
	kgSymbol.Symbol = "kilo"
	kgOff.Active = false
	for name, u := range map[string]inventory.UnitInput{"ratio": kgRatio, "symbol": kgSymbol, "deactivate": kgOff} {
		if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, weight.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{u}}); !isValidation(err) {
			t.Errorf("changing a standard unit's %s: %v", name, err)
		}
	}
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, weight.ID, inventory.UpdateUomCategory{Archived: ptr(true)}); !isValidation(err) {
		t.Errorf("archiving a standard category: %v", err)
	}
	// It can be renamed, and owners add their own units to it ("ons" = 100 g).
	kgName := asInput(units["kg"])
	kgName.Name = "Kilogram"
	got, err := f.svc.UpdateUomCategory(ctx, f.tenant, weight.ID, inventory.UpdateUomCategory{Name: ptr("Berat bersih"), Units: []inventory.UnitInput{kgName, unit("ons", "ons", 100, 1)}})
	if err != nil || got.Name != "Berat bersih" || len(got.Units) != 3 || got.Units[1].Symbol != "ons" || got.Units[1].IsStandard || got.Units[2].Name != "Kilogram" {
		t.Errorf("renaming and adding: %+v, %v", got, err)
	}
}

func TestUnitCategoryRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refHalf := ref("butir", "btr")
	refHalf.RatioNum = 2
	noSymbol := ref("butir", "  ")
	withID := ref("butir", "btr")
	withID.ID = ptr(uuid.New())
	for name, units := range map[string][]inventory.UnitInput{
		"no reference":      {unit("butir", "btr", 1, 1)},
		"two references":    {ref("butir", "btr"), ref("biji", "bj")},
		"no units":          nil,
		"reference at 2/1":  {refHalf},
		"no symbol":         {noSymbol},
		"one-letter name":   {ref("b", "b")},
		"zero ratio":        {ref("butir", "btr"), unit("tray", "try", 0, 1)},
		"ratio too large":   {ref("butir", "btr"), unit("tray", "try", 1_000_000_001, 1)},
		"zero step":         {ref("butir", "btr"), {Name: "tray", Symbol: "try", RatioNum: 30, RatioDen: 1, Active: true}},
		"a new unit has id": {withID},
	} {
		if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Telur " + name, Units: units}); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	// Ratios are reduced; the reference comes first, then small to large.
	c, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Telur", Units: []inventory.UnitInput{unit("tray", "tray", 60, 2), ref(" butir ", "btr"), unit("setengah", "1/2", 1, 2)}})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range c.Units {
		got = append(got, fmt.Sprintf("%s:%s=%d/%d", u.Name, u.Symbol, u.RatioNum, u.RatioDen))
	}
	if want := "[butir:btr=1/1 setengah:1/2=1/2 tray:tray=30/1]"; fmt.Sprint(got) != want {
		t.Errorf("units = %v, want %s", got, want)
	}
	// BR-UOM-02: symbols are unique in the business, ignoring case, so a second "KG" is refused,
	// and nothing of the failed category is left behind.
	if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Karung", Units: []inventory.UnitInput{ref("karung", "KG")}}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a duplicate symbol: %v", err)
	}
	if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Dup", Units: []inventory.UnitInput{ref("biji", "bj"), unit("BIJI", "bj2", 1, 1)}}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a duplicate unit name: %v", err)
	}
	var n int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM uom_category WHERE name IN ('Karung', 'Dup')`).Scan(&n)
	if n != 0 {
		t.Errorf("failed creates left %d categories", n)
	}
	// The reference keeps 1/1 and stays active; an id of another category is refused.
	b := asInput(c.Units[0])
	b.RatioNum = 2
	foreign := unit("x unit", "xu", 1, 1)
	foreign.ID = ptr(uuid.New())
	for name, u := range map[string]inventory.UnitInput{"reference ratio": b, "second reference": ref("telur", "tlr"), "foreign id": foreign} {
		if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{u}}); !isValidation(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, uuid.New(), inventory.UpdateUomCategory{Name: ptr("xx")}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("a missing category: %v", err)
	}
}

// BR-UOM-07: a unit an item uses keeps its ratio; one nobody uses may still be corrected.
func TestAUnitInUseKeepsItsRatio(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, units := f.standard(t)
	c, err := f.svc.UpdateUomCategory(ctx, f.tenant, func() uuid.UUID { cats, _ := f.standard(t); return cats["Berat"].ID }(), inventory.UpdateUomCategory{
		Units: []inventory.UnitInput{unit("sendok", "sdm", 15, 1), unit("cangkir", "cup", 120, 1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var spoon, cup inventory.Uom
	for _, u := range c.Units {
		switch u.Symbol {
		case "sdm":
			spoon = u
		case "cup":
			cup = u
		}
	}
	cat := f.category(t, "Bumbu")
	if _, err := f.svc.CreateStockItem(ctx, f.tenant, inventory.NewStockItem{
		Name: "Gula", Type: inventory.TypeIngredient, CategoryID: cat.ID, BaseUomID: units["g"].ID, RecipeUomID: &spoon.ID, Track: true,
	}); err != nil {
		t.Fatal(err)
	}
	s := asInput(spoon)
	s.RatioNum = 12
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{s}}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("changing the ratio of a recipe unit: %v", err)
	}
	s.RatioNum, s.Name, s.Active = 15, "sendok makan", false // same ratio: a rename and deactivation are fine
	cu := asInput(cup)
	cu.RatioNum = 125
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{s, cu}}); err != nil {
		t.Errorf("renaming a used unit and correcting an unused one: %v", err)
	}
}

func TestStockItemRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, units := f.standard(t)
	g, kg, ml, pcs := units["g"], units["kg"], units["ml"], units["pcs"]
	kopi := f.category(t, "Kopi")
	old := f.category(t, "Lama")
	if _, err := f.svc.UpdateStockCategory(ctx, f.tenant, old.ID, inventory.UpdateStockCategory{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	item := func(mod func(*inventory.NewStockItem)) inventory.NewStockItem {
		in := inventory.NewStockItem{Name: "Biji kopi", Type: inventory.TypeIngredient, CategoryID: kopi.ID, BaseUomID: g.ID, Track: true}
		mod(&in)
		return in
	}
	for name, in := range map[string]inventory.NewStockItem{
		"no type":                  item(func(i *inventory.NewStockItem) { i.Type = "equipment" }),
		"one-letter name":          item(func(i *inventory.NewStockItem) { i.Name = " B " }),
		"no category":              item(func(i *inventory.NewStockItem) { i.CategoryID = uuid.Nil }),
		"archived category":        item(func(i *inventory.NewStockItem) { i.CategoryID = old.ID }),
		"base not a reference":     item(func(i *inventory.NewStockItem) { i.BaseUomID = kg.ID }),
		"unknown base":             item(func(i *inventory.NewStockItem) { i.BaseUomID = uuid.New() }),
		"recipe in other category": item(func(i *inventory.NewStockItem) { i.RecipeUomID = &ml.ID }),
		"min stock off the step":   item(func(i *inventory.NewStockItem) { i.MinStockScaled = ptr(int64(1500)) }), // 1.5 g, step 1 g
		"min stock untracked":      item(func(i *inventory.NewStockItem) { i.Track, i.MinStockScaled = false, ptr(int64(2000)) }),
		"shelf life too long":      item(func(i *inventory.NewStockItem) { i.ShelfLifeDays = ptr(3651) }),
		"pack ratio zero": item(func(i *inventory.NewStockItem) {
			i.Packs = []inventory.PackInput{{Name: "karung", RatioDen: 1, RoundingScaled: 1000}}
		}),
	} {
		if _, err := f.svc.CreateStockItem(ctx, f.tenant, in); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}

	beans, err := f.svc.CreateStockItem(ctx, f.tenant, item(func(i *inventory.NewStockItem) {
		i.RecipeUomID, i.MinStockScaled, i.ShelfLifeDays, i.Description = &g.ID, ptr(int64(500_000)), ptr(90), ptr("  Arabika ")
		i.Packs = []inventory.PackInput{
			{Name: "karung 25 kg", RatioNum: 25_000, RatioDen: 1, RoundingScaled: 1000, Active: true},
			{Name: "sak 1 kg", RatioNum: 2000, RatioDen: 2, RoundingScaled: 1000, Active: true},
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if *beans.Description != "Arabika" || *beans.MinStockScaled != 500_000 || *beans.ShelfLifeDays != 90 || len(beans.Packs) != 2 ||
		beans.Packs[0].Name != "sak 1 kg" || beans.Packs[0].RatioNum != 1000 || beans.Packs[1].RatioNum != 25_000 {
		t.Fatalf("item = %+v", beans)
	}

	// A pack keeps its ratio (BR-UOM-07); its name, step and state may change, and packs are added.
	sak := beans.Packs[0]
	changed := inventory.PackInput{ID: &sak.ID, Name: sak.Name, RatioNum: 1001, RatioDen: 1, RoundingScaled: 1000, Active: true}
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{Packs: []inventory.PackInput{changed}}); !isValidation(err) {
		t.Errorf("changing a pack's ratio: %v", err)
	}
	changed.RatioNum, changed.Active = 1000, false
	beans, err = f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{Packs: []inventory.PackInput{changed, {Name: "toples", RatioNum: 500, RatioDen: 1, RoundingScaled: 1000, Active: true}}})
	if err != nil || len(beans.Packs) != 3 || beans.Packs[0].Name != "toples" || beans.Packs[1].Active {
		t.Fatalf("pack changes: %+v, %v", beans.Packs, err)
	}

	// The base unit cannot change while packs measure in it; recipe units must follow the base.
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{BaseUomID: &ml.ID, ClearRecipeUom: true}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("changing the base unit of an item with packs: %v", err)
	}
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{RecipeUomID: &pcs.ID}); !isValidation(err) {
		t.Errorf("a recipe unit of another category: %v", err)
	}
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{CategoryID: &old.ID}); !isValidation(err) {
		t.Errorf("moving to an archived category: %v", err)
	}
	milk, err := f.svc.CreateStockItem(ctx, f.tenant, item(func(i *inventory.NewStockItem) { i.Name = "Susu" }))
	if err != nil {
		t.Fatal(err)
	}
	milk, err = f.svc.UpdateStockItem(ctx, f.tenant, milk.ID, inventory.UpdateStockItem{BaseUomID: &ml.ID, MinStockScaled: ptr(int64(1_000_000))})
	if err != nil || milk.BaseUomID != ml.ID || *milk.MinStockScaled != 1_000_000 {
		t.Errorf("moving an item without packs to ml: %+v, %v", milk, err)
	}

	// Turning tracking off clears the minimum stock; zeros clear the optional numbers.
	beans, err = f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{Track: ptr(false), ShelfLifeDays: ptr(0), Description: ptr(" ")})
	if err != nil || beans.Track || beans.MinStockScaled != nil || beans.ShelfLifeDays != nil || beans.Description != nil {
		t.Errorf("after clearing: %+v, %v", beans, err)
	}
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{MinStockScaled: ptr(int64(1000))}); !isValidation(err) {
		t.Errorf("a minimum stock for an untracked item: %v", err)
	}

	// BR-GEN-10: names are unique ignoring case and spaces among live items; archiving frees one.
	if _, err := f.svc.CreateStockItem(ctx, f.tenant, item(func(i *inventory.NewStockItem) { i.Name = " BIJI KOPI " })); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a duplicate name: %v", err)
	}
	if _, err := f.svc.UpdateStockItem(ctx, f.tenant, beans.ID, inventory.UpdateStockItem{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	cups, err := f.svc.CreateStockItem(ctx, f.tenant, item(func(i *inventory.NewStockItem) {
		i.Name, i.Type, i.BaseUomID = "biji kopi", inventory.TypeSupply, pcs.ID
	}))
	if err != nil {
		t.Fatalf("an archived item's name should be free: %v", err)
	}

	page, err := f.svc.ListStockItems(ctx, f.tenant, kernel.Page{}, inventory.ItemFilter{Type: ptr(inventory.TypeSupply)})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != cups.ID {
		t.Errorf("supplies: %+v, %v", page.Items, err)
	}
	page, err = f.svc.ListStockItems(ctx, f.tenant, kernel.Page{}, inventory.ItemFilter{IncludeArchived: true, CategoryID: &kopi.ID})
	if err != nil || len(page.Items) != 3 {
		t.Errorf("all of Kopi: %d items, %v", len(page.Items), err)
	}
}

func TestExpenseTypesAndStockCategories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// BR-EXP-02: a debt payment is a payment status, not an expense group.
	if _, err := f.svc.CreateExpenseType(ctx, f.tenant, inventory.NewExpenseType{Name: "Bayar hutang", Group: "debt_payment"}); !isValidation(err) {
		t.Errorf("debt_payment as a group: %v", err)
	}
	et, err := f.svc.CreateExpenseType(ctx, f.tenant, inventory.NewExpenseType{Name: "Belanja Bahan Pasar", Group: inventory.GroupCostOfGoods, Description: ptr("harian")})
	if err != nil {
		t.Fatal(err)
	}
	et, err = f.svc.UpdateExpenseType(ctx, f.tenant, et.ID, inventory.UpdateExpenseType{Group: ptr(inventory.GroupOperating)})
	if err != nil || et.Group != inventory.GroupOperating || et.Name != "Belanja Bahan Pasar" || *et.Description != "harian" {
		t.Errorf("after update: %+v, %v", et, err)
	}

	cat, err := f.svc.CreateStockCategory(ctx, f.tenant, inventory.NewStockCategory{Name: "Sayur", DefaultExpenseTypeID: &et.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateExpenseType(ctx, f.tenant, et.ID, inventory.UpdateExpenseType{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	// An archived expense type leaves the pickers (BR-GEN-06), and a category keeps the one it had.
	if _, err := f.svc.CreateStockCategory(ctx, f.tenant, inventory.NewStockCategory{Name: "Daging", DefaultExpenseTypeID: &et.ID}); !isValidation(err) {
		t.Errorf("an archived default: %v", err)
	}
	cat, err = f.svc.UpdateStockCategory(ctx, f.tenant, cat.ID, inventory.UpdateStockCategory{Name: ptr("Sayur segar")})
	if err != nil || *cat.DefaultExpenseTypeID != et.ID {
		t.Errorf("a category keeps its archived default: %+v, %v", cat, err)
	}
	cat, err = f.svc.UpdateStockCategory(ctx, f.tenant, cat.ID, inventory.UpdateStockCategory{ClearDefaultExpenseType: true})
	if err != nil || cat.DefaultExpenseTypeID != nil {
		t.Errorf("clearing the default: %+v, %v", cat, err)
	}
	if _, err := f.svc.CreateStockCategory(ctx, f.tenant, inventory.NewStockCategory{Name: "SAYUR SEGAR"}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a duplicate name: %v", err)
	}
}

// The database holds the rules even against the app role.
func TestTheDatabaseKeepsUnitsHonest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, units := f.standard(t)
	cat := f.category(t, "Kopi")
	it, err := f.svc.CreateStockItem(ctx, f.tenant, inventory.NewStockItem{
		Name: "Beras", Type: inventory.TypeIngredient, CategoryID: cat.ID, BaseUomID: units["g"].ID, Track: true,
		Packs: []inventory.PackInput{{Name: "karung", RatioNum: 25000, RatioDen: 1, RoundingScaled: 1000, Active: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kg, pack := units["kg"].ID, it.Packs[0].ID
	for name, c := range map[string]struct {
		stmt string
		id   uuid.UUID
	}{
		"delete a unit":          {`DELETE FROM uom WHERE id = $1`, kg},
		"move a unit":            {`UPDATE uom SET category_id = gen_random_uuid() WHERE id = $1`, kg},
		"make a reference":       {`UPDATE uom SET is_reference = true WHERE id = $1`, kg},
		"unmark standard":        {`UPDATE uom SET is_standard = false WHERE id = $1`, kg},
		"unreduced ratio":        {`UPDATE uom SET ratio_num = 2000, ratio_den = 2 WHERE id = $1`, kg},
		"deactivate a standard":  {`UPDATE uom SET active = false WHERE id = $1`, kg},
		"archive a standard":     {`UPDATE uom_category SET archived_at = now() WHERE id = (SELECT category_id FROM uom WHERE id = $1)`, kg},
		"change a pack's ratio":  {`UPDATE stock_item_pack SET ratio_num = 30000 WHERE id = $1`, pack},
		"delete a pack":          {`DELETE FROM stock_item_pack WHERE id = $1`, pack},
		"untracked with minimum": {`UPDATE stock_item SET track = false, min_stock_scaled = 1000 WHERE id = (SELECT stock_item_id FROM stock_item_pack WHERE id = $1)`, pack},
	} {
		err := kernel.TenantTx(ctx, f.d.App, f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, c.stmt, c.id)
			return err
		})
		if err == nil {
			t.Errorf("%s: the app role was allowed", name)
		}
	}
}

func TestListsUseAFixedNumberOfQueries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, units := f.standard(t)
	cat := f.category(t, "Kopi")
	add := func(n int) {
		for i := 0; i < n; i++ {
			id := uuid.NewString()[:8]
			if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{
				Name: "C" + id, Units: []inventory.UnitInput{ref("a"+id, "a"+id), unit("b"+id, "b"+id, 10, 1)},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.CreateStockItem(ctx, f.tenant, inventory.NewStockItem{
				Name: "I" + id, Type: inventory.TypeIngredient, CategoryID: cat.ID, BaseUomID: units["g"].ID, Track: true,
				Packs: []inventory.PackInput{{Name: "pk", RatioNum: 10, RatioDen: 1, RoundingScaled: 1000, Active: true}, {Name: "box", RatioNum: 100, RatioDen: 1, RoundingScaled: 1000, Active: true}},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	uoms := func() { _, _ = f.svc.ListUomCategories(ctx, f.tenant, kernel.Page{}, false) }
	items := func() { _, _ = f.svc.ListStockItems(ctx, f.tenant, kernel.Page{}, inventory.ItemFilter{}) }
	add(1)
	oneU, oneI := f.d.Queries.During(uoms), f.d.Queries.During(items)
	add(29)
	if manyU, manyI := f.d.Queries.During(uoms), f.d.Queries.During(items); oneU != manyU || oneI != manyI {
		t.Errorf("unit categories: %d then %d queries; stock items: %d then %d", oneU, manyU, oneI, manyI)
	}
}
