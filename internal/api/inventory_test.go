package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type uomBody struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Symbol         string `json:"symbol"`
	IsReference    bool   `json:"is_reference"`
	IsStandard     bool   `json:"is_standard"`
	RatioNum       int64  `json:"ratio_num"`
	RatioDen       int64  `json:"ratio_den"`
	RoundingScaled int64  `json:"rounding_scaled"`
	Active         bool   `json:"active"`
}

type uomCategoryBody struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	IsStandard bool      `json:"is_standard"`
	Units      []uomBody `json:"units"`
}

type stockItemBody struct {
	ID             string  `json:"id"`
	Type           string  `json:"type"`
	BaseUomID      string  `json:"base_uom_id"`
	RecipeUomID    *string `json:"recipe_uom_id"`
	Track          bool    `json:"track"`
	MinStockScaled *int64  `json:"min_stock_scaled"`
	Packs          []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		RatioNum       int64  `json:"ratio_num"`
		RatioDen       int64  `json:"ratio_den"`
		RoundingScaled int64  `json:"rounding_scaled"`
		Active         bool   `json:"active"`
	} `json:"packs"`
}

func TestInventorySetupEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken

	// The standard units are there from the start.
	var cats struct {
		Items []uomCategoryBody `json:"items"`
	}
	e.do(t, "GET", "/v1/uom-categories", tok, nil).decode(t, &cats)
	units := map[string]uomBody{}
	for _, c := range cats.Items {
		for _, u := range c.Units {
			units[u.Symbol] = u
		}
	}
	if len(cats.Items) != 3 || !units["kg"].IsStandard || units["kg"].RatioNum != 1000 || !units["g"].IsReference || units["g"].RoundingScaled != 1000 {
		t.Fatalf("standard units = %+v", cats.Items)
	}

	var et, sc categoryBody
	e.create(t, "/v1/expense-types", tok, map[string]any{"name": "Belanja Bahan Pasar", "group": "cost_of_goods"}, &et)
	e.do(t, "POST", "/v1/expense-types", tok, map[string]any{"name": "Bayar hutang", "group": "debt_payment"}).problem(t, http.StatusBadRequest, "validation_failed")
	e.create(t, "/v1/stock-categories", tok, map[string]any{"name": "Bahan Kopi", "default_expense_type_id": et.ID}, &sc)

	// Units default to 1/1, a step of 0.01 and active; ratios come back reduced.
	var spoons uomCategoryBody
	e.create(t, "/v1/uom-categories", tok, map[string]any{"name": "Takaran", "units": []map[string]any{
		{"name": "sendok besar", "symbol": "sdm", "ratio_num": 30, "ratio_den": 2}, {"name": "sendok teh", "symbol": "sdt", "is_reference": true},
	}}, &spoons)
	if len(spoons.Units) != 2 || spoons.Units[0].Symbol != "sdt" || spoons.Units[0].RoundingScaled != 10 || spoons.Units[1].RatioNum != 15 || spoons.Units[1].RatioDen != 1 {
		t.Fatalf("category = %+v", spoons)
	}
	e.do(t, "POST", "/v1/uom-categories", tok, map[string]any{"name": "Lain", "units": []map[string]any{{"name": "kilo", "symbol": "KG", "is_reference": true}}}).
		problem(t, http.StatusConflict, "conflict")

	// A stock item with its packs; packs default to whole packs, active.
	var beans stockItemBody
	e.create(t, "/v1/stock-items", tok, map[string]any{
		"name": "Biji kopi", "type": "ingredient", "category_id": sc.ID, "base_uom_id": units["g"].ID, "min_stock_scaled": 1_000_000,
		"packs": []map[string]any{{"name": "karung 25 kg", "ratio_num": 25000}},
	}, &beans)
	if !beans.Track || *beans.MinStockScaled != 1_000_000 || len(beans.Packs) != 1 || beans.Packs[0].RatioDen != 1 || beans.Packs[0].RoundingScaled != 1000 || !beans.Packs[0].Active {
		t.Errorf("item = %+v", beans)
	}
	e.do(t, "POST", "/v1/stock-items", tok, map[string]any{"name": "Gula", "type": "ingredient", "category_id": sc.ID, "base_uom_id": units["kg"].ID}).
		problem(t, http.StatusBadRequest, "validation_failed") // kg is not a reference unit
	e.do(t, "POST", "/v1/stock-items", tok, map[string]any{"name": "Gula", "type": "ingredient", "category_id": sc.ID, "base_uom_id": units["g"].ID, "min_stock_scaled": 1500}).
		problem(t, http.StatusBadRequest, "validation_failed") // 1.5 g is off the 1 g step
	e.do(t, "PATCH", "/v1/stock-items/"+beans.ID, tok, map[string]any{"base_uom_id": units["ml"].ID}).problem(t, http.StatusConflict, "conflict")
	e.do(t, "PATCH", "/v1/stock-items/"+uuid.NewString(), tok, map[string]any{"name": "xy"}).problem(t, http.StatusNotFound, "not_found")

	// A recipe unit locks its ratio; the pack keeps its ratio too.
	var sugar stockItemBody
	e.create(t, "/v1/stock-items", tok, map[string]any{"name": "Gula", "type": "ingredient", "category_id": sc.ID, "base_uom_id": spoons.Units[0].ID, "recipe_uom_id": spoons.Units[1].ID}, &sugar)
	e.do(t, "PATCH", "/v1/uom-categories/"+spoons.ID, tok, map[string]any{"units": []map[string]any{
		{"id": spoons.Units[1].ID, "name": "sendok besar", "symbol": "sdm", "ratio_num": 12},
	}}).problem(t, http.StatusConflict, "conflict")
	e.do(t, "PATCH", "/v1/stock-items/"+beans.ID, tok, map[string]any{"packs": []map[string]any{{"id": beans.Packs[0].ID, "name": "karung", "ratio_num": 50000}}}).
		problem(t, http.StatusBadRequest, "validation_failed")

	var page struct {
		Items []stockItemBody `json:"items"`
	}
	e.do(t, "GET", "/v1/stock-items?type=ingredient&category_id="+sc.ID, tok, nil).decode(t, &page)
	if len(page.Items) != 2 {
		t.Errorf("ingredients of the category = %+v", page.Items)
	}
	e.do(t, "GET", "/v1/stock-items?type=equipment", tok, nil).problem(t, http.StatusBadRequest, "validation_failed")
}

