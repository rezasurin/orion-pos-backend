// Package catalog owns what a business sells: categories, items with their variants, modifier
// groups, and per-outlet prices and availability (BACKEND_PLAN.md section 6.3). Other modules use
// this package's Service and types, never the generated queries in ./db.
//
// Everything a sale may point at is archived, never deleted. Every change is written to the change
// log in the same transaction, so the POS pull (section 5.2) sees it. The change log works in
// aggregates: a change to a variant is recorded as a change to its item, and a change to a modifier
// as a change to its group, so the pull reloads one entity with its children.
package catalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Entity types written to the change log.
const (
	EntityCategory      = "category"
	EntityItem          = "item"           // an item with its variants and modifier group links
	EntityModifierGroup = "modifier_group" // a group with its modifiers
	EntityOutletVariant = "outlet_variant" // entity_id is the variant id; the outlet is in outlet_id
	EntityStation       = "kitchen_station"
)

const (
	maxName       = 60
	maxItemName   = 120
	maxCode       = 40
	maxImageURL   = 500
	maxVariants   = 50
	maxModifiers  = 100
	maxItemGroups = 20
	maxMoney      = 1_000_000_000 // rupiah, the same bound as the database checks
)

// Service is the catalog module.
type Service struct {
	Pool  *pgxpool.Pool
	Clock kernel.Clock
}

// NewService returns a Service using pool, whose role must be a member of orion_app.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{Pool: pool, Clock: kernel.SystemClock{}}
}

// Category groups items on the POS screen.
type Category struct {
	ID         uuid.UUID
	Name       string
	SortOrder  int
	ArchivedAt *time.Time
	CreatedAt  time.Time
}

// Item is something for sale, with its variants. Prices live on variants.
type Item struct {
	ID               uuid.UUID
	CategoryID       *uuid.UUID
	StationID        *uuid.UUID // where it is made; nil prints no ticket
	Name             string
	SKU              *string
	Barcode          *string
	ImageURL         *string
	TrackStock       bool
	ArchivedAt       *time.Time
	CreatedAt        time.Time
	Variants         []Variant
	ModifierGroupIDs []uuid.UUID // in display order
}

// Variant is a sellable size or kind of an item. A one-size item has one variant with no name.
type Variant struct {
	ID         uuid.UUID
	ItemID     uuid.UUID
	Name       string
	SKU        *string
	Barcode    *string
	BasePrice  kernel.Rupiah
	SortOrder  int
	ArchivedAt *time.Time
}

// ModifierGroup is a set of choices offered with an item, such as "Sugar level".
type ModifierGroup struct {
	ID         uuid.UUID
	Name       string
	MinSelect  int
	MaxSelect  int
	Required   bool
	ArchivedAt *time.Time
	CreatedAt  time.Time
	Modifiers  []Modifier
}

// Modifier is one choice of a group. PriceDelta may be negative.
type Modifier struct {
	ID         uuid.UUID
	GroupID    uuid.UUID
	Name       string
	PriceDelta kernel.Rupiah
	SortOrder  int
	ArchivedAt *time.Time
}

// OutletVariant is a variant's price and availability at one outlet. A variant without a row is
// available at its base price.
type OutletVariant struct {
	OutletID      uuid.UUID
	VariantID     uuid.UUID
	PriceOverride *kernel.Rupiah // nil means the base price
	Available     bool
}

// ItemFilter narrows ListItems.
type ItemFilter struct {
	IncludeArchived bool
	CategoryID      *uuid.UUID
}

// ---- Categories ----

// NewCategory describes a category to create.
type NewCategory struct {
	Name      string
	SortOrder int
}

// CreateCategory adds a category.
func (s *Service) CreateCategory(ctx context.Context, tenantID uuid.UUID, in NewCategory) (Category, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := validName("name", in.Name, maxName); err != nil {
		return Category{}, err
	}
	if err := validSort(in.SortOrder); err != nil {
		return Category{}, err
	}
	var out Category
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		row, err := db.New(tx).InsertCategory(ctx, db.InsertCategoryParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: in.Name, SortOrder: int32(in.SortOrder), //nolint:gosec // validated above
		})
		if err != nil {
			return mapErr(err)
		}
		out = toCategory(row)
		_, err = kernel.RecordChange(ctx, tx, EntityCategory, row.ID, "upsert", nil)
		return err
	})
	return out, err
}

