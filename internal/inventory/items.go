package inventory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Stock item types (BR-ITM-01). Equipment is an asset, not an item.
const (
	TypeIngredient = "ingredient" // bahan baku
	TypeSupporting = "supporting" // bahan penolong
	TypePrepared   = "prepared"   // setengah jadi, made in the kitchen
	TypeFinished   = "finished"   // barang jadi, bought ready to sell
	TypeSupply     = "supply"     // perlengkapan, used up but not sold
)

// StockItem is anything the business buys, stores, prepares or uses (BR-ITM). Menu items belong
// to the catalog. Its quantities are stored in thousandths of its base unit, a reference unit
// (ADR 0009).
type StockItem struct {
	ID             uuid.UUID
	Name           string
	Type           string
	CategoryID     uuid.UUID
	BaseUomID      uuid.UUID
	RecipeUomID    *uuid.UUID // nil: recipes use the base unit
	Track          bool
	MinStockScaled *int64 // reorder level in base thousandths (BR-ITM-04); nil is no alert
	ShelfLifeDays  *int   // BR-ITM-05
	Description    *string
	ArchivedAt     *time.Time
	CreatedAt      time.Time
	Packs          []Pack // from small to large
}

// Pack is an item's own packaging (BR-UOM-04): one pack is RatioNum/RatioDen of the item's base
// unit, a reduced fraction that never changes once the pack exists (BR-UOM-07).
type Pack struct {
	ID             uuid.UUID
	Name           string
	RatioNum       int64
	RatioDen       int64
	RoundingScaled int64 // the step, in thousandths of a pack
	Active         bool
}

// PackInput is the whole state of a pack as the client sends it. ID is nil for a new pack; an
// existing pack must be sent with its ratio unchanged.
type PackInput struct {
	ID             *uuid.UUID
	Name           string
	RatioNum       int64
	RatioDen       int64
	RoundingScaled int64
	Active         bool
}

// NewStockItem describes a stock item to create.
type NewStockItem struct {
	Name           string
	Type           string
	CategoryID     uuid.UUID
	BaseUomID      uuid.UUID
	RecipeUomID    *uuid.UUID
	Track          bool
	MinStockScaled *int64
	ShelfLifeDays  *int
	Description    *string
	Packs          []PackInput
}

// UpdateStockItem changes a stock item. Nil fields stay as they are. A blank description, a
// minimum stock of 0 and a shelf life of 0 clear those; ClearRecipeUom goes back to the base unit.
// Packs listed with an id are replaced by what is sent, packs without one are added, packs left out
// stay. Turning tracking off clears the minimum stock, which means nothing without a balance.
type UpdateStockItem struct {
	Name           *string
	Type           *string
	CategoryID     *uuid.UUID
	BaseUomID      *uuid.UUID
	RecipeUomID    *uuid.UUID
	ClearRecipeUom bool
	Track          *bool
	MinStockScaled *int64
	ShelfLifeDays  *int
	Description    *string
	Archived       *bool
	Packs          []PackInput
}

// ItemFilter narrows a list of stock items.
type ItemFilter struct {
	IncludeArchived bool
	CategoryID      *uuid.UUID
	Type            *string
}

// CreateStockItem adds a stock item with its packs.
func (s *Service) CreateStockItem(ctx context.Context, tenantID uuid.UUID, in NewStockItem) (StockItem, error) {
	name, desc, err := validNameDesc(in.Name, in.Description)
	if err != nil {
		return StockItem{}, err
	}
	if err := validType(in.Type); err != nil {
		return StockItem{}, err
	}
	if err := validShelfLife(in.ShelfLifeDays); err != nil {
		return StockItem{}, err
	}
	for i := range in.Packs {
		if in.Packs[i].ID != nil {
			return StockItem{}, fmt.Errorf("%w: a new item's packs have no id", kernel.ErrValidation)
		}
		if err := normalizePack(&in.Packs[i]); err != nil {
			return StockItem{}, err
		}
	}
	if len(in.Packs) > maxPacks {
		return StockItem{}, fmt.Errorf("%w: an item has at most %d packs", kernel.ErrValidation, maxPacks)
	}
	minStock := zeroIsNil(in.MinStockScaled)
	var out StockItem
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := checkItem(ctx, q, tenantID, &in.CategoryID, in.BaseUomID, in.RecipeUomID, in.Track, minStock); err != nil {
			return err
		}
		row, err := q.InsertStockItem(ctx, db.InsertStockItemParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, Type: in.Type, CategoryID: in.CategoryID, BaseUomID: in.BaseUomID,
			RecipeUomID: in.RecipeUomID, Track: in.Track, MinStockScaled: minStock, ShelfLifeDays: days(in.ShelfLifeDays), Description: desc,
		})
		if err != nil {
			return mapErr(err)
		}
		if err := insertPacks(ctx, q, tenantID, row.ID, in.Packs); err != nil {
			return err
		}
		out, err = loadItem(ctx, q, tenantID, row)
		return err
	})
	return out, err
}

