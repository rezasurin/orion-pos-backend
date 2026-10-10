package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type variantBody struct {
	ID        string  `json:"id"`
	ItemID    string  `json:"item_id"`
	Name      string  `json:"name"`
	SKU       *string `json:"sku"`
	BasePrice int64   `json:"base_price"`
	SortOrder int     `json:"sort_order"`
	Archived  *string `json:"archived_at"`
}

type itemBody struct {
	ID               string        `json:"id"`
	CategoryID       *string       `json:"category_id"`
	Name             string        `json:"name"`
	TrackStock       bool          `json:"track_stock"`
	ArchivedAt       *string       `json:"archived_at"`
	Variants         []variantBody `json:"variants"`
	ModifierGroupIDs []string      `json:"modifier_group_ids"`
}

type modifierBody struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceDelta int64  `json:"price_delta"`
}

type groupBody struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	MinSelect int            `json:"min_select"`
	MaxSelect int            `json:"max_select"`
	Required  bool           `json:"required"`
	Modifiers []modifierBody `json:"modifiers"`
}

type categoryBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type outletVariantBody struct {
	OutletID      string `json:"outlet_id"`
	VariantID     string `json:"variant_id"`
	PriceOverride *int64 `json:"price_override"`
	Available     bool   `json:"available"`
}

func (e *env) create(t *testing.T, path, token string, body any, into any) {
	t.Helper()
	r := e.do(t, "POST", path, token, body)
	if r.Code != http.StatusCreated {
		t.Fatalf("POST %s: %d %s", path, r.Code, r.Body.String())
	}
	r.decode(t, into)
}

