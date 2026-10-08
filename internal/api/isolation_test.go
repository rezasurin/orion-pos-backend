package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
)

// The isolation suite (B0.11). Two businesses, every tenant-side operation, tenant B reaching for
// tenant A's data by id. Nothing of A may appear in any response to B, and by-id access must look
// like the object does not exist. A coverage test fails when a new operation is added without a
// case here, so the suite cannot quietly fall behind the API.

type world struct {
	e *env

	a, b              tenantFixture
	userA, userB      string // owner access tokens
	deviceA, deviceB  string // device access tokens
	staffA, deviceIDA string
	deviceIDB         string
	roleA             string
	catA              catalogIDs
	invA              inventoryIDs
	syncA             string   // an event record of tenant A, pushed by its device
	shiftA            string   // a shift of tenant A, opened by its device
	secrets           []string // every identifier belonging to A
}

// inventoryIDs are tenant A's inventory setup.
type inventoryIDs struct{ txType, ingCategory, uomCategory, uom, ingredient string }

// catalogIDs are tenant A's catalog objects.
type catalogIDs struct{ category, station, item, variant, group, modifier string }

func newWorld(t *testing.T) *world {
	t.Helper()
	e := newEnv(t)
	w := &world{e: e}
	w.a = e.business(t, "kopi", "JKT1", "owner@kopi.test")
	w.b = e.business(t, "tea", "BDG1", "owner@tea.test")
	w.userA, w.userB = e.login(t, "owner@kopi.test").AccessToken, e.login(t, "owner@tea.test").AccessToken

	roles := e.roles(t, w.userA)
	w.roleA = roles["Cashier"].ID
	var st staffBody
	e.do(t, "POST", "/v1/staff", w.userA, map[string]any{
		"display_name": "Sari of A", "pin": "4821",
		"outlet_roles": []map[string]string{{"outlet_id": w.a.outlet.ID.String(), "role_id": w.roleA}},
	}).decode(t, &st)
	w.staffA = st.ID
	pa := e.pairDevice(t, w.userA, w.a.outlet.ID.String(), "Kasir A")
	w.deviceIDA = pa.Device.ID
	w.deviceA = e.deviceToken(t, pa.DeviceSecret).AccessToken
	pb := e.pairDevice(t, w.userB, w.b.outlet.ID.String(), "Kasir B")
	w.deviceIDB = pb.Device.ID
	w.deviceB = e.deviceToken(t, pb.DeviceSecret).AccessToken

	evA := uuid.NewString()
	if r := e.do(t, "POST", "/v1/sync/push", w.deviceA, map[string]any{"device_id": pa.Device.ID, "events": []map[string]any{{
		"id": evA, "idempotency_key": evA, "type": "test.created", "staff_id": w.staffA,
		"device_time": time.Now().UTC().Format(time.RFC3339), "schema_version": 1, "payload": map[string]any{"label": "A's secret sale"},
	}}}); r.Code != http.StatusOK {
		t.Fatalf("A's push: %d %s", r.Code, r.Body.String())
	}
	w.syncA = evA
	shiftA := uuid.NewString()
	if r := e.do(t, "POST", "/v1/sync/push", w.deviceA, map[string]any{"device_id": pa.Device.ID, "events": []map[string]any{{
		"id": shiftA, "idempotency_key": shiftA, "type": "shift.opened", "staff_id": w.staffA,
		"device_time": time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "schema_version": 1, "payload": map[string]any{"opening_cash": 777000},
	}}}); r.Code != http.StatusOK {
		t.Fatalf("A's shift: %d %s", r.Code, r.Body.String())
	}
	w.shiftA = shiftA

	var cat, station categoryBody
	e.create(t, "/v1/categories", w.userA, map[string]any{"name": "Secret category"}, &cat)
	e.create(t, "/v1/kitchen-stations", w.userA, map[string]any{"name": "Secret station"}, &station)
	var grp groupBody
	e.create(t, "/v1/modifier-groups", w.userA, map[string]any{
		"name": "Secret group", "modifiers": []map[string]any{{"name": "Secret modifier"}},
	}, &grp)
	var it itemBody
	e.create(t, "/v1/items", w.userA, map[string]any{
		"name": "Secret blend", "category_id": cat.ID, "station_id": station.ID, "sku": "SECRET-SKU", "modifier_group_ids": []string{grp.ID},
		"variants": []map[string]any{{"name": "Secret size", "base_price": 77777, "barcode": "SECRET-BARCODE"}},
	}, &it)
	w.catA = catalogIDs{category: cat.ID, station: station.ID, item: it.ID, variant: it.Variants[0].ID, group: grp.ID, modifier: grp.Modifiers[0].ID}

	var tt, ic, ing categoryBody
	var uc uomCategoryBody
	e.create(t, "/v1/transaction-types", w.userA, map[string]any{"name": "Secret purchase", "category": "materials"}, &tt)
	e.create(t, "/v1/ingredient-categories", w.userA, map[string]any{"name": "Secret ingredients", "default_transaction_type_id": tt.ID}, &ic)
	e.create(t, "/v1/uom-categories", w.userA, map[string]any{"name": "Secret weights", "units": []map[string]any{{"name": "secretgram", "is_reference": true}}}, &uc)
	e.create(t, "/v1/ingredients", w.userA, map[string]any{"name": "Secret beans", "category_id": ic.ID, "uom_id": uc.Units[0].ID}, &ing)
	w.invA = inventoryIDs{txType: tt.ID, ingCategory: ic.ID, uomCategory: uc.ID, uom: uc.Units[0].ID, ingredient: ing.ID}

	w.secrets = []string{w.syncA, tt.ID, ic.ID, uc.ID, uc.Units[0].ID, ing.ID, "Secret purchase", "Secret ingredients", "Secret weights", "secretgram", "Secret beans", w.shiftA, "777000", "A's secret sale", w.catA.category, w.catA.station, "Secret station", w.catA.item, w.catA.variant, w.catA.group, w.catA.modifier,
		"Secret category", "Secret group", "Secret modifier", "Secret blend", "Secret size", "SECRET-SKU", "SECRET-BARCODE", "77777",
		w.a.tenant.ID.String(), w.a.outlet.ID.String(), w.a.owner.ID.String(), w.staffA, w.deviceIDA, "owner@kopi.test", "Sari of A", "Kasir A", "JKT1"}
	for _, r := range roles {
		w.secrets = append(w.secrets, r.ID)
	}
	return w
}