// ListCategories returns a page of categories ordered by id.
func (s *Service) ListCategories(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[Category], error) {
	var out kernel.Paged[Category]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListCategories(ctx, db.ListCategoriesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.Category) uuid.UUID { return r.ID })
		out = kernel.Paged[Category]{Next: paged.Next, Items: make([]Category, len(paged.Items))}
		for i, r := range paged.Items {
			out.Items[i] = toCategory(r)
		}
		return nil
	})
	return out, err
}

// UpdateCategory changes a category. Nil fields stay as they are.
type UpdateCategory struct {
	Name      *string
	SortOrder *int
	Archived  *bool
}

// UpdateCategory applies changes to one category.
func (s *Service) UpdateCategory(ctx context.Context, tenantID, id uuid.UUID, in UpdateCategory) (Category, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := validName("name", n, maxName); err != nil {
			return Category{}, err
		}
		in.Name = &n
	}
	if in.SortOrder != nil {
		if err := validSort(*in.SortOrder); err != nil {
			return Category{}, err
		}
	}
	var out Category
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetCategoryForUpdate(ctx, db.GetCategoryForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateCategoryParams{TenantID: tenantID, ID: id, Name: cur.Name, SortOrder: cur.SortOrder, ArchivedAt: cur.ArchivedAt}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.SortOrder != nil {
			p.SortOrder = int32(*in.SortOrder) //nolint:gosec // validated above
		}
		p.ArchivedAt = s.archiveState(cur.ArchivedAt, in.Archived)
		row, err := q.UpdateCategory(ctx, p)
		if err != nil {
			return mapErr(err)
		}
		out = toCategory(row)
		_, err = kernel.RecordChange(ctx, tx, EntityCategory, id, "upsert", nil)
		return err
	})
	return out, err
}

// ---- helpers ----

// archiveState returns the archived_at to store: unchanged when archived is nil, now when
// archiving something live, nil when restoring, and the original time when archiving again.
func (s *Service) archiveState(cur *time.Time, archived *bool) *time.Time {
	switch {
	case archived == nil:
		return cur
	case !*archived:
		return nil
	case cur != nil:
		return cur
	}
	now := s.Clock.Now()
	return &now
}

func toCategory(r db.Category) Category {
	return Category{ID: r.ID, Name: r.Name, SortOrder: int(r.SortOrder), ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}

func validName(field, v string, max int) error {
	if v == "" || len([]rune(v)) > max {
		return fmt.Errorf("%w: %s is required and at most %d characters", kernel.ErrValidation, field, max)
	}
	return nil
}

func validPrice(field string, v kernel.Rupiah, allowNegative bool) error {
	lo := kernel.Rupiah(0)
	if allowNegative {
		lo = -maxMoney
	}
	if v < lo || v > maxMoney {
		return fmt.Errorf("%w: %s must be between %d and %d rupiah", kernel.ErrValidation, field, lo, maxMoney)
	}
	return nil
}

// optText trims an optional text field; blank means "none", stored as NULL.
func optText(field string, v *string, max int) (*string, error) {
	if v == nil {
		return nil, nil
	}
	t := strings.TrimSpace(*v)
	if t == "" {
		return nil, nil
	}
	if len([]rune(t)) > max {
		return nil, fmt.Errorf("%w: %s is at most %d characters", kernel.ErrValidation, field, max)
	}
	return &t, nil
}

func derefOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// mapErr turns database errors into the kernel errors the HTTP layer understands.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%w: %s", kernel.ErrConflict, conflictText[pgErr.ConstraintName])
		case "23503":
			return fmt.Errorf("%w: a referenced category, station, modifier group, outlet or variant does not exist", kernel.ErrValidation)
		case "23514":
			return fmt.Errorf("%w: a value is out of range (%s)", kernel.ErrValidation, pgErr.ConstraintName)
		}
	}
	return err
}

var conflictText = map[string]string{
	"category_name_idx":        "a category with this name already exists",
	"modifier_group_name_idx":  "a modifier group with this name already exists",
	"variant_sku_idx":          "this SKU is already used by another variant",
	"variant_barcode_idx":      "this barcode is already used by another variant",
	"kitchen_station_name_idx": "a station with this name already exists",
}

// mapRefErr is mapErr for a reference the caller named by id: a missing target, including one the
// row-level security of another tenant hides, is "not found".
func mapRefErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23503" || (pgErr.Code == "42501" && strings.Contains(pgErr.Message, "row-level security")) {
			return kernel.ErrNotFound
		}
	}
	return mapErr(err)
}