// ListStockItems returns a page of stock items ordered by id, each with its packs.
func (s *Service) ListStockItems(ctx context.Context, tenantID uuid.UUID, page kernel.Page, f ItemFilter) (kernel.Paged[StockItem], error) {
	var out kernel.Paged[StockItem]
	if f.Type != nil {
		if err := validType(*f.Type); err != nil {
			return out, err
		}
	}
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListStockItems(ctx, db.ListStockItemsParams{
			TenantID: tenantID, After: page.After, IncludeArchived: f.IncludeArchived, CategoryID: f.CategoryID, Type: f.Type, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out, err = withPacks(ctx, q, tenantID, kernel.Trim(page, rows, func(r db.StockItem) uuid.UUID { return r.ID }))
		return err
	})
	return out, err
}

// UpdateStockItem applies changes to one stock item and its packs.
//
// ponytail: the base unit may change while the item has no packs, since nothing is stored in it
// yet. B3.3 must also refuse it once the item has a ledger row or a recipe line (BR-ITM-07).
func (s *Service) UpdateStockItem(ctx context.Context, tenantID, id uuid.UUID, in UpdateStockItem) (StockItem, error) {
	if in.Type != nil {
		if err := validType(*in.Type); err != nil {
			return StockItem{}, err
		}
	}
	if err := validShelfLife(in.ShelfLifeDays); err != nil {
		return StockItem{}, err
	}
	for i := range in.Packs {
		if err := normalizePack(&in.Packs[i]); err != nil {
			return StockItem{}, err
		}
	}
	var out StockItem
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetStockItemForUpdate(ctx, db.GetStockItemForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateStockItemParams{
			TenantID: tenantID, ID: id, Type: cur.Type, CategoryID: cur.CategoryID, BaseUomID: cur.BaseUomID, RecipeUomID: cur.RecipeUomID,
			Track: cur.Track, MinStockScaled: cur.MinStockScaled, ShelfLifeDays: cur.ShelfLifeDays, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived),
		}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description); err != nil {
			return err
		}
		if in.Type != nil {
			p.Type = *in.Type
		}
		if in.Track != nil {
			p.Track = *in.Track
		}
		if in.MinStockScaled != nil {
			p.MinStockScaled = zeroIsNil(in.MinStockScaled)
		}
		if !p.Track && zeroIsNil(in.MinStockScaled) == nil {
			p.MinStockScaled = nil // a minimum asked for in this request is refused below instead
		}
		if in.ShelfLifeDays != nil {
			p.ShelfLifeDays = days(in.ShelfLifeDays)
		}
		switch {
		case in.ClearRecipeUom:
			p.RecipeUomID = nil
		case in.RecipeUomID != nil:
			p.RecipeUomID = in.RecipeUomID
		}

		packs, err := q.ListPacksByItems(ctx, db.ListPacksByItemsParams{TenantID: tenantID, ItemIds: []uuid.UUID{id}})
		if err != nil {
			return err
		}
		if in.BaseUomID != nil && *in.BaseUomID != cur.BaseUomID {
			if len(packs) > 0 {
				return fmt.Errorf("%w: the base unit cannot change while the item has packs, which are measured in it", kernel.ErrConflict)
			}
			p.BaseUomID = *in.BaseUomID
		}
		// Only what changed is checked, so an item may keep a category or unit archived after it
		// was given; the base and recipe units are checked together because they must agree.
		var newCategory *uuid.UUID
		if in.CategoryID != nil && *in.CategoryID != cur.CategoryID {
			newCategory, p.CategoryID = in.CategoryID, *in.CategoryID
		}
		unitsChanged := p.BaseUomID != cur.BaseUomID || !sameID(p.RecipeUomID, cur.RecipeUomID)
		minChanged := !sameInt(p.MinStockScaled, cur.MinStockScaled)
		if newCategory != nil || unitsChanged || minChanged {
			if err := checkItemChange(ctx, q, tenantID, newCategory, p, unitsChanged); err != nil {
				return err
			}
		}
		row, err := q.UpdateStockItem(ctx, p)
		if err != nil {
			return mapErr(err)
		}
		if err := applyPacks(ctx, q, tenantID, id, packs, in.Packs); err != nil {
			return err
		}
		out, err = loadItem(ctx, q, tenantID, row)
		return err
	})
	return out, err
}