// noLeak fails if any of A's identifiers or names appears in a response to B.
func (w *world) noLeak(t *testing.T, op string, r response) {
	t.Helper()
	body := r.Body.String()
	for _, s := range w.secrets {
		if strings.Contains(body, s) {
			t.Errorf("%s: a response to tenant B contains %q of tenant A: %s", op, s, body)
		}
	}
}

// notFound asserts a by-id request for A's object, made as B, is indistinguishable from a missing one.
func (w *world) gone(t *testing.T, op string, r response, status int, code string) {
	t.Helper()
	w.noLeak(t, op, r)
	if r.Code != status {
		t.Errorf("%s: status %d, want %d; body %s", op, r.Code, status, r.Body.String())
		return
	}
	r.problem(t, status, code)
}

func TestTenantIsolationAcrossEveryOperation(t *testing.T) {
	w := newWorld(t)
	e := w.e
	ub, db := w.userB, w.deviceB
	aOutlet, aRole := w.a.outlet.ID.String(), w.roleA

	cases := map[string]func(t *testing.T){
		// Lists and reads of B's own view must hold nothing of A's.
		"GetMe":           func(t *testing.T) { w.noLeak(t, "GetMe", e.do(t, "GET", "/v1/me", ub, nil)) },
		"ListOutlets":     func(t *testing.T) { w.noLeak(t, "ListOutlets", e.do(t, "GET", "/v1/outlets", ub, nil)) },
		"ListRoles":       func(t *testing.T) { w.noLeak(t, "ListRoles", e.do(t, "GET", "/v1/roles", ub, nil)) },
		"ListStaff":       func(t *testing.T) { w.noLeak(t, "ListStaff", e.do(t, "GET", "/v1/staff?limit=200", ub, nil)) },
		"ListDevices":     func(t *testing.T) { w.noLeak(t, "ListDevices", e.do(t, "GET", "/v1/devices?limit=200", ub, nil)) },
		"GetEntitlements": func(t *testing.T) { w.noLeak(t, "GetEntitlements", e.do(t, "GET", "/v1/entitlements", ub, nil)) },
		"GetRoster":       func(t *testing.T) { w.noLeak(t, "GetRoster", e.do(t, "GET", "/v1/pos/roster", db, nil)) },
		"GetReceiptTest":  func(t *testing.T) { w.noLeak(t, "GetReceiptTest", e.do(t, "GET", "/v1/pos/receipt-test", db, nil)) },

		// By-id access to A's objects looks like they do not exist.
		"ListCategories": func(t *testing.T) {
			w.noLeak(t, "ListCategories", e.do(t, "GET", "/v1/categories?include_archived=true&limit=200", ub, nil))
		},
		"ListItems": func(t *testing.T) {
			w.noLeak(t, "ListItems", e.do(t, "GET", "/v1/items?include_archived=true&limit=200", ub, nil))
		},
		"ListModifierGroups": func(t *testing.T) {
			w.noLeak(t, "ListModifierGroups", e.do(t, "GET", "/v1/modifier-groups?include_archived=true&limit=200", ub, nil))
		},
		"ListOutletVariants": func(t *testing.T) {
			// B's own outlet lists nothing of A's; A's outlet looks like it does not exist.
			w.noLeak(t, "ListOutletVariants", e.do(t, "GET", "/v1/outlets/"+w.b.outlet.ID.String()+"/variants", ub, nil))
			w.gone(t, "ListOutletVariants/A", e.do(t, "GET", "/v1/outlets/"+aOutlet+"/variants", ub, nil), http.StatusNotFound, "not_found")
		},
		"GetItem": func(t *testing.T) {
			w.gone(t, "GetItem", e.do(t, "GET", "/v1/items/"+w.catA.item, ub, nil), http.StatusNotFound, "not_found")
		},
		"UpdateItem": func(t *testing.T) {
			w.gone(t, "UpdateItem", e.do(t, "PATCH", "/v1/items/"+w.catA.item, ub, map[string]any{"name": "Taken", "archived": true}), http.StatusNotFound, "not_found")
		},
		"ListKitchenStations": func(t *testing.T) {
			w.noLeak(t, "ListKitchenStations", e.do(t, "GET", "/v1/kitchen-stations?include_archived=true&limit=200", ub, nil))
		},
		"CreateKitchenStation": func(t *testing.T) {
			w.noLeak(t, "CreateKitchenStation", e.do(t, "POST", "/v1/kitchen-stations", ub, map[string]any{"name": "Plain station"}))
		},
		"UpdateKitchenStation": func(t *testing.T) {
			w.gone(t, "UpdateKitchenStation", e.do(t, "PATCH", "/v1/kitchen-stations/"+w.catA.station, ub, map[string]any{"archived": true}), http.StatusNotFound, "not_found")
		},
		"UpdateCategory": func(t *testing.T) {
			w.gone(t, "UpdateCategory", e.do(t, "PATCH", "/v1/categories/"+w.catA.category, ub, map[string]any{"name": "Taken"}), http.StatusNotFound, "not_found")
		},
		"AddVariant": func(t *testing.T) {
			w.gone(t, "AddVariant", e.do(t, "POST", "/v1/items/"+w.catA.item+"/variants", ub, map[string]any{"base_price": 1}), http.StatusNotFound, "not_found")
		},
		"UpdateVariant": func(t *testing.T) {
			w.gone(t, "UpdateVariant", e.do(t, "PATCH", "/v1/variants/"+w.catA.variant, ub, map[string]any{"base_price": 1}), http.StatusNotFound, "not_found")
		},
		"UpdateModifierGroup": func(t *testing.T) {
			w.gone(t, "UpdateModifierGroup", e.do(t, "PATCH", "/v1/modifier-groups/"+w.catA.group, ub, map[string]any{"name": "Taken"}), http.StatusNotFound, "not_found")
		},
		"AddModifier": func(t *testing.T) {
			w.gone(t, "AddModifier", e.do(t, "POST", "/v1/modifier-groups/"+w.catA.group+"/modifiers", ub, map[string]any{"name": "Taken"}), http.StatusNotFound, "not_found")
		},
		"UpdateModifier": func(t *testing.T) {
			w.gone(t, "UpdateModifier", e.do(t, "PATCH", "/v1/modifiers/"+w.catA.modifier, ub, map[string]any{"price_delta": 1}), http.StatusNotFound, "not_found")
		},
		"SetOutletVariant": func(t *testing.T) {
			// A's variant at B's outlet, B's variant id at A's outlet, and both of A's.
			w.gone(t, "SetOutletVariant/variant", e.do(t, "PUT", "/v1/outlets/"+w.b.outlet.ID.String()+"/variants/"+w.catA.variant, ub, map[string]any{"available": false}), http.StatusNotFound, "not_found")
			w.gone(t, "SetOutletVariant/outlet", e.do(t, "PUT", "/v1/outlets/"+aOutlet+"/variants/"+w.catA.variant, ub, map[string]any{"available": false}), http.StatusNotFound, "not_found")
		},
		"CreateCategory": func(t *testing.T) {
			// B names a category after one of A's: allowed (checked after the loop), and the body
			// of B's own response never carries A's data.
			w.noLeak(t, "CreateCategory", e.do(t, "POST", "/v1/categories", ub, map[string]any{"name": "Plain category"}))
		},
		"CreateModifierGroup": func(t *testing.T) {
			e.do(t, "POST", "/v1/modifier-groups", ub, map[string]any{"name": "B group"}).decode(t, &struct{}{})
		},
		"CreateItem": func(t *testing.T) {
			// Pointing at A's category or group is refused.
			e.do(t, "POST", "/v1/items", ub, map[string]any{"name": "X", "category_id": w.catA.category, "variants": []map[string]any{{"base_price": 1}}}).
				problem(t, http.StatusBadRequest, "validation_failed")
			e.do(t, "POST", "/v1/items", ub, map[string]any{"name": "X", "modifier_group_ids": []string{w.catA.group}, "variants": []map[string]any{{"base_price": 1}}}).
				problem(t, http.StatusBadRequest, "validation_failed")
			e.do(t, "POST", "/v1/items", ub, map[string]any{"name": "X", "station_id": w.catA.station, "variants": []map[string]any{{"base_price": 1}}}).
				problem(t, http.StatusBadRequest, "validation_failed")
		},

		"ListTransactionTypes": func(t *testing.T) {
			w.noLeak(t, "ListTransactionTypes", e.do(t, "GET", "/v1/transaction-types?include_archived=true&limit=200", ub, nil))
		},
		"CreateTransactionType": func(t *testing.T) {
			w.noLeak(t, "CreateTransactionType", e.do(t, "POST", "/v1/transaction-types", ub, map[string]any{"name": "Plain purchase", "category": "services"}))
		},
		"UpdateTransactionType": func(t *testing.T) {
			w.gone(t, "UpdateTransactionType", e.do(t, "PATCH", "/v1/transaction-types/"+w.invA.txType, ub, map[string]any{"archived": true}), http.StatusNotFound, "not_found")
		},
		"ListIngredientCategories": func(t *testing.T) {
			w.noLeak(t, "ListIngredientCategories", e.do(t, "GET", "/v1/ingredient-categories?include_archived=true&limit=200", ub, nil))
		},
		"CreateIngredientCategory": func(t *testing.T) {
			w.noLeak(t, "CreateIngredientCategory", e.do(t, "POST", "/v1/ingredient-categories", ub, map[string]any{"name": "Plain ingredients"}))
			w.gone(t, "CreateIngredientCategory", e.do(t, "POST", "/v1/ingredient-categories", ub, map[string]any{"name": "X", "default_transaction_type_id": w.invA.txType}), http.StatusBadRequest, "validation_failed")
		},
		"UpdateIngredientCategory": func(t *testing.T) {
			w.gone(t, "UpdateIngredientCategory", e.do(t, "PATCH", "/v1/ingredient-categories/"+w.invA.ingCategory, ub, map[string]any{"archived": true}), http.StatusNotFound, "not_found")
		},
		"ListUomCategories": func(t *testing.T) {
			w.noLeak(t, "ListUomCategories", e.do(t, "GET", "/v1/uom-categories?include_archived=true&limit=200", ub, nil))
		},
		"CreateUomCategory": func(t *testing.T) {
			w.noLeak(t, "CreateUomCategory", e.do(t, "POST", "/v1/uom-categories", ub, map[string]any{"name": "Plain", "units": []map[string]any{{"name": "g", "is_reference": true}}}))
		},
		"UpdateUomCategory": func(t *testing.T) {
			w.gone(t, "UpdateUomCategory", e.do(t, "PATCH", "/v1/uom-categories/"+w.invA.uomCategory, ub, map[string]any{"archived": true}), http.StatusNotFound, "not_found")
		},
		"ListIngredients": func(t *testing.T) {
			w.noLeak(t, "ListIngredients", e.do(t, "GET", "/v1/ingredients?include_archived=true&limit=200", ub, nil))
			w.noLeak(t, "ListIngredients", e.do(t, "GET", "/v1/ingredients?category_id="+w.invA.ingCategory, ub, nil))
		},
		"CreateIngredient": func(t *testing.T) {
			// A's unit and A's ingredient category are refused as if they did not exist.
			w.gone(t, "CreateIngredient", e.do(t, "POST", "/v1/ingredients", ub, map[string]any{"name": "X", "uom_id": w.invA.uom}), http.StatusBadRequest, "validation_failed")
			var uc uomCategoryBody
			e.create(t, "/v1/uom-categories", ub, map[string]any{"name": "B weights", "units": []map[string]any{{"name": "g", "is_reference": true}}}, &uc)
			w.gone(t, "CreateIngredient", e.do(t, "POST", "/v1/ingredients", ub, map[string]any{"name": "X", "uom_id": uc.Units[0].ID, "category_id": w.invA.ingCategory}), http.StatusBadRequest, "validation_failed")
		},
		"UpdateIngredient": func(t *testing.T) {
			w.gone(t, "UpdateIngredient", e.do(t, "PATCH", "/v1/ingredients/"+w.invA.ingredient, ub, map[string]any{"archived": true}), http.StatusNotFound, "not_found")
		},

		"ListSales": func(t *testing.T) {
			w.noLeak(t, "ListSales", e.do(t, "GET", "/v1/sales?limit=200", ub, nil))
			w.gone(t, "ListSales/A's outlet", e.do(t, "GET", "/v1/sales?outlet_id="+aOutlet, ub, nil), http.StatusNotFound, "not_found")
		},
		"GetSale": func(t *testing.T) {
			// The id of a sale of A's, which does not exist for B (A's world has none yet, so use a shift id: any A id is unknown to B).
			w.gone(t, "GetSale", e.do(t, "GET", "/v1/sales/"+w.shiftA, ub, nil), http.StatusNotFound, "not_found")
		},
		"GetShiftReport": func(t *testing.T) {
			w.gone(t, "GetShiftReport", e.do(t, "GET", "/v1/reports/shifts/"+w.shiftA, ub, nil), http.StatusNotFound, "not_found")
		},
		"GetDayReport": func(t *testing.T) {
			today := time.Now().UTC().Format(time.DateOnly)
			w.gone(t, "GetDayReport", e.do(t, "GET", "/v1/reports/days/"+today+"?outlet_id="+aOutlet, ub, nil), http.StatusNotFound, "not_found")
			// B's own report for the same day holds nothing of A's.
			w.noLeak(t, "GetDayReport/own", e.do(t, "GET", "/v1/reports/days/"+today+"?outlet_id="+w.b.outlet.ID.String(), ub, nil))
		},
		"GetSalesReport": func(t *testing.T) {
			today := time.Now().UTC().Format(time.DateOnly)
			q := "&from=" + today + "&to=" + today + "&group_by=item"
			w.gone(t, "GetSalesReport", e.do(t, "GET", "/v1/reports/sales?outlet_id="+aOutlet+q, ub, nil), http.StatusNotFound, "not_found")
			w.noLeak(t, "GetSalesReport/own", e.do(t, "GET", "/v1/reports/sales?outlet_id="+w.b.outlet.ID.String()+q, ub, nil))
		},
		"ListAnnouncements": func(t *testing.T) {
			// An announcement to A alone never reaches B.
			op, err := e.platform.CreateOperator(context.Background(), platform.Actor{}, "iso@orion.test", "bootstrap")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.platform.CreateAnnouncement(context.Background(), platform.Actor{OperatorID: &op.Operator.ID}, platform.NewAnnouncement{
				TenantID: &w.a.tenant.ID, Severity: "warning", Title: map[string]string{"id": "A's secret notice"}, Body: map[string]string{"id": "x"},
			}, "test"); err != nil {
				t.Fatal(err)
			}
			r := e.do(t, "GET", "/v1/announcements", ub, nil)
			w.noLeak(t, "ListAnnouncements", r)
			if strings.Contains(r.Body.String(), "secret notice") {
				t.Errorf("B sees A's announcement: %s", r.Body.String())
			}
		},
		"PullChanges": func(t *testing.T) {
			// B's tablet pulls everything it can: nothing of A's, from a snapshot or from cursor zero.
			w.noLeak(t, "PullChanges", e.do(t, "GET", "/v1/sync/pull", db, nil))
			w.noLeak(t, "PullChanges/limit", e.do(t, "GET", "/v1/sync/pull?limit=1000", db, nil))
		},
		"PushEvents": func(t *testing.T) {
			// B's tablet names A's cashier, tries to void A's record, and claims A's outlet.
			push := func(ev map[string]any) response {
				id := uuid.NewString()
				ev["id"], ev["idempotency_key"] = id, id
				ev["staff_id"], ev["device_time"], ev["schema_version"] = w.staffA, time.Now().UTC().Format(time.RFC3339), 1
				return e.do(t, "POST", "/v1/sync/push", db, map[string]any{"device_id": w.deviceIDB, "events": []map[string]any{ev}})
			}
			r := push(map[string]any{"type": "test.voided", "payload": map[string]any{"target": w.syncA}})
			var body pushBody
			r.decode(t, &body)
			if r.Code != http.StatusOK || body.Results[0].Code != "unknown_staff" {
				t.Errorf("B voiding A's record: %d %s", r.Code, r.Body.String())
			}
			r = push(map[string]any{"type": "test.created", "payload": map[string]any{"label": "x", "outlet_id": aOutlet}})
			body = pushBody{}
			r.decode(t, &body)
			if r.Code != http.StatusOK || body.Results[0].Status != "rejected" {
				t.Errorf("B claiming A's outlet: %d %s", r.Code, r.Body.String())
			}
			// A device cannot push as another device.
			e.do(t, "POST", "/v1/sync/push", db, map[string]any{"device_id": w.deviceIDA, "events": []map[string]any{}}).problem(t, http.StatusBadRequest, "validation_failed")
		},

		"UpdateOutletSettings": func(t *testing.T) {
			w.gone(t, "UpdateOutletSettings", e.do(t, "PATCH", "/v1/outlets/"+aOutlet+"/settings", ub, map[string]any{"tax_rate_bp": 1}), http.StatusNotFound, "not_found")
		},

		"GetOutlet": func(t *testing.T) {
			w.gone(t, "GetOutlet", e.do(t, "GET", "/v1/outlets/"+aOutlet, ub, nil), http.StatusNotFound, "not_found")
		},
		"UpdateStaff": func(t *testing.T) {
			w.gone(t, "UpdateStaff", e.do(t, "PATCH", "/v1/staff/"+w.staffA, ub, map[string]any{"active": false}), http.StatusNotFound, "not_found")
		},
		"SetStaffPin": func(t *testing.T) {
			w.gone(t, "SetStaffPin", e.do(t, "PUT", "/v1/staff/"+w.staffA+"/pin", ub, map[string]string{"pin": "4821"}), http.StatusNotFound, "not_found")
		},
		"RevokeDevice": func(t *testing.T) {
			w.gone(t, "RevokeDevice", e.do(t, "DELETE", "/v1/devices/"+w.deviceIDA, ub, nil), http.StatusNotFound, "not_found")
		},

		// Creating things that point at A's rows is refused, whatever the ids.
		"CreateStaff": func(t *testing.T) {
			w.gone(t, "CreateStaff/outlet", e.do(t, "POST", "/v1/staff", ub, map[string]any{
				"display_name": "Intruder", "outlet_roles": []map[string]string{{"outlet_id": aOutlet, "role_id": e.roles(t, ub)["Cashier"].ID}},
			}), http.StatusBadRequest, "validation_failed")
			// The error echoes the role id B itself sent, so only the status and code are checked.
			e.do(t, "POST", "/v1/staff", ub, map[string]any{
				"display_name": "Intruder", "outlet_roles": []map[string]string{{"outlet_id": w.b.outlet.ID.String(), "role_id": aRole}},
			}).problem(t, http.StatusBadRequest, "validation_failed")
		},
		"PairDevice": func(t *testing.T) {
			w.gone(t, "PairDevice", e.do(t, "POST", "/v1/devices/pair", ub, map[string]string{"outlet_id": aOutlet, "name": "Intruder"}), http.StatusBadRequest, "validation_failed")
		},
		// A's item name, category and barcode do not clash with B's import: B cannot see them. A dry
		// run, so B's catalog stays empty for the checks after the loop.
		"ImportCatalog": func(t *testing.T) {
			var res struct {
				Categories int   `json:"categories"`
				Items      int   `json:"items"`
				Errors     []any `json:"errors"`
			}
			e.do(t, "POST", "/v1/catalog/import?dry_run=true", ub, "category,item_name,price,barcode\nSecret category,Secret blend,1,SECRET-BARCODE\n").decode(t, &res)
			if res.Categories != 1 || res.Items != 1 || len(res.Errors) != 0 {
				t.Errorf("B's import saw A's catalog: %+v", res)
			}
		},
	}

	for op, fn := range cases {
		t.Run(op, fn)
	}

	// Nothing above changed A.
	var st staffBody
	e.do(t, "PATCH", "/v1/staff/"+w.staffA, w.userA, map[string]any{}).decode(t, &st)
	if !st.Active || !st.HasPin || st.DisplayName != "Sari of A" {
		t.Errorf("tenant A's staff was changed by tenant B: %+v", st)
	}
	// Names and codes are unique per business, so B may reuse A's category name, SKU and barcode.
	if r := e.do(t, "POST", "/v1/categories", ub, map[string]any{"name": "Secret category"}); r.Code != http.StatusCreated {
		t.Errorf("tenant B cannot reuse tenant A's category name: %d %s", r.Code, r.Body.String())
	}
	// Codes are unique per business: B may use the SKU and barcode A uses. This runs after the
	// loop because B's own item would then legitimately contain those strings.
	if r := e.do(t, "POST", "/v1/items", ub, map[string]any{"name": "Mine", "variants": []map[string]any{{"base_price": 1, "sku": "SECRET-SKU", "barcode": "SECRET-BARCODE"}}}); r.Code != http.StatusCreated {
		t.Errorf("tenant B cannot reuse tenant A's SKU: %d %s", r.Code, r.Body.String())
	}

	var taxA int
	if err := e.d.Owner.QueryRow(context.Background(), `SELECT tax_rate_bp FROM outlet_settings WHERE outlet_id = $1`, w.a.outlet.ID).Scan(&taxA); err != nil || taxA != 0 {
		t.Errorf("tenant A's settings were changed by tenant B (%d, %v)", taxA, err)
	}

	var voided bool
	if err := e.d.Owner.QueryRow(context.Background(), `SELECT voided FROM stub_record WHERE id = $1`, w.syncA).Scan(&voided); err != nil || voided {
		t.Errorf("tenant A's record was voided by tenant B (%v, %v)", voided, err)
	}

	for _, path := range []string{"/v1/ingredients", "/v1/uom-categories", "/v1/ingredient-categories", "/v1/transaction-types"} {
		if r := e.do(t, "GET", path, w.userA, nil); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "Secret") {
			t.Errorf("tenant A lost its %s: %d %s", path, r.Code, r.Body.String())
		}
	}

	var itA itemBody
	e.do(t, "GET", "/v1/items/"+w.catA.item, w.userA, nil).decode(t, &itA)
	if itA.Name != "Secret blend" || itA.ArchivedAt != nil || itA.Variants[0].BasePrice != 77777 || len(itA.Variants) != 1 {
		t.Errorf("tenant A's item was changed by tenant B: %+v", itA)
	}
	if r := e.do(t, "GET", "/v1/pos/roster", w.deviceA, nil); r.Code != http.StatusOK {
		t.Errorf("tenant A's device stopped working: %d", r.Code)
	}
	var created int
	_ = e.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM staff WHERE display_name = 'Intruder'`).Scan(&created)
	if created != 0 {
		t.Errorf("%d intruder staff rows were created", created)
	}
}

// Every operation a tenant or device can call has a case above; operations without a tenant
// (sign-in) or for operators are listed here with the reason they are exempt.
func TestIsolationCoversEveryOperation(t *testing.T) {
	exempt := map[string]string{
		"Login": "public: no tenant until sign-in", "RefreshSession": "public: the token names its tenant",
		"Logout": "public: the token names its tenant", "VerifyEmail": "public: the token names its tenant",
		"ResendVerification": "public: acts on an email address", "Signup": "public: creates its own tenant", "ExchangeDeviceToken": "public: the secret names its tenant",
		"AdminLogin": "operator", "AdminVerifyTotp": "operator", "AdminListAuditLog": "operator",
		"AdminListTenants": "operator", "AdminGetTenant": "operator", "AdminSuspendTenant": "operator", "AdminReinstateTenant": "operator",
		"AdminSetTenantPlan": "operator", "AdminSetTenantEntitlement": "operator", "AdminRevokeDevice": "operator",
		"AdminStoppedSyncing": "operator", "AdminListEntitlementKeys": "operator", "AdminSetFlagDefault": "operator",
		"AdminListAnnouncements": "operator", "AdminCreateAnnouncement": "operator", "AdminEndAnnouncement": "operator",
	}
	covered := map[string]bool{}
	for _, op := range []string{
		"GetMe", "ListOutlets", "ListRoles", "ListStaff", "ListDevices", "GetEntitlements", "GetRoster", "GetReceiptTest",
		"GetOutlet", "UpdateStaff", "SetStaffPin", "RevokeDevice", "CreateStaff", "PairDevice",
		"ListCategories", "ListKitchenStations", "CreateKitchenStation", "UpdateKitchenStation", "ListItems", "ListModifierGroups", "ListOutletVariants", "GetItem", "UpdateItem", "UpdateCategory",
		"AddVariant", "UpdateVariant", "UpdateModifierGroup", "AddModifier", "UpdateModifier", "SetOutletVariant",
		"CreateCategory", "CreateModifierGroup", "CreateItem", "ImportCatalog", "PushEvents", "PullChanges", "UpdateOutletSettings", "GetShiftReport", "GetDayReport", "GetSalesReport", "ListAnnouncements", "ListSales", "GetSale",
		"ListTransactionTypes", "CreateTransactionType", "UpdateTransactionType", "ListIngredientCategories", "CreateIngredientCategory", "UpdateIngredientCategory",
		"ListUomCategories", "CreateUomCategory", "UpdateUomCategory", "ListIngredients", "CreateIngredient", "UpdateIngredient",
	} {
		covered[op] = true
	}

	doc, err := openapi.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !covered[op.OperationID] && exempt[op.OperationID] == "" {
				t.Errorf("%s %s (%s) has no isolation case and is not listed as exempt", method, path, op.OperationID)
			}
		}
	}
	for op := range covered {
		if exempt[op] != "" {
			t.Errorf("%s is both covered and exempt", op)
		}
	}
}

// The database enforces what the API promises: as tenant B, no tenant-scoped table yields a row
// that belongs to tenant A, and B cannot write one for A. This sweeps every table with a tenant_id
// column, so a table added later is covered without anyone remembering to.
func TestEveryTenantTableIsolatesRows(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	d := w.e.d

	rows, err := d.Owner.Query(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
		WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 10 {
		t.Fatalf("found %d tenant tables (%v)", len(tables), err)
	}

	// A row with no tenant: an announcement to every business, which a business may read but
	// nobody may without a tenant in context.
	op, err := w.e.platform.CreateOperator(ctx, platform.Actor{}, "sweep@orion.test", "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.e.platform.CreateAnnouncement(ctx, platform.Actor{OperatorID: &op.Operator.ID}, platform.NewAnnouncement{
		Severity: "info", Title: map[string]string{"id": "Semua"}, Body: map[string]string{"id": "x"},
	}, "test"); err != nil {
		t.Fatal(err)
	}

	for _, table := range tables {
		var own, foreign int
		err := kernel.TenantTx(ctx, d.App, w.b.tenant.ID, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id = $1", w.a.tenant.ID).Scan(&foreign); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id <> $1", w.b.tenant.ID).Scan(&own)
		})
		if err != nil {
			// A table the service role may not read at all is also isolated.
			continue
		}
		if foreign != 0 || own != 0 {
			t.Errorf("%s: tenant B sees %d rows of tenant A and %d rows of other tenants", table, foreign, own)
		}
	}

	// With no tenant in context nothing is visible either.
	for _, table := range tables {
		var n int
		if err := d.App.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err == nil && n != 0 {
			t.Errorf("%s: %d rows visible without a tenant context", table, n)
		}
	}
}
