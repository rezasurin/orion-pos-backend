package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// NewVariant describes a variant to create.
type NewVariant struct {
	Name      string
	SKU       *string
	Barcode   *string
	BasePrice kernel.Rupiah
}

// NewItem describes an item to create. At least one variant is required: the item is sold through
// its variants.
type NewItem struct {
	CategoryID       *uuid.UUID
	StationID        *uuid.UUID
	Name             string
	SKU              *string
	Barcode          *string
	ImageURL         *string
	TrackStock       bool
	Variants         []NewVariant
	ModifierGroupIDs []uuid.UUID
}

// CreateItem adds an item with its variants and modifier group links in one transaction.
func (s *Service) CreateItem(ctx context.Context, tenantID uuid.UUID, in NewItem) (Item, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := validName("name", in.Name, maxItemName); err != nil {
		return Item{}, err
	}
	var err error
	if in.SKU, err = optText("sku", in.SKU, maxCode); err != nil {
		return Item{}, err
	}
	if in.Barcode, err = optText("barcode", in.Barcode, maxCode); err != nil {
		return Item{}, err
	}
	if in.ImageURL, err = optText("image_url", in.ImageURL, maxImageURL); err != nil {
		return Item{}, err
	}
	if len(in.Variants) == 0 || len(in.Variants) > maxVariants {
		return Item{}, fmt.Errorf("%w: an item needs between 1 and %d variants", kernel.ErrValidation, maxVariants)
	}
	groups, err := cleanGroupIDs(in.ModifierGroupIDs)
	if err != nil {
		return Item{}, err
	}
	vs, err := variantColumns(in.Variants)
	if err != nil {
		return Item{}, err
	}

	id := kernel.NewID()
	var out Item
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		if _, err := q.InsertItem(ctx, db.InsertItemParams{
			ID: id, TenantID: tenantID, CategoryID: in.CategoryID, StationID: in.StationID, Name: in.Name, Sku: in.SKU, Barcode: in.Barcode,
			ImageUrl: in.ImageURL, TrackStock: in.TrackStock,
		}); err != nil {
			return mapErr(err)
		}
		vs.TenantID, vs.ItemID = tenantID, id
		if _, err := q.InsertVariants(ctx, vs); err != nil {
			return mapErr(err)
		}
		if len(groups) > 0 {
			if err := q.InsertItemModifierGroups(ctx, db.InsertItemModifierGroupsParams{TenantID: tenantID, ItemID: id, GroupIds: groups}); err != nil {
				return mapErr(err)
			}
		}
		if _, err := kernel.RecordChange(ctx, tx, EntityItem, id, "upsert", nil); err != nil {
			return err
		}
		out, err = loadItem(ctx, q, tenantID, id)
		return err
	})
	return out, err
}

// GetItem returns one item with its variants and modifier groups.
func (s *Service) GetItem(ctx context.Context, tenantID, id uuid.UUID) (Item, error) {
	var out Item
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		var err error
		out, err = loadItem(ctx, db.New(tx), tenantID, id)
		return err
	})
	return out, err
}

// ListItems returns a page of items ordered by id. Their variants and modifier group links come
// from two more queries, not one per item.
func (s *Service) ListItems(ctx context.Context, tenantID uuid.UUID, page kernel.Page, f ItemFilter) (kernel.Paged[Item], error) {
	var out kernel.Paged[Item]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListItems(ctx, db.ListItemsParams{
			TenantID: tenantID, After: page.After, IncludeArchived: f.IncludeArchived, CategoryID: f.CategoryID, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.Item) uuid.UUID { return r.ID })
		items, err := assembleItems(ctx, q, tenantID, paged.Items)
		if err != nil {
			return err
		}
		out = kernel.Paged[Item]{Items: items, Next: paged.Next}
		return nil
	})
	return out, err
}

// UpdateItem changes an item. Nil fields stay as they are. For the optional text fields (SKU,
// Barcode, ImageURL) a blank string clears the value. ModifierGroupIDs, when set, replaces the
// item's groups, in that display order.
type UpdateItem struct {
	Name             *string
	CategoryID       *uuid.UUID
	ClearCategory    bool
	StationID        *uuid.UUID
	ClearStation     bool
	SKU              *string
	Barcode          *string
	ImageURL         *string
	TrackStock       *bool
	Archived         *bool
	ModifierGroupIDs *[]uuid.UUID
}

