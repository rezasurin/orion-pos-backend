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

func unit(name string, num, den int64) inventory.UnitInput {
	return inventory.UnitInput{Name: name, RatioNum: num, RatioDen: den, RoundingScaled: 10, Active: true}
}

func ref(name string) inventory.UnitInput {
	u := unit(name, 1, 1)
	u.IsReference = true
	return u
}

// weight creates a gram-based weight category: g, kg, a 25 kg sack and a tenth of a gram.
func (f fixture) weight(t *testing.T) inventory.UomCategory {
	t.Helper()
	c, err := f.svc.CreateUomCategory(context.Background(), f.tenant, inventory.NewUomCategory{
		Name: "Berat", Units: []inventory.UnitInput{unit("karung 25kg", 25000, 1), unit("kg", 2000, 2), ref("g"), unit("dg", 1, 10)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func byName(c inventory.UomCategory, name string) inventory.Uom {
	for _, u := range c.Units {
		if u.Name == name {
			return u
		}
	}
	panic("no unit " + name)
}

func isValidation(err error) bool { return errors.Is(err, kernel.ErrValidation) }

func TestUnitCategoryReducesRatiosAndOrdersUnits(t *testing.T) {
	f := newFixture(t)
	c := f.weight(t)
	var got []string
	for _, u := range c.Units {
		got = append(got, fmt.Sprintf("%s=%d/%d ref=%v", u.Name, u.RatioNum, u.RatioDen, u.IsReference))
	}
	want := []string{"g=1/1 ref=true", "dg=1/10 ref=false", "kg=1000/1 ref=false", "karung 25kg=25000/1 ref=false"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("units = %v, want %v (reference first, then small to large, ratios reduced)", got, want)
	}

	// The list returns the same.
	page, err := f.svc.ListUomCategories(context.Background(), f.tenant, kernel.Page{}, false)
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Units) != 4 || page.Items[0].Units[2].RatioNum != 1000 {
		t.Fatalf("list = %+v, %v", page, err)
	}
}

func TestUnitCategoryNeedsExactlyOneReferenceAtOneToOne(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	refHalf := ref("g")
	refHalf.RatioNum = 2
	refOff := ref("g")
	refOff.Active = false
	withID := ref("g")
	withID.ID = ptr(uuid.New())
	for name, units := range map[string][]inventory.UnitInput{
		"none":             {unit("g", 1, 1)},
		"two":              {ref("g"), ref("gram")},
		"no units":         nil,
		"reference at 2/1": {refHalf},
		"reference off":    {refOff},
		"zero ratio":       {ref("g"), unit("kg", 0, 1)},
		"ratio too large":  {ref("g"), unit("ton", 1_000_000_001, 1)},
		"zero rounding":    {ref("g"), {Name: "kg", RatioNum: 1000, RatioDen: 1, Active: true}},
		"blank unit name":  {ref("g"), unit("  ", 1000, 1)},
		"new unit with id": {withID},
	} {
		if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "X " + name, Units: units}); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	// 2/2 reduces to 1/1, so it is a valid reference.
	r := ref("g")
	r.RatioNum, r.RatioDen = 7, 7
	if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Ok", Units: []inventory.UnitInput{r}}); err != nil {
		t.Errorf("7/7 reference: %v", err)
	}
	// Unit names are unique within a category, ignoring case, and nothing is left behind.
	if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Dup", Units: []inventory.UnitInput{ref("g"), unit("G", 1, 1)}}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate unit name: %v", err)
	}
	var n int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM uom_category WHERE name = 'Dup'`).Scan(&n)
	if n != 0 {
		t.Errorf("a failed create left %d categories", n)
	}
}

func TestUpdateUnitCategoryAddsAndChangesUnitsButKeepsTheReference(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.weight(t)
	g, kg := byName(c, "g"), byName(c, "kg")

	ons := unit("ons", 300, 3)
	kgOff := unit("kilo", 1000, 1)
	kgOff.ID, kgOff.Active = &kg.ID, false
	got, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Name: ptr("Weight"), Units: []inventory.UnitInput{kgOff, ons}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Weight" || len(got.Units) != 5 {
		t.Fatalf("category = %+v", got)
	}
	if k := byName(got, "kilo"); k.ID != kg.ID || k.Active {
		t.Errorf("kg was not renamed and deactivated: %+v", k)
	}
	if o := byName(got, "ons"); o.RatioNum != 100 || o.RatioDen != 1 || o.IsReference {
		t.Errorf("ons = %+v, want 100/1", o)
	}

	gHalf := unit("g", 2, 1)
	gHalf.ID = &g.ID
	gOff := unit("g", 1, 1)
	gOff.ID, gOff.Active = &g.ID, false
	gRenamed := unit("gram", 1, 1)
	gRenamed.ID = &g.ID
	newRef := ref("mg")
	foreign := unit("x", 1, 1)
	foreign.ID = ptr(uuid.New())
	for name, u := range map[string]inventory.UnitInput{"reference ratio": gHalf, "reference off": gOff, "second reference": newRef, "unit of another category": foreign} {
		if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{u}}); !isValidation(err) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	got, err = f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{gRenamed}})
	if err != nil || byName(got, "gram").ID != g.ID || !byName(got, "gram").IsReference {
		t.Errorf("renaming the reference: %+v, %v", got, err)
	}
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, uuid.New(), inventory.UpdateUomCategory{Name: ptr("x")}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("missing category: %v", err)
	}
}

func TestIngredientNeedsAUsableUnit(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.weight(t)
	g := byName(c, "g")

	tt, err := f.svc.CreateTransactionType(ctx, f.tenant, inventory.NewTransactionType{Name: "Belanja Bahan Pasar", Category: inventory.TxMaterials})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := f.svc.CreateIngredientCategory(ctx, f.tenant, inventory.NewIngredientCategory{Name: "Bahan Kopi", DefaultTransactionTypeID: &tt.ID})
	if err != nil {
		t.Fatal(err)
	}
	beans, err := f.svc.CreateIngredient(ctx, f.tenant, inventory.NewIngredient{Name: "Biji kopi", CategoryID: &cat.ID, UomID: byName(c, "kg").ID, Track: true, Description: ptr("  Arabika  ")})
	if err != nil {
		t.Fatal(err)
	}
	if *beans.Description != "Arabika" || *beans.CategoryID != cat.ID || !beans.Track {
		t.Errorf("ingredient = %+v", beans)
	}

	// An inactive unit, a unit of an archived category, or no such unit cannot be given.
	dg := byName(c, "dg")
	dg.Active = false
	off := inventory.UnitInput{ID: &dg.ID, Name: dg.Name, RatioNum: dg.RatioNum, RatioDen: dg.RatioDen, RoundingScaled: dg.RoundingScaled, Active: false}
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, c.ID, inventory.UpdateUomCategory{Units: []inventory.UnitInput{off}}); err != nil {
		t.Fatal(err)
	}
	vol, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{Name: "Volume", Units: []inventory.UnitInput{ref("ml")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateUomCategory(ctx, f.tenant, vol.ID, inventory.UpdateUomCategory{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"inactive": dg.ID, "archived category": vol.Units[0].ID, "missing": uuid.New()} {
		if _, err := f.svc.CreateIngredient(ctx, f.tenant, inventory.NewIngredient{Name: "Gula " + name, UomID: id}); !isValidation(err) {
			t.Errorf("create with %s unit: %v", name, err)
		}
		if _, err := f.svc.UpdateIngredient(ctx, f.tenant, beans.ID, inventory.UpdateIngredient{UomID: &id}); !isValidation(err) {
			t.Errorf("update to %s unit: %v", name, err)
		}
	}
	// An ingredient keeps a unit that is deactivated after it was given.
	if _, err := f.svc.UpdateIngredient(ctx, f.tenant, beans.ID, inventory.UpdateIngredient{UomID: &g.ID}); err != nil {
		t.Fatal(err)
	}

	// Clearing: a blank description and clear_category; archiving frees the name.
	beans, err = f.svc.UpdateIngredient(ctx, f.tenant, beans.ID, inventory.UpdateIngredient{Description: ptr(" "), ClearCategory: true, Track: ptr(false), Archived: ptr(true)})
	if err != nil || beans.Description != nil || beans.CategoryID != nil || beans.Track || beans.ArchivedAt == nil || beans.UomID != g.ID {
		t.Fatalf("after clearing: %+v, %v", beans, err)
	}
	if _, err := f.svc.CreateIngredient(ctx, f.tenant, inventory.NewIngredient{Name: "biji KOPI", UomID: g.ID}); err != nil {
		t.Errorf("an archived ingredient's name should be free: %v", err)
	}
	if _, err := f.svc.CreateIngredient(ctx, f.tenant, inventory.NewIngredient{Name: "Biji Kopi", UomID: g.ID}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a live duplicate name: %v", err)
	}
	if _, err := f.svc.UpdateIngredient(ctx, f.tenant, beans.ID, inventory.UpdateIngredient{Archived: ptr(false)}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("restoring onto a live name: %v", err)
	}
	if _, err := f.svc.CreateIngredient(ctx, f.tenant, inventory.NewIngredient{Name: "Susu", UomID: g.ID, CategoryID: ptr(uuid.New())}); !isValidation(err) {
		t.Errorf("a missing ingredient category: %v", err)
	}

	page, err := f.svc.ListIngredients(ctx, f.tenant, kernel.Page{}, true, &cat.ID)
	if err != nil || len(page.Items) != 0 {
		t.Errorf("the category's ingredients after clearing: %+v, %v", page.Items, err)
	}
	page, err = f.svc.ListIngredients(ctx, f.tenant, kernel.Page{}, false, nil)
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "biji KOPI" {
		t.Errorf("live ingredients: %+v, %v", page.Items, err)
	}
}

func TestTransactionTypesAndIngredientCategories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.CreateTransactionType(ctx, f.tenant, inventory.NewTransactionType{Name: "Belanja", Category: "groceries"}); !isValidation(err) {
		t.Errorf("unknown category: %v", err)
	}
	tt, err := f.svc.CreateTransactionType(ctx, f.tenant, inventory.NewTransactionType{Name: "Servis AC", Category: inventory.TxServices, Description: ptr("tiap bulan")})
	if err != nil {
		t.Fatal(err)
	}
	tt, err = f.svc.UpdateTransactionType(ctx, f.tenant, tt.ID, inventory.UpdateTransactionType{Category: ptr(inventory.TxDebtPayment)})
	if err != nil || tt.Category != inventory.TxDebtPayment || tt.Name != "Servis AC" || *tt.Description != "tiap bulan" {
		t.Errorf("after update: %+v, %v", tt, err)
	}
	if _, err := f.svc.UpdateTransactionType(ctx, f.tenant, tt.ID, inventory.UpdateTransactionType{Category: ptr("x")}); !isValidation(err) {
		t.Errorf("unknown category on update: %v", err)
	}

	cat, err := f.svc.CreateIngredientCategory(ctx, f.tenant, inventory.NewIngredientCategory{Name: "Sayur", DefaultTransactionTypeID: &tt.ID})
	if err != nil {
		t.Fatal(err)
	}
	cat, err = f.svc.UpdateIngredientCategory(ctx, f.tenant, cat.ID, inventory.UpdateIngredientCategory{ClearDefaultTransactionType: true})
	if err != nil || cat.DefaultTransactionTypeID != nil || cat.Name != "Sayur" {
		t.Errorf("after clearing the default: %+v, %v", cat, err)
	}
	if _, err := f.svc.UpdateIngredientCategory(ctx, f.tenant, cat.ID, inventory.UpdateIngredientCategory{DefaultTransactionTypeID: ptr(uuid.New())}); !isValidation(err) {
		t.Errorf("a missing transaction type: %v", err)
	}
	if _, err := f.svc.CreateIngredientCategory(ctx, f.tenant, inventory.NewIngredientCategory{Name: "SAYUR"}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("a duplicate name: %v", err)
	}
}

// The database holds the rules even against the app role: no deletes, no moving a unit to another
// category or making it the reference, only reduced ratios, one reference per category.
func TestTheDatabaseKeepsUnitsHonest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.weight(t)
	kg := byName(c, "kg")
	for name, stmt := range map[string]string{
		"delete":           `DELETE FROM uom WHERE id = $1`,
		"move":             `UPDATE uom SET category_id = gen_random_uuid() WHERE id = $1`,
		"make reference":   `UPDATE uom SET is_reference = true WHERE id = $1`,
		"unreduced ratio":  `UPDATE uom SET ratio_num = 2000, ratio_den = 2 WHERE id = $1`,
		"delete category":  `DELETE FROM uom_category WHERE id = (SELECT category_id FROM uom WHERE id = $1)`,
		"second reference": `INSERT INTO uom (id, tenant_id, category_id, name, is_reference, ratio_num, ratio_den, rounding_scaled) SELECT gen_random_uuid(), tenant_id, category_id, 'mg', true, 1, 1, 1 FROM uom WHERE id = $1`,
	} {
		err := kernel.TenantTx(ctx, f.d.App, f.tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, kg.ID)
			return err
		})
		if err == nil {
			t.Errorf("%s: the app role was allowed", name)
		}
	}
}

func TestListUomCategoriesUsesAFixedNumberOfQueries(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	add := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := f.svc.CreateUomCategory(ctx, f.tenant, inventory.NewUomCategory{
				Name: "C" + uuid.NewString(), Units: []inventory.UnitInput{ref("a"), unit("b", 10, 1), unit("c", 100, 1)},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	list := func() { _, _ = f.svc.ListUomCategories(ctx, f.tenant, kernel.Page{}, false) }
	add(1)
	one := f.d.Queries.During(list)
	add(29)
	if many := f.d.Queries.During(list); one != many {
		t.Errorf("ListUomCategories: %d queries for 1 category, %d for 30", one, many)
	}
}