func TestCatalogEndToEnd(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken

	var cat categoryBody
	e.create(t, "/v1/categories", tok, map[string]any{"name": "Coffee", "sort_order": 2}, &cat)
	var grp groupBody
	e.create(t, "/v1/modifier-groups", tok, map[string]any{
		"name": "Sugar", "min_select": 1, "required": true,
		"modifiers": []map[string]any{{"name": "Normal"}, {"name": "Extra shot", "price_delta": 5000}},
	}, &grp)
	if grp.MaxSelect != 1 || !grp.Required || len(grp.Modifiers) != 2 || grp.Modifiers[1].PriceDelta != 5000 {
		t.Fatalf("group = %+v", grp)
	}

	var it itemBody
	e.create(t, "/v1/items", tok, map[string]any{
		"name": "Latte", "category_id": cat.ID, "sku": "LAT", "track_stock": true, "modifier_group_ids": []string{grp.ID},
		"variants": []map[string]any{{"name": "Small", "base_price": 25000}, {"name": "Large", "base_price": 30000, "sku": "LAT-L"}},
	}, &it)
	if it.Name != "Latte" || !it.TrackStock || len(it.Variants) != 2 || it.Variants[1].BasePrice != 30000 ||
		len(it.ModifierGroupIDs) != 1 || it.CategoryID == nil || *it.CategoryID != cat.ID {
		t.Fatalf("item = %+v", it)
	}

	// Read it back, by id and in the list.
	var got itemBody
	e.do(t, "GET", "/v1/items/"+it.ID, tok, nil).decode(t, &got)
	if got.ID != it.ID || len(got.Variants) != 2 {
		t.Errorf("get = %+v", got)
	}
	var list struct {
		Items      []itemBody `json:"items"`
		NextCursor *string    `json:"next_cursor"`
	}
	e.do(t, "GET", "/v1/items?category_id="+cat.ID, tok, nil).decode(t, &list)
	if len(list.Items) != 1 || list.NextCursor != nil {
		t.Errorf("list = %+v", list)
	}

	// Patch: rename, clear the category, replace the groups; prices stay on variants.
	r := e.do(t, "PATCH", "/v1/items/"+it.ID, tok, map[string]any{"name": "Caffe latte", "clear_category": true, "modifier_group_ids": []string{}})
	if r.Code != http.StatusOK {
		t.Fatalf("PATCH item: %d %s", r.Code, r.Body.String())
	}
	got = itemBody{}
	r.decode(t, &got)
	if got.Name != "Caffe latte" || got.CategoryID != nil || len(got.ModifierGroupIDs) != 0 || len(got.Variants) != 2 {
		t.Errorf("patched = %+v", got)
	}

	// A variant: add, reprice, archive.
	var v variantBody
	e.create(t, "/v1/items/"+it.ID+"/variants", tok, map[string]any{"name": "XL", "base_price": 35000}, &v)
	if v.ItemID != it.ID || v.SortOrder != 2 {
		t.Errorf("variant = %+v", v)
	}
	r = e.do(t, "PATCH", "/v1/variants/"+v.ID, tok, map[string]any{"base_price": 36000, "archived": true})
	r.decode(t, &v)
	if r.Code != http.StatusOK || v.BasePrice != 36000 || v.Archived == nil {
		t.Errorf("variant after patch = %d %+v", r.Code, v)
	}

	// Modifiers.
	var m modifierBody
	e.create(t, "/v1/modifier-groups/"+grp.ID+"/modifiers", tok, map[string]any{"name": "Oat milk", "price_delta": 4000}, &m)
	r = e.do(t, "PATCH", "/v1/modifiers/"+m.ID, tok, map[string]any{"price_delta": -1000})
	r.decode(t, &m)
	if r.Code != http.StatusOK || m.PriceDelta != -1000 {
		t.Errorf("modifier = %d %+v", r.Code, m)
	}
	r = e.do(t, "PATCH", "/v1/modifier-groups/"+grp.ID, tok, map[string]any{"max_select": 2})
	r.decode(t, &grp)
	if r.Code != http.StatusOK || grp.MaxSelect != 2 || len(grp.Modifiers) != 3 {
		t.Errorf("group = %d %+v", r.Code, grp)
	}

	// An outlet's price and availability.
	path := "/v1/outlets/" + f.outlet.ID.String() + "/variants/" + it.Variants[0].ID
	var ov outletVariantBody
	r = e.do(t, "PUT", path, tok, map[string]any{"price_override": 27000, "available": false})
	r.decode(t, &ov)
	if r.Code != http.StatusOK || ov.PriceOverride == nil || *ov.PriceOverride != 27000 || ov.Available {
		t.Errorf("outlet variant = %d %+v", r.Code, ov)
	}
	var ovs struct {
		Items []outletVariantBody `json:"items"`
	}
	e.do(t, "GET", "/v1/outlets/"+f.outlet.ID.String()+"/variants", tok, nil).decode(t, &ovs)
	if len(ovs.Items) != 1 || ovs.Items[0].VariantID != it.Variants[0].ID {
		t.Errorf("outlet variants = %+v", ovs)
	}
	r = e.do(t, "PUT", path, tok, map[string]any{"available": true})
	ov = outletVariantBody{}
	r.decode(t, &ov)
	if ov.PriceOverride != nil || !ov.Available {
		t.Errorf("cleared outlet variant = %+v", ov)
	}

	// Archive and restore through the list.
	e.do(t, "PATCH", "/v1/items/"+it.ID, tok, map[string]any{"archived": true})
	e.do(t, "GET", "/v1/items", tok, nil).decode(t, &list)
	if len(list.Items) != 0 {
		t.Errorf("archived item listed: %+v", list.Items)
	}
	e.do(t, "GET", "/v1/items?include_archived=true", tok, nil).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ArchivedAt == nil {
		t.Errorf("include_archived list = %+v", list.Items)
	}
}

func TestCatalogErrorCodes(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	item := map[string]any{"name": "X", "variants": []map[string]any{{"base_price": 1000, "sku": "S1"}}}

	e.do(t, "POST", "/v1/items", tok, map[string]any{"name": "X"}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/items", tok, map[string]any{"name": "X", "variants": []map[string]any{{"base_price": -5}}}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/items", tok, map[string]any{"name": "X", "category_id": uuid.NewString(), "variants": []map[string]any{{"base_price": 1}}}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/items", tok, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/items?limit=0", tok, nil).problem(t, http.StatusBadRequest, "validation_failed")

	if r := e.do(t, "POST", "/v1/items", tok, item); r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "POST", "/v1/items", tok, item).problem(t, http.StatusConflict, "conflict")
	e.do(t, "POST", "/v1/categories", tok, map[string]any{"name": "Food"})
	e.do(t, "POST", "/v1/categories", tok, map[string]any{"name": "food"}).problem(t, http.StatusConflict, "conflict")
	e.do(t, "POST", "/v1/modifier-groups", tok, map[string]any{"name": "G", "required": true}).problem(t, http.StatusBadRequest, "validation_failed")

	missing := uuid.NewString()
	e.do(t, "GET", "/v1/items/"+missing, tok, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/items/"+missing, tok, map[string]any{}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/variants/"+missing, tok, map[string]any{}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/categories/"+missing, tok, map[string]any{}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/modifier-groups/"+missing, tok, map[string]any{}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/modifiers/"+missing, tok, map[string]any{}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "POST", "/v1/items/"+missing+"/variants", tok, map[string]any{"base_price": 1}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "POST", "/v1/modifier-groups/"+missing+"/modifiers", tok, map[string]any{"name": "x"}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "GET", "/v1/outlets/"+missing+"/variants", tok, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PUT", "/v1/outlets/"+missing+"/variants/"+missing, tok, map[string]any{"available": true}).problem(t, http.StatusNotFound, "not_found")
}