// UpdateItem applies changes to one item.
func (s *Service) UpdateItem(ctx context.Context, tenantID, id uuid.UUID, in UpdateItem) (Item, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := validName("name", n, maxItemName); err != nil {
			return Item{}, err
		}
		in.Name = &n
	}
	if in.CategoryID != nil && in.ClearCategory {
		return Item{}, fmt.Errorf("%w: category_id and clear_category cannot be combined", kernel.ErrValidation)
	}
	if in.StationID != nil && in.ClearStation {
		return Item{}, fmt.Errorf("%w: station_id and clear_station cannot be combined", kernel.ErrValidation)
	}
	var err error
	// A pointer to a blank string means "clear", so keep it distinct from nil (no change).
	trim := func(field string, v *string, max int) (*string, error) {
		if v == nil {
			return nil, nil
		}
		t, err := optText(field, v, max)
		if err != nil {
			return nil, err
		}
		if t == nil {
			blank := ""
			return &blank, nil
		}
		return t, nil
	}
	if in.SKU, err = trim("sku", in.SKU, maxCode); err != nil {
		return Item{}, err
	}
	if in.Barcode, err = trim("barcode", in.Barcode, maxCode); err != nil {
		return Item{}, err
	}
	if in.ImageURL, err = trim("image_url", in.ImageURL, maxImageURL); err != nil {
		return Item{}, err
	}
	var groups []uuid.UUID
	if in.ModifierGroupIDs != nil {
		if groups, err = cleanGroupIDs(*in.ModifierGroupIDs); err != nil {
			return Item{}, err
		}
	}

	var out Item
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetItemForUpdate(ctx, db.GetItemForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateItemParams{
			TenantID: tenantID, ID: id, CategoryID: cur.CategoryID, StationID: cur.StationID, Name: cur.Name, Sku: cur.Sku, Barcode: cur.Barcode,
			ImageUrl: cur.ImageUrl, TrackStock: cur.TrackStock, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived),
		}
		if in.Name != nil {
			p.Name = *in.Name
		}
		switch {
		case in.ClearCategory:
			p.CategoryID = nil
		case in.CategoryID != nil:
			p.CategoryID = in.CategoryID
		}
		switch {
		case in.ClearStation:
			p.StationID = nil
		case in.StationID != nil:
			p.StationID = in.StationID
		}
		p.Sku, p.Barcode, p.ImageUrl = patchText(p.Sku, in.SKU), patchText(p.Barcode, in.Barcode), patchText(p.ImageUrl, in.ImageURL)
		if in.TrackStock != nil {
			p.TrackStock = *in.TrackStock
		}
		if _, err := q.UpdateItem(ctx, p); err != nil {
			return mapErr(err)
		}
		if in.ModifierGroupIDs != nil {
			if err := q.DeleteItemModifierGroups(ctx, db.DeleteItemModifierGroupsParams{TenantID: tenantID, ItemID: id}); err != nil {
				return err
			}
			if len(groups) > 0 {
				if err := q.InsertItemModifierGroups(ctx, db.InsertItemModifierGroupsParams{TenantID: tenantID, ItemID: id, GroupIds: groups}); err != nil {
					return mapErr(err)
				}
			}
		}
		if _, err := kernel.RecordChange(ctx, tx, EntityItem, id, "upsert", nil); err != nil {
			return err
		}
		out, err = loadItem(ctx, q, tenantID, id)
		return err
	})
	return out, err
}

// patchText applies an optional-text patch: nil leaves the value, blank clears it.
func patchText(cur, patch *string) *string {
	switch {
	case patch == nil:
		return cur
	case *patch == "":
		return nil
	}
	return patch
}