func validType(typ string) error {
	switch typ {
	case TypeIngredient, TypeSupporting, TypePrepared, TypeFinished, TypeSupply:
		return nil
	}
	return fmt.Errorf("%w: type must be %s, %s, %s, %s or %s", kernel.ErrValidation, TypeIngredient, TypeSupporting, TypePrepared, TypeFinished, TypeSupply)
}

func validShelfLife(shelfLife *int) error {
	if shelfLife != nil && (*shelfLife < 0 || *shelfLife > maxShelfLife) {
		return fmt.Errorf("%w: shelf_life_days is from 1 to %d (0 clears it)", kernel.ErrValidation, maxShelfLife)
	}
	return nil
}

// checkItem checks a new item's references: a live category, a base unit that is an active
// reference unit, a recipe unit in the same category, and a minimum stock on the base unit's step.
func checkItem(ctx context.Context, q *db.Queries, tenantID uuid.UUID, category *uuid.UUID, base uuid.UUID, recipe *uuid.UUID, track bool, minStock *int64) error {
	p := db.UpdateStockItemParams{BaseUomID: base, RecipeUomID: recipe, Track: track, MinStockScaled: minStock}
	return checkItemChange(ctx, q, tenantID, category, p, true)
}

func checkItemChange(ctx context.Context, q *db.Queries, tenantID uuid.UUID, newCategory *uuid.UUID, p db.UpdateStockItemParams, unitsChanged bool) error {
	if newCategory != nil {
		ok, err := q.IsLiveStockCategory(ctx, db.IsLiveStockCategoryParams{TenantID: tenantID, ID: *newCategory})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: category_id must be a stock category that is not archived", kernel.ErrValidation)
		}
	}
	base, err := unitRow(ctx, q, tenantID, p.BaseUomID, "base_uom_id")
	if err != nil {
		return err
	}
	if unitsChanged {
		if !base.Usable || !base.IsReference {
			return fmt.Errorf("%w: base_uom_id must be the active reference unit of a unit category that is not archived", kernel.ErrValidation)
		}
		if p.RecipeUomID != nil {
			recipe, err := unitRow(ctx, q, tenantID, *p.RecipeUomID, "recipe_uom_id")
			if err != nil {
				return err
			}
			if !recipe.Usable || recipe.CategoryID != base.CategoryID {
				return fmt.Errorf("%w: recipe_uom_id must be an active unit in the base unit's category (BR-UOM-05)", kernel.ErrValidation)
			}
		}
	}
	if p.MinStockScaled != nil {
		if !p.Track {
			return fmt.Errorf("%w: an untracked item has no minimum stock (BR-ITM-03)", kernel.ErrValidation)
		}
		if err := onStep("min_stock_scaled", *p.MinStockScaled, base.RoundingScaled); err != nil {
			return err
		}
	}
	return nil
}

func unitRow(ctx context.Context, q *db.Queries, tenantID, id uuid.UUID, field string) (db.GetUnitForItemRow, error) {
	u, err := q.GetUnitForItem(ctx, db.GetUnitForItemParams{TenantID: tenantID, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return u, fmt.Errorf("%w: %s is not a unit of this business", kernel.ErrValidation, field)
		}
		return u, err
	}
	return u, nil
}

// onStep refuses a quantity that is not a whole number of its unit's steps (BR-UOM-06). Both are
// thousandths of the same unit.
func onStep(field string, q, step int64) error {
	if q < 1 || q > maxQuantity {
		return fmt.Errorf("%w: %s is from 1 to %d thousandths", kernel.ErrValidation, field, int64(maxQuantity))
	}
	if q%step != 0 {
		return fmt.Errorf("%w: %s must be a multiple of the unit's step (%d thousandths)", kernel.ErrValidation, field, step)
	}
	return nil
}

func normalizePack(p *PackInput) error {
	name, err := validName("pack name", p.Name, maxUnitName)
	if err != nil {
		return err
	}
	p.Name = name
	return reduce(name, &p.RatioNum, &p.RatioDen, p.RoundingScaled)
}

