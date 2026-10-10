package api

import (
	"context"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/inventory"
)

func (s *Server) ListExpenseTypes(ctx context.Context, req openapi.ListExpenseTypesRequestObject) (openapi.ListExpenseTypesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListExpenseTypes(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	return openapi.ListExpenseTypes200JSONResponse{Items: mapSlice(res.Items, toExpenseTypeBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateExpenseType(ctx context.Context, req openapi.CreateExpenseTypeRequestObject) (openapi.CreateExpenseTypeResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	t, err := s.Inventory.CreateExpenseType(ctx, principalFrom(ctx).TenantID, inventory.NewExpenseType{
		Name: b.Name, Group: string(b.Group), Description: b.Description,
	})
	if err != nil {
		return nil, err
	}
	return openapi.CreateExpenseType201JSONResponse(toExpenseTypeBody(t)), nil
}

func (s *Server) UpdateExpenseType(ctx context.Context, req openapi.UpdateExpenseTypeRequestObject) (openapi.UpdateExpenseTypeResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	t, err := s.Inventory.UpdateExpenseType(ctx, principalFrom(ctx).TenantID, req.ExpenseTypeId, inventory.UpdateExpenseType{
		Name: b.Name, Group: (*string)(b.Group), Description: b.Description, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateExpenseType200JSONResponse(toExpenseTypeBody(t)), nil
}

func (s *Server) ListStockCategories(ctx context.Context, req openapi.ListStockCategoriesRequestObject) (openapi.ListStockCategoriesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListStockCategories(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	return openapi.ListStockCategories200JSONResponse{Items: mapSlice(res.Items, toStockCategoryBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateStockCategory(ctx context.Context, req openapi.CreateStockCategoryRequestObject) (openapi.CreateStockCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	c, err := s.Inventory.CreateStockCategory(ctx, principalFrom(ctx).TenantID, inventory.NewStockCategory{
		Name: b.Name, DefaultExpenseTypeID: b.DefaultExpenseTypeId, Description: b.Description,
	})
	if err != nil {
		return nil, err
	}
	return openapi.CreateStockCategory201JSONResponse(toStockCategoryBody(c)), nil
}

func (s *Server) UpdateStockCategory(ctx context.Context, req openapi.UpdateStockCategoryRequestObject) (openapi.UpdateStockCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	c, err := s.Inventory.UpdateStockCategory(ctx, principalFrom(ctx).TenantID, req.StockCategoryId, inventory.UpdateStockCategory{
		Name: b.Name, DefaultExpenseTypeID: b.DefaultExpenseTypeId, ClearDefaultExpenseType: flag(b.ClearDefaultExpenseType),
		Description: b.Description, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateStockCategory200JSONResponse(toStockCategoryBody(c)), nil
}

func (s *Server) ListUomCategories(ctx context.Context, req openapi.ListUomCategoriesRequestObject) (openapi.ListUomCategoriesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListUomCategories(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	return openapi.ListUomCategories200JSONResponse{Items: mapSlice(res.Items, toUomCategoryBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateUomCategory(ctx context.Context, req openapi.CreateUomCategoryRequestObject) (openapi.CreateUomCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	c, err := s.Inventory.CreateUomCategory(ctx, principalFrom(ctx).TenantID, inventory.NewUomCategory{
		Name: req.Body.Name, Units: mapSlice(req.Body.Units, fromUnitInput),
	})
	if err != nil {
		return nil, err
	}
	return openapi.CreateUomCategory201JSONResponse(toUomCategoryBody(c)), nil
}

func (s *Server) UpdateUomCategory(ctx context.Context, req openapi.UpdateUomCategoryRequestObject) (openapi.UpdateUomCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	in := inventory.UpdateUomCategory{Name: b.Name, Archived: b.Archived}
	if b.Units != nil {
		in.Units = mapSlice(*b.Units, fromUnitInput)
	}
	c, err := s.Inventory.UpdateUomCategory(ctx, principalFrom(ctx).TenantID, req.UomCategoryId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateUomCategory200JSONResponse(toUomCategoryBody(c)), nil
}

func (s *Server) ListStockItems(ctx context.Context, req openapi.ListStockItemsRequestObject) (openapi.ListStockItemsResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListStockItems(ctx, principalFrom(ctx).TenantID, page, inventory.ItemFilter{
		IncludeArchived: flag(req.Params.IncludeArchived), CategoryID: req.Params.CategoryId, Type: (*string)(req.Params.Type),
	})
	if err != nil {
		return nil, err
	}
	return openapi.ListStockItems200JSONResponse{Items: mapSlice(res.Items, toStockItemBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateStockItem(ctx context.Context, req openapi.CreateStockItemRequestObject) (openapi.CreateStockItemResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	in := inventory.NewStockItem{
		Name: b.Name, Type: string(b.Type), CategoryID: b.CategoryId, BaseUomID: b.BaseUomId, RecipeUomID: b.RecipeUomId,
		Track: b.Track == nil || *b.Track, MinStockScaled: b.MinStockScaled, ShelfLifeDays: b.ShelfLifeDays, Description: b.Description,
	}
	if b.Packs != nil {
		in.Packs = mapSlice(*b.Packs, fromPackInput)
	}
	it, err := s.Inventory.CreateStockItem(ctx, principalFrom(ctx).TenantID, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateStockItem201JSONResponse(toStockItemBody(it)), nil
}

func (s *Server) UpdateStockItem(ctx context.Context, req openapi.UpdateStockItemRequestObject) (openapi.UpdateStockItemResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	in := inventory.UpdateStockItem{
		Name: b.Name, Type: (*string)(b.Type), CategoryID: b.CategoryId, BaseUomID: b.BaseUomId, RecipeUomID: b.RecipeUomId,
		ClearRecipeUom: flag(b.ClearRecipeUom), Track: b.Track, MinStockScaled: b.MinStockScaled, ShelfLifeDays: b.ShelfLifeDays,
		Description: b.Description, Archived: b.Archived,
	}
	if b.Packs != nil {
		in.Packs = mapSlice(*b.Packs, fromPackInput)
	}
	it, err := s.Inventory.UpdateStockItem(ctx, principalFrom(ctx).TenantID, req.StockItemId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateStockItem200JSONResponse(toStockItemBody(it)), nil
}

// fromUnitInput applies the spec's defaults: ratio 1/1, a step of 0.01, active.
func fromUnitInput(u openapi.UnitInput) inventory.UnitInput {
	return inventory.UnitInput{
		ID: u.Id, Name: u.Name, Symbol: u.Symbol, IsReference: flag(u.IsReference),
		RatioNum: orInt(u.RatioNum, 1), RatioDen: orInt(u.RatioDen, 1), RoundingScaled: orInt(u.RoundingScaled, 10),
		Active: u.Active == nil || *u.Active,
	}
}

// fromPackInput applies the spec's defaults: a denominator of 1, whole packs, active.
func fromPackInput(p openapi.PackInput) inventory.PackInput {
	return inventory.PackInput{
		ID: p.Id, Name: p.Name, RatioNum: p.RatioNum, RatioDen: orInt(p.RatioDen, 1), RoundingScaled: orInt(p.RoundingScaled, 1000),
		Active: p.Active == nil || *p.Active,
	}
}

func orInt(v *int64, def int64) int64 {
	if v == nil {
		return def
	}
	return *v
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func toExpenseTypeBody(t inventory.ExpenseType) openapi.ExpenseType {
	return openapi.ExpenseType{
		Id: t.ID, Name: t.Name, Group: openapi.ExpenseGroup(t.Group), Description: t.Description, ArchivedAt: t.ArchivedAt, CreatedAt: t.CreatedAt,
	}
}

func toStockCategoryBody(c inventory.StockCategory) openapi.StockCategory {
	return openapi.StockCategory{
		Id: c.ID, Name: c.Name, DefaultExpenseTypeId: c.DefaultExpenseTypeID, Description: c.Description, ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt,
	}
}

func toUomCategoryBody(c inventory.UomCategory) openapi.UomCategory {
	return openapi.UomCategory{
		Id: c.ID, Name: c.Name, IsStandard: c.IsStandard, ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt,
		Units: mapSlice(c.Units, func(u inventory.Uom) openapi.Uom {
			return openapi.Uom{
				Id: u.ID, Name: u.Name, Symbol: u.Symbol, IsReference: u.IsReference, IsStandard: u.IsStandard,
				RatioNum: u.RatioNum, RatioDen: u.RatioDen, RoundingScaled: u.RoundingScaled, Active: u.Active,
			}
		}),
	}
}

func toStockItemBody(i inventory.StockItem) openapi.StockItem {
	return openapi.StockItem{
		Id: i.ID, Name: i.Name, Type: openapi.StockItemType(i.Type), CategoryId: i.CategoryID, BaseUomId: i.BaseUomID, RecipeUomId: i.RecipeUomID,
		Track: i.Track, MinStockScaled: i.MinStockScaled, ShelfLifeDays: i.ShelfLifeDays, Description: i.Description,
		ArchivedAt: i.ArchivedAt, CreatedAt: i.CreatedAt,
		Packs: mapSlice(i.Packs, func(p inventory.Pack) openapi.Pack {
			return openapi.Pack{Id: p.ID, Name: p.Name, RatioNum: p.RatioNum, RatioDen: p.RatioDen, RoundingScaled: p.RoundingScaled, Active: p.Active}
		}),
	}
}