// AddVariant adds a variant to an existing item.
func (s *Service) AddVariant(ctx context.Context, tenantID, itemID uuid.UUID, in NewVariant) (Variant, error) {
	cols, err := variantColumns([]NewVariant{in})
	if err != nil {
		return Variant{}, err
	}
	var out Variant
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		if _, err := q.GetItemForUpdate(ctx, db.GetItemForUpdateParams{TenantID: tenantID, ID: itemID}); err != nil {
			return mapErr(err)
		}
		count, err := q.CountVariants(ctx, db.CountVariantsParams{TenantID: tenantID, ItemID: itemID})
		if err != nil {
			return err
		}
		if count >= maxVariants {
			return fmt.Errorf("%w: an item has at most %d variants", kernel.ErrValidation, maxVariants)
		}
		row, err := q.InsertVariant(ctx, db.InsertVariantParams{
			ID: kernel.NewID(), TenantID: tenantID, ItemID: itemID, Name: in.Name, Sku: optNonBlank(cols.Skus[0]),
			Barcode: optNonBlank(cols.Barcodes[0]), BasePrice: cols.BasePrices[0], SortOrder: int32(count), //nolint:gosec // at most maxVariants
		})
		if err != nil {
			return mapErr(err)
		}
		out = toVariant(row)
		_, err = kernel.RecordChange(ctx, tx, EntityItem, itemID, "upsert", nil)
		return err
	})
	return out, err
}

// UpdateVariant changes a variant. Nil fields stay as they are; a blank SKU or barcode clears it.
type UpdateVariant struct {
	Name      *string
	SKU       *string
	Barcode   *string
	BasePrice *kernel.Rupiah
	SortOrder *int
	Archived  *bool
}

// UpdateVariant applies changes to one variant. A changed base price is written to the audit log.
func (s *Service) UpdateVariant(ctx context.Context, tenantID, id uuid.UUID, in UpdateVariant) (Variant, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if len([]rune(n)) > maxName {
			return Variant{}, fmt.Errorf("%w: name is at most %d characters", kernel.ErrValidation, maxName)
		}
		in.Name = &n
	}
	var err error
	if in.SKU != nil {
		t := strings.TrimSpace(*in.SKU)
		if len([]rune(t)) > maxCode {
			return Variant{}, fmt.Errorf("%w: sku is at most %d characters", kernel.ErrValidation, maxCode)
		}
		in.SKU = &t
	}
	if in.Barcode != nil {
		t := strings.TrimSpace(*in.Barcode)
		if len([]rune(t)) > maxCode {
			return Variant{}, fmt.Errorf("%w: barcode is at most %d characters", kernel.ErrValidation, maxCode)
		}
		in.Barcode = &t
	}
	if in.BasePrice != nil {
		if err = validPrice("base_price", *in.BasePrice, false); err != nil {
			return Variant{}, err
		}
	}
	if in.SortOrder != nil {
		if err = validSort(*in.SortOrder); err != nil {
			return Variant{}, err
		}
	}

	var out Variant
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetVariantForUpdate(ctx, db.GetVariantForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateVariantParams{
			TenantID: tenantID, ID: id, Name: cur.Name, Sku: cur.Sku, Barcode: cur.Barcode, BasePrice: cur.BasePrice,
			SortOrder: cur.SortOrder, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived),
		}
		if in.Name != nil {
			p.Name = *in.Name
		}
		p.Sku, p.Barcode = patchText(p.Sku, in.SKU), patchText(p.Barcode, in.Barcode)
		if in.BasePrice != nil {
			p.BasePrice = int64(*in.BasePrice)
		}
		if in.SortOrder != nil {
			p.SortOrder = int32(*in.SortOrder) //nolint:gosec // validated above
		}
		row, err := q.UpdateVariant(ctx, p)
		if err != nil {
			return mapErr(err)
		}
		out = toVariant(row)
		if _, err := kernel.RecordChange(ctx, tx, EntityItem, cur.ItemID, "upsert", nil); err != nil {
			return err
		}
		if cur.BasePrice != row.BasePrice {
			return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
				Action: "catalog.price_changed", TargetType: "variant", TargetID: id,
				Detail: map[string]any{"scope": "base", "before": cur.BasePrice, "after": row.BasePrice},
			})
		}
		return nil
	})
	return out, err
}