func TestCatalogNeedsCatalogManage(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	manager := e.member(t, f, "mgr@kopi.test", "Manager")
	owner := e.login(t, "owner@kopi.test").AccessToken

	var it itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Tea", "variants": []map[string]any{{"base_price": 8000}}}, &it)
	v := it.Variants[0].ID
	outlet := f.outlet.ID.String()

	attempts := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/categories", nil}, {"POST", "/v1/categories", map[string]any{"name": "C"}},
		{"GET", "/v1/items", nil}, {"POST", "/v1/items", map[string]any{"name": "N", "variants": []map[string]any{{"base_price": 1}}}},
		{"GET", "/v1/items/" + it.ID, nil}, {"PATCH", "/v1/items/" + it.ID, map[string]any{"name": "Z"}},
		{"POST", "/v1/items/" + it.ID + "/variants", map[string]any{"base_price": 1}},
		{"PATCH", "/v1/variants/" + v, map[string]any{"base_price": 1}},
		{"GET", "/v1/modifier-groups", nil}, {"POST", "/v1/modifier-groups", map[string]any{"name": "G"}},
		{"GET", "/v1/outlets/" + outlet + "/variants", nil},
		{"PUT", "/v1/outlets/" + outlet + "/variants/" + v, map[string]any{"available": false}},
	}
	for _, a := range attempts {
		e.do(t, a.method, a.path, cashier.AccessToken, a.body).problem(t, http.StatusForbidden, "forbidden")
	}
	// A cashier changed nothing.
	var got itemBody
	e.do(t, "GET", "/v1/items/"+it.ID, owner, nil).decode(t, &got)
	if got.Name != "Tea" || got.Variants[0].BasePrice != 8000 {
		t.Errorf("a cashier changed the catalog: %+v", got)
	}
	// A manager holds catalog.manage.
	if r := e.do(t, "PATCH", "/v1/variants/"+v, manager.AccessToken, map[string]any{"base_price": 9000}); r.Code != http.StatusOK {
		t.Errorf("manager: %d %s", r.Code, r.Body.String())
	}
}

