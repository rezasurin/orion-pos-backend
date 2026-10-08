package api

import (
	"context"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/inventory"
)

func (s *Server) ListTransactionTypes(ctx context.Context, req openapi.ListTransactionTypesRequestObject) (openapi.ListTransactionTypesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListTransactionTypes(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	return openapi.ListTransactionTypes200JSONResponse{Items: mapSlice(res.Items, toTransactionTypeBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateTransactionType(ctx context.Context, req openapi.CreateTransactionTypeRequestObject) (openapi.CreateTransactionTypeResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	t, err := s.Inventory.CreateTransactionType(ctx, principalFrom(ctx).TenantID, inventory.NewTransactionType{
		Name: b.Name, Category: string(b.Category), Description: b.Description,
	})
	if err != nil {
		return nil, err
	}
	return openapi.CreateTransactionType201JSONResponse(toTransactionTypeBody(t)), nil
}

func (s *Server) UpdateTransactionType(ctx context.Context, req openapi.UpdateTransactionTypeRequestObject) (openapi.UpdateTransactionTypeResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	t, err := s.Inventory.UpdateTransactionType(ctx, principalFrom(ctx).TenantID, req.TransactionTypeId, inventory.UpdateTransactionType{
		Name: b.Name, Category: (*string)(b.Category), Description: b.Description, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateTransactionType200JSONResponse(toTransactionTypeBody(t)), nil
}

func (s *Server) ListIngredientCategories(ctx context.Context, req openapi.ListIngredientCategoriesRequestObject) (openapi.ListIngredientCategoriesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListIngredientCategories(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	return openapi.ListIngredientCategories200JSONResponse{Items: mapSlice(res.Items, toIngredientCategoryBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateIngredientCategory(ctx context.Context, req openapi.CreateIngredientCategoryRequestObject) (openapi.CreateIngredientCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	c, err := s.Inventory.CreateIngredientCategory(ctx, principalFrom(ctx).TenantID, inventory.NewIngredientCategory{
		Name: b.Name, DefaultTransactionTypeID: b.DefaultTransactionTypeId, Description: b.Description,
	})
	if err != nil {
		return nil, err
	}
	return openapi.CreateIngredientCategory201JSONResponse(toIngredientCategoryBody(c)), nil
}

func (s *Server) UpdateIngredientCategory(ctx context.Context, req openapi.UpdateIngredientCategoryRequestObject) (openapi.UpdateIngredientCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	c, err := s.Inventory.UpdateIngredientCategory(ctx, principalFrom(ctx).TenantID, req.IngredientCategoryId, inventory.UpdateIngredientCategory{
		Name: b.Name, DefaultTransactionTypeID: b.DefaultTransactionTypeId, ClearDefaultTransactionType: flag(b.ClearDefaultTransactionType),
		Description: b.Description, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateIngredientCategory200JSONResponse(toIngredientCategoryBody(c)), nil
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

func (s *Server) ListIngredients(ctx context.Context, req openapi.ListIngredientsRequestObject) (openapi.ListIngredientsResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Inventory.ListIngredients(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived), req.Params.CategoryId)
	if err != nil {
		return nil, err
	}
	return openapi.ListIngredients200JSONResponse{Items: mapSlice(res.Items, toIngredientBody), NextCursor: nextCursor(res.Next)}, nil
}

func (s *Server) CreateIngredient(ctx context.Context, req openapi.CreateIngredientRequestObject) (openapi.CreateIngredientResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	in := inventory.NewIngredient{Name: b.Name, CategoryID: b.CategoryId, UomID: b.UomId, Track: true, Description: b.Description}
	if b.Track != nil {
		in.Track = *b.Track
	}
	i, err := s.Inventory.CreateIngredient(ctx, principalFrom(ctx).TenantID, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateIngredient201JSONResponse(toIngredientBody(i)), nil
}

func (s *Server) UpdateIngredient(ctx context.Context, req openapi.UpdateIngredientRequestObject) (openapi.UpdateIngredientResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	i, err := s.Inventory.UpdateIngredient(ctx, principalFrom(ctx).TenantID, req.IngredientId, inventory.UpdateIngredient{
		Name: b.Name, CategoryID: b.CategoryId, ClearCategory: flag(b.ClearCategory), UomID: b.UomId, Track: b.Track,
		Description: b.Description, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateIngredient200JSONResponse(toIngredientBody(i)), nil
}

// fromUnitInput applies the spec's defaults: ratio 1/1, rounding 0.01, active.
func fromUnitInput(u openapi.UnitInput) inventory.UnitInput {
	return inventory.UnitInput{
		ID: u.Id, Name: u.Name, IsReference: flag(u.IsReference),
		RatioNum: orInt(u.RatioNum, 1), RatioDen: orInt(u.RatioDen, 1), RoundingScaled: orInt(u.RoundingScaled, 10),
		Active: u.Active == nil || *u.Active,
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

func toTransactionTypeBody(t inventory.TransactionType) openapi.TransactionType {
	return openapi.TransactionType{
		Id: t.ID, Name: t.Name, Category: openapi.TransactionTypeCategory(t.Category), Description: t.Description,
		ArchivedAt: t.ArchivedAt, CreatedAt: t.CreatedAt,
	}
}

func toIngredientCategoryBody(c inventory.IngredientCategory) openapi.IngredientCategory {
	return openapi.IngredientCategory{
		Id: c.ID, Name: c.Name, DefaultTransactionTypeId: c.DefaultTransactionTypeID, Description: c.Description,
		ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt,
	}
}

func toUomCategoryBody(c inventory.UomCategory) openapi.UomCategory {
	return openapi.UomCategory{
		Id: c.ID, Name: c.Name, ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt,
		Units: mapSlice(c.Units, func(u inventory.Uom) openapi.Uom {
			return openapi.Uom{
				Id: u.ID, Name: u.Name, IsReference: u.IsReference, RatioNum: u.RatioNum, RatioDen: u.RatioDen,
				RoundingScaled: u.RoundingScaled, Active: u.Active,
			}
		}),
	}
}

func toIngredientBody(i inventory.Ingredient) openapi.Ingredient {
	return openapi.Ingredient{
		Id: i.ID, Name: i.Name, CategoryId: i.CategoryID, UomId: i.UomID, Track: i.Track, Description: i.Description,
		ArchivedAt: i.ArchivedAt, CreatedAt: i.CreatedAt,
	}
}