// ---- loading ----

func loadItem(ctx context.Context, q *db.Queries, tenantID, id uuid.UUID) (Item, error) {
	row, err := q.GetItem(ctx, db.GetItemParams{TenantID: tenantID, ID: id})
	if err != nil {
		return Item{}, mapErr(err)
	}
	items, err := assembleItems(ctx, q, tenantID, []db.Item{row})
	if err != nil {
		return Item{}, err
	}
	return items[0], nil
}

// assembleItems attaches variants and modifier group links to items with two queries, however
// many items there are.
func assembleItems(ctx context.Context, q *db.Queries, tenantID uuid.UUID, rows []db.Item) ([]Item, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	variants, err := q.ListVariantsByItems(ctx, db.ListVariantsByItemsParams{TenantID: tenantID, ItemIds: ids})
	if err != nil {
		return nil, err
	}
	links, err := q.ListItemModifierGroups(ctx, db.ListItemModifierGroupsParams{TenantID: tenantID, ItemIds: ids})
	if err != nil {
		return nil, err
	}
	vByItem := map[uuid.UUID][]Variant{}
	for _, v := range variants {
		vByItem[v.ItemID] = append(vByItem[v.ItemID], toVariant(v))
	}
	gByItem := map[uuid.UUID][]uuid.UUID{}
	for _, l := range links {
		gByItem[l.ItemID] = append(gByItem[l.ItemID], l.GroupID)
	}
	out := make([]Item, len(rows))
	for i, r := range rows {
		out[i] = Item{
			ID: r.ID, CategoryID: r.CategoryID, StationID: r.StationID, Name: r.Name, SKU: r.Sku, Barcode: r.Barcode, ImageURL: r.ImageUrl,
			TrackStock: r.TrackStock, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt,
			Variants: orEmpty(vByItem[r.ID]), ModifierGroupIDs: orEmpty(gByItem[r.ID]),
		}
	}
	return out, nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func toVariant(r db.Variant) Variant {
	return Variant{
		ID: r.ID, ItemID: r.ItemID, Name: r.Name, SKU: r.Sku, Barcode: r.Barcode, BasePrice: kernel.Rupiah(r.BasePrice),
		SortOrder: int(r.SortOrder), ArchivedAt: r.ArchivedAt,
	}
}

// variantColumns validates variants and lays them out as the parallel arrays the bulk insert takes.
func variantColumns(vs []NewVariant) (db.InsertVariantsParams, error) {
	var p db.InsertVariantsParams
	for i := range vs {
		v := vs[i]
		name := strings.TrimSpace(v.Name)
		if len([]rune(name)) > maxName {
			return p, fmt.Errorf("%w: variant name is at most %d characters", kernel.ErrValidation, maxName)
		}
		if err := validPrice("base_price", v.BasePrice, false); err != nil {
			return p, err
		}
		sku, err := optText("sku", v.SKU, maxCode)
		if err != nil {
			return p, err
		}
		barcode, err := optText("barcode", v.Barcode, maxCode)
		if err != nil {
			return p, err
		}
		p.Ids = append(p.Ids, kernel.NewID())
		p.Names = append(p.Names, name)
		p.Skus = append(p.Skus, derefOr(sku))
		p.Barcodes = append(p.Barcodes, derefOr(barcode))
		p.BasePrices = append(p.BasePrices, int64(v.BasePrice))
	}
	return p, nil
}

func optNonBlank(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// cleanGroupIDs rejects duplicates and absurd counts, keeping the caller's order.
func cleanGroupIDs(ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) > maxItemGroups {
		return nil, fmt.Errorf("%w: an item has at most %d modifier groups", kernel.ErrValidation, maxItemGroups)
	}
	seen := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if slices.Contains(seen, id) {
			return nil, fmt.Errorf("%w: modifier group %s is listed twice", kernel.ErrValidation, id)
		}
		seen = append(seen, id)
	}
	return seen, nil
}

func validSort(v int) error {
	if v < -100000 || v > 100000 {
		return fmt.Errorf("%w: sort_order must be between -100000 and 100000", kernel.ErrValidation)
	}
	return nil
}