func applyPacks(ctx context.Context, q *db.Queries, tenantID, itemID uuid.UUID, existing []db.StockItemPack, in []PackInput) error {
	byID := make(map[uuid.UUID]db.StockItemPack, len(existing))
	for _, p := range existing {
		byID[p.ID] = p
	}
	var added []PackInput
	for _, p := range in {
		if p.ID == nil {
			added = append(added, p)
			continue
		}
		old, ok := byID[*p.ID]
		if !ok {
			return fmt.Errorf("%w: pack %s is not this item's", kernel.ErrValidation, p.ID)
		}
		if p.RatioNum != old.RatioNum || p.RatioDen != old.RatioDen {
			return fmt.Errorf("%w: a pack keeps its ratio; add a new pack and deactivate %q (BR-UOM-07)", kernel.ErrValidation, old.Name)
		}
		if _, err := q.UpdatePack(ctx, db.UpdatePackParams{
			TenantID: tenantID, StockItemID: itemID, ID: *p.ID, Name: p.Name, RoundingScaled: p.RoundingScaled, Active: p.Active,
		}); err != nil {
			return mapErr(err)
		}
	}
	if len(existing)+len(added) > maxPacks {
		return fmt.Errorf("%w: an item has at most %d packs", kernel.ErrValidation, maxPacks)
	}
	return insertPacks(ctx, q, tenantID, itemID, added)
}

func insertPacks(ctx context.Context, q *db.Queries, tenantID, itemID uuid.UUID, packs []PackInput) error {
	if len(packs) == 0 {
		return nil
	}
	p := db.InsertPacksParams{TenantID: tenantID, StockItemID: itemID}
	for _, pk := range packs {
		p.Ids = append(p.Ids, kernel.NewID())
		p.Names = append(p.Names, pk.Name)
		p.RatioNum = append(p.RatioNum, pk.RatioNum)
		p.RatioDen = append(p.RatioDen, pk.RatioDen)
		p.RoundingScaled = append(p.RoundingScaled, pk.RoundingScaled)
		p.Active = append(p.Active, pk.Active)
	}
	return mapErr(q.InsertPacks(ctx, p))
}

func loadItem(ctx context.Context, q *db.Queries, tenantID uuid.UUID, row db.StockItem) (StockItem, error) {
	p, err := withPacks(ctx, q, tenantID, kernel.Paged[db.StockItem]{Items: []db.StockItem{row}})
	if err != nil {
		return StockItem{}, err
	}
	return p.Items[0], nil
}

// withPacks attaches every item's packs with one query, whatever the page size.
func withPacks(ctx context.Context, q *db.Queries, tenantID uuid.UUID, page kernel.Paged[db.StockItem]) (kernel.Paged[StockItem], error) {
	out := kernel.Paged[StockItem]{Next: page.Next, Items: make([]StockItem, len(page.Items))}
	ids := make([]uuid.UUID, len(page.Items))
	at := make(map[uuid.UUID]int, len(page.Items))
	for i, r := range page.Items {
		out.Items[i] = toStockItem(r)
		ids[i], at[r.ID] = r.ID, i
	}
	if len(ids) == 0 {
		return out, nil
	}
	packs, err := q.ListPacksByItems(ctx, db.ListPacksByItemsParams{TenantID: tenantID, ItemIds: ids})
	if err != nil {
		return out, err
	}
	for _, p := range packs {
		it := &out.Items[at[p.StockItemID]]
		it.Packs = append(it.Packs, Pack{ID: p.ID, Name: p.Name, RatioNum: p.RatioNum, RatioDen: p.RatioDen, RoundingScaled: p.RoundingScaled, Active: p.Active})
	}
	return out, nil
}

func toStockItem(r db.StockItem) StockItem {
	it := StockItem{
		ID: r.ID, Name: r.Name, Type: r.Type, CategoryID: r.CategoryID, BaseUomID: r.BaseUomID, RecipeUomID: r.RecipeUomID,
		Track: r.Track, MinStockScaled: r.MinStockScaled, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt, Packs: []Pack{},
	}
	if r.ShelfLifeDays != nil {
		d := int(*r.ShelfLifeDays)
		it.ShelfLifeDays = &d
	}
	return it
}

func zeroIsNil(v *int64) *int64 {
	if v == nil || *v == 0 {
		return nil
	}
	return v
}

func days(v *int) *int32 {
	if v == nil || *v == 0 {
		return nil
	}
	d := int32(*v) //nolint:gosec // validated: at most maxShelfLife
	return &d
}

func sameID(a, b *uuid.UUID) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

func sameInt(a, b *int64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