// A plan without the inventory module hides every inventory operation; the data stays.
func TestInventoryNeedsTheModule(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	var et categoryBody
	e.create(t, "/v1/expense-types", tok, map[string]any{"name": "Belanja", "group": "operating"}, &et)

	if _, err := e.d.Owner.Exec(context.Background(), `UPDATE tenant SET plan_id = (SELECT id FROM plan WHERE code = 'free') WHERE id = $1`, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	e.ents.Invalidate(f.tenant.ID) // an operator's plan change does this
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/expense-types"}, {"POST", "/v1/expense-types"}, {"PATCH", "/v1/expense-types/" + et.ID},
		{"GET", "/v1/stock-categories"}, {"POST", "/v1/stock-categories"}, {"PATCH", "/v1/stock-categories/" + uuid.NewString()},
		{"GET", "/v1/uom-categories"}, {"POST", "/v1/uom-categories"}, {"PATCH", "/v1/uom-categories/" + uuid.NewString()},
		{"GET", "/v1/stock-items"}, {"POST", "/v1/stock-items"}, {"PATCH", "/v1/stock-items/" + uuid.NewString()},
	} {
		e.do(t, call.method, call.path, tok, map[string]any{"name": "xy"}).problem(t, http.StatusForbidden, "module_disabled")
	}
	var n int
	if err := e.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM expense_type WHERE id = $1`, et.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("the data should stay: %d rows, %v", n, err)
	}
}
