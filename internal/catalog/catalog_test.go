package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type fixture struct {
	d      *testdb.DB
	svc    *catalog.Service
	tenant uuid.UUID
	outlet uuid.UUID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	d := testdb.New(t)
	tn, o, err := tenancy.NewService(d.App).CreateTenant(context.Background(), tenancy.NewTenant{
		Name: "Kopi", Slug: "kopi", Outlet: tenancy.NewOutlet{Name: "Pusat", Code: "JKT1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{d: d, svc: catalog.NewService(d.App), tenant: tn.ID, outlet: o.ID}
}

func ptr[T any](v T) *T { return &v }

func (f fixture) item(t *testing.T, name string, price kernel.Rupiah) catalog.Item {
	t.Helper()
	it, err := f.svc.CreateItem(context.Background(), f.tenant, catalog.NewItem{
		Name: name, Variants: []catalog.NewVariant{{BasePrice: price}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// changes returns the change log entries after seq, as "type:op".
func (f fixture) changes(t *testing.T, after int64) []string {
	t.Helper()
	rows, err := f.d.Owner.Query(context.Background(),
		`SELECT entity_type || ':' || op || coalesce(':' || outlet_id::text, '') FROM change_log WHERE tenant_id = $1 AND seq > $2 ORDER BY seq`, f.tenant, after)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f fixture) seq(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.d.Owner.QueryRow(context.Background(), `SELECT change_seq FROM tenant WHERE id = $1`, f.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f fixture) audits(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := f.d.Owner.QueryRow(context.Background(),
		`SELECT count(*) FROM tenant_audit_log WHERE tenant_id = $1 AND action = $2`, f.tenant, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateItemWithVariantsAndModifiers(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cat, err := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Coffee", SortOrder: 1})
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{
		Name: "Sugar", MinSelect: 1, MaxSelect: 1, Required: true,
		Modifiers: []catalog.NewModifier{{Name: "Normal"}, {Name: "Less", PriceDelta: 0}, {Name: "Extra shot", PriceDelta: 5000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Modifiers) != 3 || g.Modifiers[0].Name != "Normal" || g.Modifiers[2].PriceDelta != 5000 {
		t.Errorf("modifiers = %+v", g.Modifiers)
	}

	before := f.seq(t)
	it, err := f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{
		CategoryID: &cat.ID, Name: " Latte ", SKU: ptr("LAT"), Barcode: ptr(" "), TrackStock: true,
		Variants:         []catalog.NewVariant{{Name: "Small", BasePrice: 25000, SKU: ptr("LAT-S")}, {Name: "Large", BasePrice: 30000}},
		ModifierGroupIDs: []uuid.UUID{g.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if it.Name != "Latte" || it.Barcode != nil || it.CategoryID == nil || *it.CategoryID != cat.ID || !it.TrackStock {
		t.Errorf("item = %+v", it)
	}
	if len(it.Variants) != 2 || it.Variants[0].Name != "Small" || it.Variants[1].Name != "Large" || it.Variants[1].BasePrice != 30000 {
		t.Errorf("variants = %+v", it.Variants)
	}
	if it.Variants[1].SKU != nil || it.Variants[0].SKU == nil || *it.Variants[0].SKU != "LAT-S" {
		t.Errorf("variant SKUs = %v, %v", it.Variants[0].SKU, it.Variants[1].SKU)
	}
	if len(it.ModifierGroupIDs) != 1 || it.ModifierGroupIDs[0] != g.ID {
		t.Errorf("groups = %v", it.ModifierGroupIDs)
	}
	// One change for the whole item, however many variants it has.
	if got := f.changes(t, before); len(got) != 1 || got[0] != "item:upsert" {
		t.Errorf("changes = %v", got)
	}
}

func TestCreateItemValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := map[string]catalog.NewItem{
		"no name":      {Variants: []catalog.NewVariant{{BasePrice: 1}}},
		"no variants":  {Name: "X"},
		"negative":     {Name: "X", Variants: []catalog.NewVariant{{BasePrice: -1}}},
		"huge":         {Name: "X", Variants: []catalog.NewVariant{{BasePrice: 2_000_000_000}}},
		"long sku":     {Name: "X", Variants: []catalog.NewVariant{{BasePrice: 1, SKU: ptr(string(make([]byte, 41)))}}},
		"unknown cat":  {Name: "X", CategoryID: ptr(kernel.NewID()), Variants: []catalog.NewVariant{{BasePrice: 1}}},
		"unknown grp":  {Name: "X", Variants: []catalog.NewVariant{{BasePrice: 1}}, ModifierGroupIDs: []uuid.UUID{kernel.NewID()}},
		"duplicate gr": {Name: "X", Variants: []catalog.NewVariant{{BasePrice: 1}}, ModifierGroupIDs: []uuid.UUID{{1}, {1}}},
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.CreateItem(ctx, f.tenant, in); !errors.Is(err, kernel.ErrValidation) {
				t.Errorf("err = %v, want validation", err)
			}
		})
	}
	// Nothing was left behind by the failed attempts.
	var n int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM item`).Scan(&n)
	if n != 0 {
		t.Errorf("%d items left behind", n)
	}
}

func TestSKUAndBarcodeAreUniqueUntilArchived(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mk := func(name string) (catalog.Item, error) {
		return f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{
			Name: name, Variants: []catalog.NewVariant{{BasePrice: 1000, SKU: ptr("A1"), Barcode: ptr("899")}},
		})
	}
	first, err := mk("One")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mk("Two"); !errors.Is(err, kernel.ErrConflict) {
		t.Fatalf("duplicate SKU: err = %v, want conflict", err)
	}
	// Archiving the variant frees its codes.
	if _, err := f.svc.UpdateVariant(ctx, f.tenant, first.Variants[0].ID, catalog.UpdateVariant{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := mk("Two"); err != nil {
		t.Fatalf("after archive: %v", err)
	}
	// Restoring it now collides with the new one.
	if _, err := f.svc.UpdateVariant(ctx, f.tenant, first.Variants[0].ID, catalog.UpdateVariant{Archived: ptr(false)}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("restore: err = %v, want conflict", err)
	}
}

func TestUpdateItemPatchSemantics(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cat, _ := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Food"})
	it, err := f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{
		CategoryID: &cat.ID, Name: "Toast", SKU: ptr("T1"), ImageURL: ptr("https://img/x.png"),
		Variants: []catalog.NewVariant{{BasePrice: 15000}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leaving fields out changes nothing.
	same, err := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{})
	if err != nil {
		t.Fatal(err)
	}
	if same.Name != "Toast" || same.SKU == nil || same.CategoryID == nil || same.ImageURL == nil {
		t.Errorf("empty patch changed the item: %+v", same)
	}

	// Blank clears an optional text; ClearCategory clears the category; a name changes.
	got, err := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{
		Name: ptr("Butter toast"), SKU: ptr(""), ClearCategory: true, TrackStock: ptr(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Butter toast" || got.SKU != nil || got.CategoryID != nil || !got.TrackStock || got.ImageURL == nil {
		t.Errorf("item = %+v", got)
	}

	if _, err := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{CategoryID: &cat.ID, ClearCategory: true}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("both category options: err = %v", err)
	}
	if _, err := f.svc.UpdateItem(ctx, f.tenant, kernel.NewID(), catalog.UpdateItem{}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown item: err = %v", err)
	}
}

func TestArchiveKeepsTheRowAndRecordsAChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Tea", 10000)

	before := f.seq(t)
	got, err := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{Archived: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchivedAt == nil {
		t.Fatal("not archived")
	}
	first := *got.ArchivedAt
	// Archiving again keeps the original time.
	again, _ := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{Archived: ptr(true)})
	if !again.ArchivedAt.Equal(first) {
		t.Errorf("archived_at moved from %v to %v", first, again.ArchivedAt)
	}
	if c := f.changes(t, before); len(c) != 2 || c[0] != "item:upsert" {
		t.Errorf("changes = %v", c)
	}

	// Hidden from the default list, present with include_archived, still readable by id.
	list, _ := f.svc.ListItems(ctx, f.tenant, kernel.Page{}, catalog.ItemFilter{})
	if len(list.Items) != 0 {
		t.Errorf("archived item listed: %+v", list.Items)
	}
	list, _ = f.svc.ListItems(ctx, f.tenant, kernel.Page{}, catalog.ItemFilter{IncludeArchived: true})
	if len(list.Items) != 1 {
		t.Errorf("include_archived list has %d items", len(list.Items))
	}
	if _, err := f.svc.GetItem(ctx, f.tenant, it.ID); err != nil {
		t.Errorf("get archived: %v", err)
	}

	restored, _ := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{Archived: ptr(false)})
	if restored.ArchivedAt != nil {
		t.Error("not restored")
	}

	// Nothing is ever deleted: the app role has no DELETE on any catalog table but the links.
	for _, table := range []string{"category", "item", "variant", "modifier_group", "modifier", "outlet_variant"} {
		_, err := f.d.App.Exec(ctx, fmt.Sprintf("DELETE FROM %s", table))
		if err == nil {
			t.Errorf("orion_app could delete from %s", table)
		}
	}
}

func TestVariantPriceChangeIsAudited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Mocha", 20000)
	v := it.Variants[0]

	if _, err := f.svc.UpdateVariant(ctx, f.tenant, v.ID, catalog.UpdateVariant{Name: ptr("Regular")}); err != nil {
		t.Fatal(err)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 0 {
		t.Errorf("a rename was audited as a price change")
	}
	if _, err := f.svc.UpdateVariant(ctx, f.tenant, v.ID, catalog.UpdateVariant{BasePrice: ptr(kernel.Rupiah(22000))}); err != nil {
		t.Fatal(err)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 1 {
		t.Errorf("audit entries = %d, want 1", n)
	}
	var detail string
	_ = f.d.Owner.QueryRow(ctx, `SELECT detail::text FROM tenant_audit_log WHERE action = 'catalog.price_changed'`).Scan(&detail)
	if detail != `{"after": 22000, "scope": "base", "before": 20000}` {
		t.Errorf("detail = %s", detail)
	}
	// Same price again: no new entry.
	_, _ = f.svc.UpdateVariant(ctx, f.tenant, v.ID, catalog.UpdateVariant{BasePrice: ptr(kernel.Rupiah(22000))})
	if n := f.audits(t, "catalog.price_changed"); n != 1 {
		t.Errorf("audit entries = %d after a no-op, want 1", n)
	}
}

func TestAddVariantKeepsOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Juice", 12000)
	before := f.seq(t)
	if _, err := f.svc.AddVariant(ctx, f.tenant, it.ID, catalog.NewVariant{Name: "Large", BasePrice: 16000}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.GetItem(ctx, f.tenant, it.ID)
	if len(got.Variants) != 2 || got.Variants[1].Name != "Large" || got.Variants[1].SortOrder != 1 {
		t.Errorf("variants = %+v", got.Variants)
	}
	// The variant change is a change to its item.
	if c := f.changes(t, before); len(c) != 1 || c[0] != "item:upsert" {
		t.Errorf("changes = %v", c)
	}
	if _, err := f.svc.AddVariant(ctx, f.tenant, kernel.NewID(), catalog.NewVariant{BasePrice: 1}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown item: err = %v", err)
	}
}

func TestModifierGroupRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := map[string]catalog.NewModifierGroup{
		"required without min": {Name: "A", MinSelect: 0, MaxSelect: 1, Required: true},
		"min above max":        {Name: "A", MinSelect: 3, MaxSelect: 2},
		"zero max":             {Name: "A", MaxSelect: 0},
		"blank modifier":       {Name: "A", MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: " "}}},
		"huge delta":           {Name: "A", MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: "x", PriceDelta: 5_000_000_000}}},
	}
	for name, in := range bad {
		if _, err := f.svc.CreateModifierGroup(ctx, f.tenant, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}

	g, err := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{
		Name: "Toppings", MinSelect: 0, MaxSelect: 3, Modifiers: []catalog.NewModifier{{Name: "Boba", PriceDelta: 4000}, {Name: "Jelly"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{Name: "toppings", MaxSelect: 1}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate name: err = %v", err)
	}
	// An update is validated against the whole group, not the field alone.
	if _, err := f.svc.UpdateModifierGroup(ctx, f.tenant, g.ID, catalog.UpdateModifierGroup{Required: ptr(true)}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("required with min 0: err = %v", err)
	}
	if _, err := f.svc.UpdateModifierGroup(ctx, f.tenant, g.ID, catalog.UpdateModifierGroup{MinSelect: ptr(1), Required: ptr(true), MaxSelect: ptr(2)}); err != nil {
		t.Errorf("valid update: %v", err)
	}
	if _, err := f.svc.UpdateModifierGroup(ctx, f.tenant, g.ID, catalog.UpdateModifierGroup{MaxSelect: ptr(1 << 40)}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("huge max: err = %v", err)
	}

	before := f.seq(t)
	m, err := f.svc.AddModifier(ctx, f.tenant, g.ID, catalog.NewModifier{Name: "Pudding", PriceDelta: 3000})
	if err != nil {
		t.Fatal(err)
	}
	if m.SortOrder != 2 {
		t.Errorf("sort order = %d, want 2", m.SortOrder)
	}
	if _, err := f.svc.UpdateModifier(ctx, f.tenant, m.ID, catalog.UpdateModifier{PriceDelta: ptr(kernel.Rupiah(3500))}); err != nil {
		t.Fatal(err)
	}
	if c := f.changes(t, before); len(c) != 2 || c[0] != "modifier_group:upsert" || c[1] != "modifier_group:upsert" {
		t.Errorf("changes = %v", c)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 1 {
		t.Errorf("audit entries = %d, want 1", n)
	}
}

func TestReplaceItemModifierGroups(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mk := func(name string) uuid.UUID {
		g, err := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{Name: name, MaxSelect: 1})
		if err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	a, b, c := mk("A"), mk("B"), mk("C")
	it := f.item(t, "Drink", 10000)

	set := func(ids ...uuid.UUID) catalog.Item {
		got, err := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{ModifierGroupIDs: &ids})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := set(c, a); len(got.ModifierGroupIDs) != 2 || got.ModifierGroupIDs[0] != c || got.ModifierGroupIDs[1] != a {
		t.Errorf("groups = %v, want [c a] in that order", got.ModifierGroupIDs)
	}
	if got := set(b); len(got.ModifierGroupIDs) != 1 || got.ModifierGroupIDs[0] != b {
		t.Errorf("groups = %v, want [b]", got.ModifierGroupIDs)
	}
	if got := set(); len(got.ModifierGroupIDs) != 0 {
		t.Errorf("groups = %v, want none", got.ModifierGroupIDs)
	}
	// Leaving the field out keeps the links.
	set(a)
	got, _ := f.svc.UpdateItem(ctx, f.tenant, it.ID, catalog.UpdateItem{Name: ptr("Drink 2")})
	if len(got.ModifierGroupIDs) != 1 {
		t.Errorf("groups lost by an unrelated update: %v", got.ModifierGroupIDs)
	}
}

func TestOutletVariantOverrideAndAvailability(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Cake", 30000)
	v := it.Variants[0].ID

	before := f.seq(t)
	got, err := f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, v, catalog.SetOutletVariant{PriceOverride: ptr(kernel.Rupiah(32000)), Available: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.PriceOverride == nil || *got.PriceOverride != 32000 || !got.Available {
		t.Errorf("got %+v", got)
	}
	// Recorded for that outlet only.
	if c := f.changes(t, before); len(c) != 1 || c[0] != "outlet_variant:upsert:"+f.outlet.String() {
		t.Errorf("changes = %v", c)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 1 {
		t.Errorf("audit = %d, want 1", n)
	}

	// Sold out, keeping the price: not a price change.
	if _, err := f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, v, catalog.SetOutletVariant{PriceOverride: ptr(kernel.Rupiah(32000)), Available: false}); err != nil {
		t.Fatal(err)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 1 {
		t.Errorf("audit = %d after availability change, want 1", n)
	}
	// Back to the base price.
	got, err = f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, v, catalog.SetOutletVariant{Available: true})
	if err != nil || got.PriceOverride != nil {
		t.Fatalf("clear: %+v, %v", got, err)
	}
	if n := f.audits(t, "catalog.price_changed"); n != 2 {
		t.Errorf("audit = %d after clearing, want 2", n)
	}

	list, err := f.svc.ListOutletVariants(ctx, f.tenant, f.outlet, kernel.Page{})
	if err != nil || len(list.Items) != 1 || list.Items[0].VariantID != v {
		t.Errorf("list = %+v, %v", list, err)
	}

	for name, call := range map[string]func() error{
		"unknown variant": func() error {
			_, err := f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, kernel.NewID(), catalog.SetOutletVariant{Available: true})
			return err
		},
		"unknown outlet": func() error {
			_, err := f.svc.SetOutletVariant(ctx, f.tenant, kernel.NewID(), v, catalog.SetOutletVariant{Available: true})
			return err
		},
	} {
		if err := call(); !errors.Is(err, kernel.ErrNotFound) {
			t.Errorf("%s: err = %v, want not found", name, err)
		}
	}
	if _, err := f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, v, catalog.SetOutletVariant{PriceOverride: ptr(kernel.Rupiah(-5))}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("negative override: err = %v", err)
	}
}

// Another business can neither see nor touch this one's catalog, whatever ids it sends.
func TestCatalogIsTenantScoped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Secret blend", 50000)
	cat, _ := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Hidden"})
	g, _ := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{Name: "G", MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: "m"}}})

	other, oo, err := tenancy.NewService(f.d.App).CreateTenant(ctx, tenancy.NewTenant{Name: "Tea", Slug: "tea", Outlet: tenancy.NewOutlet{Name: "Main", Code: "BDG1"}})
	if err != nil {
		t.Fatal(err)
	}
	b := other.ID

	if l, _ := f.svc.ListItems(ctx, b, kernel.Page{}, catalog.ItemFilter{IncludeArchived: true}); len(l.Items) != 0 {
		t.Errorf("other tenant lists %d items", len(l.Items))
	}
	if l, _ := f.svc.ListCategories(ctx, b, kernel.Page{}, true); len(l.Items) != 0 {
		t.Errorf("other tenant lists %d categories", len(l.Items))
	}
	if l, _ := f.svc.ListModifierGroups(ctx, b, kernel.Page{}, true); len(l.Items) != 0 {
		t.Errorf("other tenant lists %d groups", len(l.Items))
	}
	if _, err := f.svc.GetItem(ctx, b, it.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("get: err = %v", err)
	}
	if _, err := f.svc.UpdateItem(ctx, b, it.ID, catalog.UpdateItem{Archived: ptr(true)}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("update: err = %v", err)
	}
	if _, err := f.svc.UpdateVariant(ctx, b, it.Variants[0].ID, catalog.UpdateVariant{BasePrice: ptr(kernel.Rupiah(1))}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("update variant: err = %v", err)
	}
	if _, err := f.svc.UpdateCategory(ctx, b, cat.ID, catalog.UpdateCategory{Name: ptr("x")}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("update category: err = %v", err)
	}
	if _, err := f.svc.AddModifier(ctx, b, g.ID, catalog.NewModifier{Name: "x"}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("add modifier: err = %v", err)
	}
	if _, err := f.svc.SetOutletVariant(ctx, b, oo.ID, it.Variants[0].ID, catalog.SetOutletVariant{Available: false}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("other tenant's variant at own outlet: err = %v", err)
	}
	if _, err := f.svc.SetOutletVariant(ctx, b, f.outlet, it.Variants[0].ID, catalog.SetOutletVariant{Available: false}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("own variant at other tenant's outlet: err = %v", err)
	}
	// Creating with another tenant's category or group is a validation failure, not a link.
	if _, err := f.svc.CreateItem(ctx, b, catalog.NewItem{Name: "X", CategoryID: &cat.ID, Variants: []catalog.NewVariant{{BasePrice: 1}}}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("foreign category: err = %v", err)
	}
	if _, err := f.svc.CreateItem(ctx, b, catalog.NewItem{Name: "X", ModifierGroupIDs: []uuid.UUID{g.ID}, Variants: []catalog.NewVariant{{BasePrice: 1}}}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("foreign group: err = %v", err)
	}
	// The same SKU in two businesses is fine.
	if _, err := f.svc.CreateItem(ctx, b, catalog.NewItem{Name: "Own", Variants: []catalog.NewVariant{{BasePrice: 1, SKU: ptr("S")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{Name: "Own", Variants: []catalog.NewVariant{{BasePrice: 1, SKU: ptr("S")}}}); err != nil {
		t.Errorf("same SKU in another business: %v", err)
	}
}

// Lists make the same number of queries for 1 row as for 40: children come from one batched
// query each (BACKEND_PLAN.md section 4.12).
func TestListsDoNotQueryPerRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	g, _ := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{Name: "G", MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: "a"}}})
	add := func(n int) {
		for i := 0; i < n; i++ {
			it, err := f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{
				Name: fmt.Sprintf("Item %d", kernel.NewID().ID()), ModifierGroupIDs: []uuid.UUID{g.ID},
				Variants: []catalog.NewVariant{{BasePrice: 1000}, {Name: "L", BasePrice: 2000}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.CreateModifierGroup(ctx, f.tenant, catalog.NewModifierGroup{
				Name: "G" + it.ID.String(), MaxSelect: 1, Modifiers: []catalog.NewModifier{{Name: "x"}, {Name: "y"}},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(1)
	items := func() { _, _ = f.svc.ListItems(ctx, f.tenant, kernel.Page{}, catalog.ItemFilter{}) }
	groups := func() { _, _ = f.svc.ListModifierGroups(ctx, f.tenant, kernel.Page{}, false) }
	oneI, oneG := f.d.Queries.During(items), f.d.Queries.During(groups)
	add(39)
	manyI, manyG := f.d.Queries.During(items), f.d.Queries.During(groups)
	if oneI != manyI {
		t.Errorf("ListItems: %d queries for 2 items, %d for 40", oneI, manyI)
	}
	if oneG != manyG {
		t.Errorf("ListModifierGroups: %d queries for 2 groups, %d for 41", oneG, manyG)
	}
}

func TestListItemsPagingAndFilter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	coffee, _ := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Coffee"})
	for i := 0; i < 5; i++ {
		var cid *uuid.UUID
		if i%2 == 0 {
			cid = &coffee.ID
		}
		if _, err := f.svc.CreateItem(ctx, f.tenant, catalog.NewItem{Name: fmt.Sprint("I", i), CategoryID: cid, Variants: []catalog.NewVariant{{BasePrice: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	page := kernel.Page{Limit: 2}
	for {
		res, err := f.svc.ListItems(ctx, f.tenant, page, catalog.ItemFilter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res.Items {
			seen = append(seen, it.Name)
		}
		if res.Next == uuid.Nil {
			break
		}
		page.After = res.Next
	}
	if fmt.Sprint(seen) != "[I0 I1 I2 I3 I4]" {
		t.Errorf("paged names = %v", seen)
	}
	res, _ := f.svc.ListItems(ctx, f.tenant, kernel.Page{}, catalog.ItemFilter{CategoryID: &coffee.ID})
	if len(res.Items) != 3 {
		t.Errorf("category filter returned %d items, want 3", len(res.Items))
	}
}

func TestCategoryNamesAreUniqueWhileLive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c, err := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Drinks"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: " drinks "}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate: err = %v", err)
	}
	if _, err := f.svc.UpdateCategory(ctx, f.tenant, c.ID, catalog.UpdateCategory{Archived: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "Drinks"}); err != nil {
		t.Errorf("name of an archived category: %v", err)
	}
	if _, err := f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: "  "}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("blank: err = %v", err)
	}
}

// Writers on one tenant take the tenant lock first, so concurrent catalog edits neither deadlock
// nor leave gaps in the change numbers (BACKEND_PLAN.md section 4.12).
func TestConcurrentEditsKeepChangeNumbersGapless(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	it := f.item(t, "Shared", 1000)
	before := f.seq(t)

	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var err error
			switch i % 3 {
			case 0:
				_, err = f.svc.UpdateVariant(ctx, f.tenant, it.Variants[0].ID, catalog.UpdateVariant{BasePrice: ptr(kernel.Rupiah(1000 + i))})
			case 1:
				_, err = f.svc.SetOutletVariant(ctx, f.tenant, f.outlet, it.Variants[0].ID, catalog.SetOutletVariant{Available: i%2 == 0})
			default:
				_, err = f.svc.CreateCategory(ctx, f.tenant, catalog.NewCategory{Name: fmt.Sprint("C", i)})
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if got := f.seq(t) - before; got != n {
		t.Errorf("change_seq advanced by %d, want %d", got, n)
	}
	var rows int
	_ = f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM change_log WHERE tenant_id = $1 AND seq > $2`, f.tenant, before).Scan(&rows)
	if rows != n {
		t.Errorf("%d change_log rows, want %d", rows, n)
	}
}
