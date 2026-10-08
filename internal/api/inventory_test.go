package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type uomCategoryBody struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Units []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		IsReference    bool   `json:"is_reference"`
		RatioNum       int64  `json:"ratio_num"`
		RatioDen       int64  `json:"ratio_den"`
		RoundingScaled int64  `json:"rounding_scaled"`
		Active         bool   `json:"active"`
	} `json:"units"`
}

type ingredientBody struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	CategoryID  *string `json:"category_id"`
	UomID       string  `json:"uom_id"`
	Track       bool    `json:"track"`
	Description *string `json:"description"`
}

func TestInventorySetupEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken

	var tt, ic categoryBody
	e.create(t, "/v1/transaction-types", tok, map[string]any{"name": "Belanja Bahan Pasar", "category": "materials"}, &tt)
	e.create(t, "/v1/ingredient-categories", tok, map[string]any{"name": "Bahan Kopi", "default_transaction_type_id": tt.ID}, &ic)

	// Units default to 1/1, a rounding of 0.01 and active; ratios come back reduced.
	var uc uomCategoryBody
	e.create(t, "/v1/uom-categories", tok, map[string]any{"name": "Berat", "units": []map[string]any{
		{"name": "kg", "ratio_num": 2000, "ratio_den": 2}, {"name": "g", "is_reference": true},
	}}, &uc)
	if len(uc.Units) != 2 || uc.Units[0].Name != "g" || !uc.Units[0].IsReference || uc.Units[0].RoundingScaled != 10 || !uc.Units[0].Active ||
		uc.Units[1].RatioNum != 1000 || uc.Units[1].RatioDen != 1 {
		t.Fatalf("category = %+v", uc)
	}
	e.do(t, "POST", "/v1/uom-categories", tok, map[string]any{"name": "Rusak", "units": []map[string]any{{"name": "g"}}}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/uom-categories", tok, map[string]any{"name": "berat", "units": []map[string]any{{"name": "g", "is_reference": true}}}).
		problem(t, http.StatusConflict, "conflict")

	var ing ingredientBody
	e.create(t, "/v1/ingredients", tok, map[string]any{"name": "Biji kopi", "category_id": ic.ID, "uom_id": uc.Units[1].ID}, &ing)
	if !ing.Track || ing.UomID != uc.Units[1].ID || *ing.CategoryID != ic.ID {
		t.Errorf("ingredient = %+v (track defaults to true)", ing)
	}
	e.do(t, "PATCH", "/v1/ingredients/"+ing.ID, tok, map[string]any{"uom_id": uuid.NewString()}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "PATCH", "/v1/ingredients/"+uuid.NewString(), tok, map[string]any{"name": "x"}).problem(t, http.StatusNotFound, "not_found")

	// Deactivating kg leaves the ingredient on it, and adding a unit keeps the others.
	r := e.do(t, "PATCH", "/v1/uom-categories/"+uc.ID, tok, map[string]any{"units": []map[string]any{
		{"id": uc.Units[1].ID, "name": "kg", "ratio_num": 1000, "active": false}, {"name": "ons", "ratio_num": 100},
	}})
	if r.Code != http.StatusOK {
		t.Fatalf("update: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &uc)
	if len(uc.Units) != 3 || uc.Units[1].Name != "ons" || uc.Units[2].Active {
		t.Errorf("after update = %+v", uc)
	}

	var page struct {
		Items []ingredientBody `json:"items"`
	}
	e.do(t, "GET", "/v1/ingredients?category_id="+ic.ID, tok, nil).decode(t, &page)
	if len(page.Items) != 1 || page.Items[0].ID != ing.ID {
		t.Errorf("ingredients of the category = %+v", page.Items)
	}
}

// A plan without the inventory module hides every inventory operation; the data stays.
func TestInventoryNeedsTheModule(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	var uc uomCategoryBody
	e.create(t, "/v1/uom-categories", tok, map[string]any{"name": "Berat", "units": []map[string]any{{"name": "g", "is_reference": true}}}, &uc)

	if _, err := e.d.Owner.Exec(context.Background(), `UPDATE tenant SET plan_id = (SELECT id FROM plan WHERE code = 'free') WHERE id = $1`, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	e.ents.Invalidate(f.tenant.ID) // an operator's plan change does this
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/transaction-types"}, {"POST", "/v1/transaction-types"}, {"PATCH", "/v1/transaction-types/" + uuid.NewString()},
		{"GET", "/v1/ingredient-categories"}, {"POST", "/v1/ingredient-categories"}, {"PATCH", "/v1/ingredient-categories/" + uuid.NewString()},
		{"GET", "/v1/uom-categories"}, {"POST", "/v1/uom-categories"}, {"PATCH", "/v1/uom-categories/" + uc.ID},
		{"GET", "/v1/ingredients"}, {"POST", "/v1/ingredients"}, {"PATCH", "/v1/ingredients/" + uuid.NewString()},
	} {
		e.do(t, call.method, call.path, tok, map[string]any{"name": "x"}).problem(t, http.StatusForbidden, "module_disabled")
	}
	var n int
	if err := e.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM uom WHERE category_id = $1`, uc.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the data should stay: %d units, %v", n, err)
	}
}