// The permission is per outlet where the route names one: catalog.manage at one outlet does not
// allow changing another outlet's prices, though the same person may edit the shared catalog, and
// report.view at one outlet does not show another's sales.
func TestOutletRoutesNeedThePermissionAtThatOutlet(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	manager := e.member(t, f, "mgr@kopi.test", "Manager") // Manager at JKT1 only

	// A second outlet, which tenancy cannot create yet (that arrives with signup).
	second := uuid.New()
	ctx := t.Context()
	if _, err := e.d.Owner.Exec(ctx, `INSERT INTO outlet (id, tenant_id, name, code) VALUES ($1, $2, 'Cabang', 'BDG1')`, second, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Owner.Exec(ctx, `INSERT INTO outlet_settings (outlet_id, tenant_id) VALUES ($1, $2)`, second, f.tenant.ID); err != nil {
		t.Fatal(err)
	}

	var it itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Tea", "variants": []map[string]any{{"base_price": 8000}}}, &it)
	v := it.Variants[0].ID
	body := map[string]any{"price_override": 9000, "available": true}

	if r := e.do(t, "PUT", "/v1/outlets/"+f.outlet.ID.String()+"/variants/"+v, manager.AccessToken, body); r.Code != http.StatusOK {
		t.Errorf("own outlet: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "PUT", "/v1/outlets/"+second.String()+"/variants/"+v, manager.AccessToken, body).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/outlets/"+second.String()+"/variants", manager.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	if r := e.do(t, "PUT", "/v1/outlets/"+second.String()+"/variants/"+v, owner, body); r.Code != http.StatusOK {
		t.Errorf("owner at the second outlet: %d %s", r.Code, r.Body.String())
	}

	// Reports too: the manager reads the outlet they manage, not the other one.
	report := func(outlet uuid.UUID) string {
		return "/v1/reports/sales?outlet_id=" + outlet.String() + "&from=2026-01-01&to=2026-01-31&group_by=day"
	}
	if r := e.do(t, "GET", report(f.outlet.ID), manager.AccessToken, nil); r.Code != http.StatusOK {
		t.Errorf("sales report at own outlet: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "GET", report(second), manager.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
}

type outletSettingsBody struct {
	Timezone          string `json:"timezone"`
	BusinessDayCutoff string `json:"business_day_cutoff"`
	TaxRateBP         int    `json:"tax_rate_bp"`
	CashRoundingMode  string `json:"cash_rounding_mode"`
	ReceiptFooter     string `json:"receipt_footer"`
}

func TestOutletSettingsOverHTTP(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	manager := e.member(t, f, "mgr@kopi.test", "Manager") // Manager lacks settings.manage
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	path := "/v1/outlets/" + f.outlet.ID.String() + "/settings"

	var o struct {
		Settings outletSettingsBody `json:"settings"`
	}
	r := e.do(t, "PATCH", path, owner, map[string]any{
		"timezone": "Asia/Makassar", "business_day_cutoff": "04:30", "tax_rate_bp": 1100, "cash_rounding_mode": "up", "receipt_footer": "Terima kasih",
	})
	if r.Code != http.StatusOK {
		t.Fatalf("PATCH settings: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &o)
	if o.Settings.Timezone != "Asia/Makassar" || o.Settings.BusinessDayCutoff != "04:30" || o.Settings.TaxRateBP != 1100 ||
		o.Settings.CashRoundingMode != "up" || o.Settings.ReceiptFooter != "Terima kasih" {
		t.Errorf("settings = %+v", o.Settings)
	}
	// The outlet endpoints show them too, with the cutoff always present.
	o.Settings = outletSettingsBody{}
	e.do(t, "GET", "/v1/outlets/"+f.outlet.ID.String(), owner, nil).decode(t, &o)
	if o.Settings.BusinessDayCutoff != "04:30" {
		t.Errorf("GET outlet: %+v", o.Settings)
	}

	for name, token := range map[string]string{"manager": manager.AccessToken, "cashier": cashier.AccessToken} {
		e.do(t, "PATCH", path, token, map[string]any{"tax_rate_bp": 0}).problem(t, http.StatusForbidden, "forbidden")
		_ = name
	}
	e.do(t, "PATCH", path, "", map[string]any{"tax_rate_bp": 0}).problem(t, http.StatusUnauthorized, "invalid_token")

	for name, body := range map[string]map[string]any{
		"bad zone":    {"timezone": "Europe/Paris"},
		"bad cutoff":  {"business_day_cutoff": "25:00"},
		"noon":        {"business_day_cutoff": "12:00"},
		"not a time":  {"business_day_cutoff": "four"},
		"tax too big": {"tax_rate_bp": 10001},
		"bad mode":    {"cash_rounding_mode": "sideways"},
	} {
		if r := e.do(t, "PATCH", path, owner, body); r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.Code, r.Body.String())
		}
	}
	e.do(t, "PATCH", "/v1/outlets/"+uuid.NewString()+"/settings", owner, map[string]any{"tax_rate_bp": 0}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", path, owner, nil).problem(t, http.StatusBadRequest, "validation_failed")

	// Nothing refused changed anything.
	o.Settings = outletSettingsBody{}
	e.do(t, "GET", "/v1/outlets/"+f.outlet.ID.String(), owner, nil).decode(t, &o)
	if o.Settings.TaxRateBP != 1100 || o.Settings.Timezone != "Asia/Makassar" {
		t.Errorf("settings after refused updates: %+v", o.Settings)
	}
}

// Stations are managed like categories, items point at one, and tablets get both in the pull.
func TestKitchenStationsRouteItemsAndReachTheTablet(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")

	var kitchen, bar categoryBody
	e.create(t, "/v1/kitchen-stations", owner, map[string]any{"name": "Kitchen"}, &kitchen)
	e.create(t, "/v1/kitchen-stations", owner, map[string]any{"name": "Bar", "sort_order": 1}, &bar)
	e.do(t, "POST", "/v1/kitchen-stations", owner, map[string]any{"name": "BAR"}).problem(t, http.StatusConflict, "conflict")
	e.do(t, "POST", "/v1/kitchen-stations", owner, map[string]any{"name": strings.Repeat("x", 41)}).problem(t, http.StatusBadRequest, "validation_failed")

	type stationItem struct {
		ID        string  `json:"id"`
		StationID *string `json:"station_id"`
	}
	var latte, water stationItem
	e.create(t, "/v1/items", owner, map[string]any{"name": "Latte", "station_id": bar.ID, "variants": []map[string]any{{"base_price": 28000}}}, &latte)
	e.create(t, "/v1/items", owner, map[string]any{"name": "Air", "variants": []map[string]any{{"base_price": 5000}}}, &water)
	if latte.StationID == nil || *latte.StationID != bar.ID || water.StationID != nil {
		t.Fatalf("latte %+v, water %+v", latte, water)
	}

	type pullStations struct {
		Cursor          string         `json:"cursor"`
		KitchenStations []categoryBody `json:"kitchen_stations"`
		Items           []stationItem  `json:"items"`
	}
	var snap pullStations
	e.do(t, "GET", "/v1/sync/pull", p.token, nil).decode(t, &snap)
	if len(snap.KitchenStations) != 2 || len(snap.Items) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// Moving the latte to the kitchen and renaming the bar arrive as a delta.
	var moved stationItem
	r := e.do(t, "PATCH", "/v1/items/"+latte.ID, owner, map[string]any{"station_id": kitchen.ID})
	r.decode(t, &moved)
	if moved.StationID == nil || *moved.StationID != kitchen.ID {
		t.Errorf("moved = %+v", moved)
	}
	var renamed categoryBody
	e.do(t, "PATCH", "/v1/kitchen-stations/"+bar.ID, owner, map[string]any{"name": "Coffee bar"}).decode(t, &renamed)
	var delta pullStations
	e.do(t, "GET", "/v1/sync/pull?cursor="+snap.Cursor, p.token, nil).decode(t, &delta)
	if len(delta.KitchenStations) != 1 || delta.KitchenStations[0].Name != "Coffee bar" || len(delta.Items) != 1 || *delta.Items[0].StationID != kitchen.ID {
		t.Errorf("delta = %+v", delta)
	}

	// Clearing, and the two ways of saying it at once.
	var cleared stationItem // a null station_id is left out of the body, like category_id
	e.do(t, "PATCH", "/v1/items/"+latte.ID, owner, map[string]any{"clear_station": true}).decode(t, &cleared)
	if cleared.StationID != nil {
		t.Errorf("cleared = %+v", cleared)
	}
	e.do(t, "PATCH", "/v1/items/"+latte.ID, owner, map[string]any{"clear_station": true, "station_id": bar.ID}).problem(t, http.StatusBadRequest, "validation_failed")
	// A patch that does not mention the station leaves it alone.
	e.do(t, "PATCH", "/v1/items/"+latte.ID, owner, map[string]any{"station_id": bar.ID})
	var renamedItem stationItem
	e.do(t, "PATCH", "/v1/items/"+latte.ID, owner, map[string]any{"name": "Caffe latte"}).decode(t, &renamedItem)
	if renamedItem.StationID == nil || *renamedItem.StationID != bar.ID {
		t.Errorf("a rename moved the station: %+v", renamedItem)
	}

	// Archived stations leave the list but stay pointed at; a new one may take the name.
	e.do(t, "PATCH", "/v1/kitchen-stations/"+kitchen.ID, owner, map[string]any{"archived": true})
	var list struct{ Items []categoryBody }
	e.do(t, "GET", "/v1/kitchen-stations", owner, nil).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != bar.ID {
		t.Errorf("list = %+v", list)
	}
	e.create(t, "/v1/kitchen-stations", owner, map[string]any{"name": "Kitchen"}, &categoryBody{})

	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	e.do(t, "GET", "/v1/kitchen-stations", cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
}
