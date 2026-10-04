package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

func (s *Server) ListCategories(ctx context.Context, req openapi.ListCategoriesRequestObject) (openapi.ListCategoriesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Catalog.ListCategories(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	out := openapi.ListCategories200JSONResponse{Items: make([]openapi.Category, len(res.Items)), NextCursor: nextCursor(res.Next)}
	for i, c := range res.Items {
		out.Items[i] = toCategoryBody(c)
	}
	return out, nil
}

func (s *Server) CreateCategory(ctx context.Context, req openapi.CreateCategoryRequestObject) (openapi.CreateCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	in := catalog.NewCategory{Name: req.Body.Name}
	if req.Body.SortOrder != nil {
		in.SortOrder = *req.Body.SortOrder
	}
	c, err := s.Catalog.CreateCategory(ctx, principalFrom(ctx).TenantID, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateCategory201JSONResponse(toCategoryBody(c)), nil
}

func (s *Server) UpdateCategory(ctx context.Context, req openapi.UpdateCategoryRequestObject) (openapi.UpdateCategoryResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	b := req.Body
	c, err := s.Catalog.UpdateCategory(ctx, principalFrom(ctx).TenantID, req.CategoryId, catalog.UpdateCategory{
		Name: b.Name, SortOrder: b.SortOrder, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateCategory200JSONResponse(toCategoryBody(c)), nil
}

func (s *Server) ListItems(ctx context.Context, req openapi.ListItemsRequestObject) (openapi.ListItemsResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Catalog.ListItems(ctx, principalFrom(ctx).TenantID, page, catalog.ItemFilter{
		IncludeArchived: flag(req.Params.IncludeArchived), CategoryID: req.Params.CategoryId,
	})
	if err != nil {
		return nil, err
	}
	out := openapi.ListItems200JSONResponse{Items: make([]openapi.Item, len(res.Items)), NextCursor: nextCursor(res.Next)}
	for i, it := range res.Items {
		out.Items[i] = toItemBody(it)
	}
	return out, nil
}

func (s *Server) CreateItem(ctx context.Context, req openapi.CreateItemRequestObject) (openapi.CreateItemResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	in := catalog.NewItem{
		CategoryID: b.CategoryId, Name: b.Name, SKU: b.Sku, Barcode: b.Barcode, ImageURL: b.ImageUrl,
		TrackStock: flag(b.TrackStock), Variants: make([]catalog.NewVariant, len(b.Variants)),
	}
	for i, v := range b.Variants {
		in.Variants[i] = fromNewVariant(v)
	}
	if b.ModifierGroupIds != nil {
		in.ModifierGroupIDs = *b.ModifierGroupIds
	}
	it, err := s.Catalog.CreateItem(ctx, principalFrom(ctx).TenantID, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateItem201JSONResponse(toItemBody(it)), nil
}

func (s *Server) GetItem(ctx context.Context, req openapi.GetItemRequestObject) (openapi.GetItemResponseObject, error) {
	it, err := s.Catalog.GetItem(ctx, principalFrom(ctx).TenantID, req.ItemId)
	if err != nil {
		return nil, err
	}
	return openapi.GetItem200JSONResponse(toItemBody(it)), nil
}

func (s *Server) UpdateItem(ctx context.Context, req openapi.UpdateItemRequestObject) (openapi.UpdateItemResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	it, err := s.Catalog.UpdateItem(ctx, principalFrom(ctx).TenantID, req.ItemId, catalog.UpdateItem{
		Name: b.Name, CategoryID: b.CategoryId, ClearCategory: flag(b.ClearCategory), SKU: b.Sku, Barcode: b.Barcode,
		ImageURL: b.ImageUrl, TrackStock: b.TrackStock, Archived: b.Archived, ModifierGroupIDs: b.ModifierGroupIds,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateItem200JSONResponse(toItemBody(it)), nil
}

func (s *Server) AddVariant(ctx context.Context, req openapi.AddVariantRequestObject) (openapi.AddVariantResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	v, err := s.Catalog.AddVariant(ctx, principalFrom(ctx).TenantID, req.ItemId, fromNewVariant(*req.Body))
	if err != nil {
		return nil, err
	}
	return openapi.AddVariant201JSONResponse(toVariantBody(v)), nil
}

func (s *Server) UpdateVariant(ctx context.Context, req openapi.UpdateVariantRequestObject) (openapi.UpdateVariantResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	in := catalog.UpdateVariant{Name: b.Name, SKU: b.Sku, Barcode: b.Barcode, SortOrder: b.SortOrder, Archived: b.Archived}
	if b.BasePrice != nil {
		p := kernel.Rupiah(*b.BasePrice)
		in.BasePrice = &p
	}
	v, err := s.Catalog.UpdateVariant(ctx, principalFrom(ctx).TenantID, req.VariantId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateVariant200JSONResponse(toVariantBody(v)), nil
}

func (s *Server) ListModifierGroups(ctx context.Context, req openapi.ListModifierGroupsRequestObject) (openapi.ListModifierGroupsResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Catalog.ListModifierGroups(ctx, principalFrom(ctx).TenantID, page, flag(req.Params.IncludeArchived))
	if err != nil {
		return nil, err
	}
	out := openapi.ListModifierGroups200JSONResponse{Items: make([]openapi.ModifierGroup, len(res.Items)), NextCursor: nextCursor(res.Next)}
	for i, g := range res.Items {
		out.Items[i] = toGroupBody(g)
	}
	return out, nil
}

func (s *Server) CreateModifierGroup(ctx context.Context, req openapi.CreateModifierGroupRequestObject) (openapi.CreateModifierGroupResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	in := catalog.NewModifierGroup{Name: b.Name, MaxSelect: 1, Required: flag(b.Required)}
	if b.MinSelect != nil {
		in.MinSelect = *b.MinSelect
	}
	if b.MaxSelect != nil {
		in.MaxSelect = *b.MaxSelect
	}
	if b.Modifiers != nil {
		for _, m := range *b.Modifiers {
			in.Modifiers = append(in.Modifiers, fromNewModifier(m))
		}
	}
	g, err := s.Catalog.CreateModifierGroup(ctx, principalFrom(ctx).TenantID, in)
	if err != nil {
		return nil, err
	}
	return openapi.CreateModifierGroup201JSONResponse(toGroupBody(g)), nil
}

func (s *Server) UpdateModifierGroup(ctx context.Context, req openapi.UpdateModifierGroupRequestObject) (openapi.UpdateModifierGroupResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	g, err := s.Catalog.UpdateModifierGroup(ctx, principalFrom(ctx).TenantID, req.GroupId, catalog.UpdateModifierGroup{
		Name: b.Name, MinSelect: b.MinSelect, MaxSelect: b.MaxSelect, Required: b.Required, Archived: b.Archived,
	})
	if err != nil {
		return nil, err
	}
	return openapi.UpdateModifierGroup200JSONResponse(toGroupBody(g)), nil
}

func (s *Server) AddModifier(ctx context.Context, req openapi.AddModifierRequestObject) (openapi.AddModifierResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	m, err := s.Catalog.AddModifier(ctx, principalFrom(ctx).TenantID, req.GroupId, fromNewModifier(*req.Body))
	if err != nil {
		return nil, err
	}
	return openapi.AddModifier201JSONResponse(toModifierBody(m)), nil
}

func (s *Server) UpdateModifier(ctx context.Context, req openapi.UpdateModifierRequestObject) (openapi.UpdateModifierResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	in := catalog.UpdateModifier{Name: b.Name, SortOrder: b.SortOrder, Archived: b.Archived}
	if b.PriceDelta != nil {
		p := kernel.Rupiah(*b.PriceDelta)
		in.PriceDelta = &p
	}
	m, err := s.Catalog.UpdateModifier(ctx, principalFrom(ctx).TenantID, req.ModifierId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateModifier200JSONResponse(toModifierBody(m)), nil
}

func (s *Server) ListOutletVariants(ctx context.Context, req openapi.ListOutletVariantsRequestObject) (openapi.ListOutletVariantsResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	tenantID, err := s.outletForCatalog(ctx, req.OutletId)
	if err != nil {
		return nil, err
	}
	res, err := s.Catalog.ListOutletVariants(ctx, tenantID, req.OutletId, page)
	if err != nil {
		return nil, err
	}
	out := openapi.ListOutletVariants200JSONResponse{Items: make([]openapi.OutletVariant, len(res.Items)), NextCursor: nextCursor(res.Next)}
	for i, v := range res.Items {
		out.Items[i] = toOutletVariantBody(v)
	}
	return out, nil
}

func (s *Server) SetOutletVariant(ctx context.Context, req openapi.SetOutletVariantRequestObject) (openapi.SetOutletVariantResponseObject, error) {
	if req.Body == nil {
		return nil, errBodyRequired
	}
	tenantID, err := s.outletForCatalog(ctx, req.OutletId)
	if err != nil {
		return nil, err
	}
	in := catalog.SetOutletVariant{Available: req.Body.Available}
	if req.Body.PriceOverride != nil {
		p := kernel.Rupiah(*req.Body.PriceOverride)
		in.PriceOverride = &p
	}
	v, err := s.Catalog.SetOutletVariant(ctx, tenantID, req.OutletId, req.VariantId, in)
	if err != nil {
		return nil, err
	}
	return openapi.SetOutletVariant200JSONResponse(toOutletVariantBody(v)), nil
}

// outletForCatalog checks that the outlet exists in the caller's business (another business's
// outlet is "not found") and that the caller may manage the catalog there. The middleware only
// knows the permission at some outlet.
func (s *Server) outletForCatalog(ctx context.Context, outletID uuid.UUID) (uuid.UUID, error) {
	tenantID := principalFrom(ctx).TenantID
	if _, err := s.Tenancy.GetOutlet(ctx, tenantID, outletID); err != nil {
		return uuid.Nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermCatalogManage, outletID) {
		return uuid.Nil, &identity.ForbiddenError{Permission: string(identity.PermCatalogManage), Reason: "catalog.manage is needed at this outlet"}
	}
	return tenantID, nil
}

var errBodyRequired = fmt.Errorf("%w: a body is required", kernel.ErrValidation)

func flag(b *bool) bool { return b != nil && *b }

func nextCursor(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func fromNewVariant(v openapi.NewVariant) catalog.NewVariant {
	out := catalog.NewVariant{SKU: v.Sku, Barcode: v.Barcode, BasePrice: kernel.Rupiah(v.BasePrice)}
	if v.Name != nil {
		out.Name = *v.Name
	}
	return out
}

func fromNewModifier(m openapi.NewModifier) catalog.NewModifier {
	out := catalog.NewModifier{Name: m.Name}
	if m.PriceDelta != nil {
		out.PriceDelta = kernel.Rupiah(*m.PriceDelta)
	}
	return out
}

func toCategoryBody(c catalog.Category) openapi.Category {
	return openapi.Category{Id: c.ID, Name: c.Name, SortOrder: c.SortOrder, ArchivedAt: c.ArchivedAt, CreatedAt: c.CreatedAt}
}

func toVariantBody(v catalog.Variant) openapi.Variant {
	return openapi.Variant{
		Id: v.ID, ItemId: v.ItemID, Name: v.Name, Sku: v.SKU, Barcode: v.Barcode, BasePrice: int64(v.BasePrice),
		SortOrder: v.SortOrder, ArchivedAt: v.ArchivedAt,
	}
}

func toItemBody(it catalog.Item) openapi.Item {
	vs := make([]openapi.Variant, len(it.Variants))
	for i, v := range it.Variants {
		vs[i] = toVariantBody(v)
	}
	return openapi.Item{
		Id: it.ID, CategoryId: it.CategoryID, Name: it.Name, Sku: it.SKU, Barcode: it.Barcode, ImageUrl: it.ImageURL,
		TrackStock: it.TrackStock, ArchivedAt: it.ArchivedAt, CreatedAt: it.CreatedAt, Variants: vs, ModifierGroupIds: it.ModifierGroupIDs,
	}
}

func toModifierBody(m catalog.Modifier) openapi.Modifier {
	return openapi.Modifier{
		Id: m.ID, GroupId: m.GroupID, Name: m.Name, PriceDelta: int64(m.PriceDelta), SortOrder: m.SortOrder, ArchivedAt: m.ArchivedAt,
	}
}

func toGroupBody(g catalog.ModifierGroup) openapi.ModifierGroup {
	ms := make([]openapi.Modifier, len(g.Modifiers))
	for i, m := range g.Modifiers {
		ms[i] = toModifierBody(m)
	}
	return openapi.ModifierGroup{
		Id: g.ID, Name: g.Name, MinSelect: g.MinSelect, MaxSelect: g.MaxSelect, Required: g.Required,
		ArchivedAt: g.ArchivedAt, CreatedAt: g.CreatedAt, Modifiers: ms,
	}
}

func toOutletVariantBody(v catalog.OutletVariant) openapi.OutletVariant {
	out := openapi.OutletVariant{OutletId: v.OutletID, VariantId: v.VariantID, Available: v.Available}
	if v.PriceOverride != nil {
		p := int64(*v.PriceOverride)
		out.PriceOverride = &p
	}
	return out
}
