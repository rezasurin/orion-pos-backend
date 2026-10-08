// Package inventory owns stock: units of measure, ingredients, and (from B3.3) the stock ledger
// (BACKEND_PLAN.md section 6.6, ADR 0009). Other modules use this package's Service and types,
// never the generated queries in ./db.
//
// Setup data is archived or deactivated, never deleted, because ledger rows and recipes will point
// at it. None of it reaches tablets, so nothing here writes the change log.
package inventory

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

	"github.com/rezasurin/orion-pos-backend/internal/inventory/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

const (
	maxName        = 60
	maxIngredient  = 120
	maxUnitName    = 30
	maxDescription = 500
	maxUnits       = 50
	maxRatioPart   = 1_000_000_000
	maxRounding    = 1_000_000 // thousandths of a unit: 1000 units
)

// Transaction type categories, the back office's three groups.
const (
	TxMaterials   = "materials"    // raw and supporting materials
	TxServices    = "services"     // services, maintenance and the like
	TxDebtPayment = "debt_payment" // paying off purchases made on credit
)

// Service is the inventory module.
type Service struct {
	Pool  *pgxpool.Pool
	Clock kernel.Clock
}

// NewService returns a Service using pool, whose role must be a member of orion_app.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{Pool: pool, Clock: kernel.SystemClock{}}
}

// TransactionType is what a purchase or expense is booked as, such as "Belanja Bahan Pasar".
type TransactionType struct {
	ID          uuid.UUID
	Name        string
	Category    string
	Description *string
	ArchivedAt  *time.Time
	CreatedAt   time.Time
}

// NewTransactionType describes a transaction type to create.
type NewTransactionType struct {
	Name        string
	Category    string
	Description *string
}

// UpdateTransactionType changes a transaction type. Nil fields stay as they are; a blank
// description clears it.
type UpdateTransactionType struct {
	Name        *string
	Category    *string
	Description *string
	Archived    *bool
}

// CreateTransactionType adds a transaction type.
func (s *Service) CreateTransactionType(ctx context.Context, tenantID uuid.UUID, in NewTransactionType) (TransactionType, error) {
	name, desc, err := validNameDesc(in.Name, in.Description, maxName)
	if err != nil {
		return TransactionType{}, err
	}
	if err := validTxCategory(in.Category); err != nil {
		return TransactionType{}, err
	}
	var out TransactionType
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertTransactionType(ctx, db.InsertTransactionTypeParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, Category: in.Category, Description: desc,
		})
		out = toTransactionType(row)
		return mapErr(err)
	})
	return out, err
}

// ListTransactionTypes returns a page of transaction types ordered by id.
func (s *Service) ListTransactionTypes(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[TransactionType], error) {
	var out kernel.Paged[TransactionType]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListTransactionTypes(ctx, db.ListTransactionTypesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out = mapPage(kernel.Trim(page, rows, func(r db.TransactionType) uuid.UUID { return r.ID }), toTransactionType)
		return nil
	})
	return out, err
}

// UpdateTransactionType applies changes to one transaction type.
func (s *Service) UpdateTransactionType(ctx context.Context, tenantID, id uuid.UUID, in UpdateTransactionType) (TransactionType, error) {
	if in.Category != nil {
		if err := validTxCategory(*in.Category); err != nil {
			return TransactionType{}, err
		}
	}
	var out TransactionType
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetTransactionTypeForUpdate(ctx, db.GetTransactionTypeForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateTransactionTypeParams{TenantID: tenantID, ID: id, Name: cur.Name, Category: cur.Category, Description: cur.Description, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description, maxName); err != nil {
			return err
		}
		if in.Category != nil {
			p.Category = *in.Category
		}
		row, err := q.UpdateTransactionType(ctx, p)
		out = toTransactionType(row)
		return mapErr(err)
	})
	return out, err
}

// IngredientCategory groups ingredients, with the transaction type their purchases default to.
type IngredientCategory struct {
	ID                       uuid.UUID
	Name                     string
	DefaultTransactionTypeID *uuid.UUID
	Description              *string
	ArchivedAt               *time.Time
	CreatedAt                time.Time
}

// NewIngredientCategory describes an ingredient category to create.
type NewIngredientCategory struct {
	Name                     string
	DefaultTransactionTypeID *uuid.UUID
	Description              *string
}

// UpdateIngredientCategory changes an ingredient category. Nil fields stay as they are; a blank
// description clears it, and ClearDefaultTransactionType removes the default.
type UpdateIngredientCategory struct {
	Name                        *string
	DefaultTransactionTypeID    *uuid.UUID
	ClearDefaultTransactionType bool
	Description                 *string
	Archived                    *bool
}

// CreateIngredientCategory adds an ingredient category.
func (s *Service) CreateIngredientCategory(ctx context.Context, tenantID uuid.UUID, in NewIngredientCategory) (IngredientCategory, error) {
	name, desc, err := validNameDesc(in.Name, in.Description, maxName)
	if err != nil {
		return IngredientCategory{}, err
	}
	var out IngredientCategory
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertIngredientCategory(ctx, db.InsertIngredientCategoryParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, DefaultTransactionTypeID: in.DefaultTransactionTypeID, Description: desc,
		})
		out = toIngredientCategory(row)
		return mapErr(err)
	})
	return out, err
}

// ListIngredientCategories returns a page of ingredient categories ordered by id.
func (s *Service) ListIngredientCategories(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[IngredientCategory], error) {
	var out kernel.Paged[IngredientCategory]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListIngredientCategories(ctx, db.ListIngredientCategoriesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out = mapPage(kernel.Trim(page, rows, func(r db.IngredientCategory) uuid.UUID { return r.ID }), toIngredientCategory)
		return nil
	})
	return out, err
}

// UpdateIngredientCategory applies changes to one ingredient category.
func (s *Service) UpdateIngredientCategory(ctx context.Context, tenantID, id uuid.UUID, in UpdateIngredientCategory) (IngredientCategory, error) {
	var out IngredientCategory
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetIngredientCategoryForUpdate(ctx, db.GetIngredientCategoryForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateIngredientCategoryParams{TenantID: tenantID, ID: id, DefaultTransactionTypeID: cur.DefaultTransactionTypeID, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description, maxName); err != nil {
			return err
		}
		switch {
		case in.ClearDefaultTransactionType:
			p.DefaultTransactionTypeID = nil
		case in.DefaultTransactionTypeID != nil:
			p.DefaultTransactionTypeID = in.DefaultTransactionTypeID
		}
		row, err := q.UpdateIngredientCategory(ctx, p)
		out = toIngredientCategory(row)
		return mapErr(err)
	})
	return out, err
}

// Ingredient is something stocked: bought, used by recipes, counted. Its quantities are stored in
// thousandths of the reference unit of its unit's category (ADR 0009).
type Ingredient struct {
	ID          uuid.UUID
	Name        string
	CategoryID  *uuid.UUID
	UomID       uuid.UUID // the unit it is shown and entered in
	Track       bool
	Description *string
	ArchivedAt  *time.Time
	CreatedAt   time.Time
}

// NewIngredient describes an ingredient to create.
type NewIngredient struct {
	Name        string
	CategoryID  *uuid.UUID
	UomID       uuid.UUID
	Track       bool
	Description *string
}

// UpdateIngredient changes an ingredient. Nil fields stay as they are; a blank description clears
// it, and ClearCategory removes the category.
type UpdateIngredient struct {
	Name          *string
	CategoryID    *uuid.UUID
	ClearCategory bool
	UomID         *uuid.UUID
	Track         *bool
	Description   *string
	Archived      *bool
}

// CreateIngredient adds an ingredient. Its unit must be active and in a category that is not
// archived.
func (s *Service) CreateIngredient(ctx context.Context, tenantID uuid.UUID, in NewIngredient) (Ingredient, error) {
	name, desc, err := validNameDesc(in.Name, in.Description, maxIngredient)
	if err != nil {
		return Ingredient{}, err
	}
	var out Ingredient
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := usableUnit(ctx, q, tenantID, in.UomID); err != nil {
			return err
		}
		row, err := q.InsertIngredient(ctx, db.InsertIngredientParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, CategoryID: in.CategoryID, UomID: in.UomID, Track: in.Track, Description: desc,
		})
		out = toIngredient(row)
		return mapErr(err)
	})
	return out, err
}

// ListIngredients returns a page of ingredients ordered by id, optionally only one category's.
func (s *Service) ListIngredients(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool, categoryID *uuid.UUID) (kernel.Paged[Ingredient], error) {
	var out kernel.Paged[Ingredient]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListIngredients(ctx, db.ListIngredientsParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, CategoryID: categoryID, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out = mapPage(kernel.Trim(page, rows, func(r db.Ingredient) uuid.UUID { return r.ID }), toIngredient)
		return nil
	})
	return out, err
}

// UpdateIngredient applies changes to one ingredient.
//
// ponytail: an ingredient may move to a unit of another category while nothing is stored in its
// units. B3.3 must refuse that once it has a ledger row or a recipe line (ADR 0009, "fixed once used").
func (s *Service) UpdateIngredient(ctx context.Context, tenantID, id uuid.UUID, in UpdateIngredient) (Ingredient, error) {
	var out Ingredient
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetIngredientForUpdate(ctx, db.GetIngredientForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateIngredientParams{TenantID: tenantID, ID: id, CategoryID: cur.CategoryID, UomID: cur.UomID, Track: cur.Track, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description, maxIngredient); err != nil {
			return err
		}
		switch {
		case in.ClearCategory:
			p.CategoryID = nil
		case in.CategoryID != nil:
			p.CategoryID = in.CategoryID
		}
		if in.UomID != nil && *in.UomID != cur.UomID {
			if err := usableUnit(ctx, q, tenantID, *in.UomID); err != nil {
				return err
			}
			p.UomID = *in.UomID
		}
		if in.Track != nil {
			p.Track = *in.Track
		}
		row, err := q.UpdateIngredient(ctx, p)
		out = toIngredient(row)
		return mapErr(err)
	})
	return out, err
}

func usableUnit(ctx context.Context, q *db.Queries, tenantID, id uuid.UUID) error {
	if _, err := q.GetUomForIngredient(ctx, db.GetUomForIngredientParams{TenantID: tenantID, ID: id}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: uom_id must be an active unit in a category that is not archived", kernel.ErrValidation)
		}
		return err
	}
	return nil
}

func validTxCategory(c string) error {
	switch c {
	case TxMaterials, TxServices, TxDebtPayment:
		return nil
	}
	return fmt.Errorf("%w: category must be %s, %s or %s", kernel.ErrValidation, TxMaterials, TxServices, TxDebtPayment)
}

func validName(field, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || len([]rune(v)) > max {
		return "", fmt.Errorf("%w: %s is required and at most %d characters", kernel.ErrValidation, field, max)
	}
	return v, nil
}

// optDesc trims a description; blank is none.
func optDesc(v *string) (*string, error) {
	if v == nil {
		return nil, nil
	}
	t := strings.TrimSpace(*v)
	if t == "" {
		return nil, nil
	}
	if len([]rune(t)) > maxDescription {
		return nil, fmt.Errorf("%w: description is at most %d characters", kernel.ErrValidation, maxDescription)
	}
	return &t, nil
}

func validNameDesc(name string, desc *string, max int) (string, *string, error) {
	n, err := validName("name", name, max)
	if err != nil {
		return "", nil, err
	}
	d, err := optDesc(desc)
	return n, d, err
}

// patchNameDesc applies an optional new name and description to the current ones.
func patchNameDesc(curName string, curDesc, name, desc *string, max int) (string, *string, error) {
	n, d := curName, curDesc
	var err error
	if name != nil {
		if n, err = validName("name", *name, max); err != nil {
			return "", nil, err
		}
	}
	if desc != nil {
		if d, err = optDesc(desc); err != nil {
			return "", nil, err
		}
	}
	return n, d, nil
}

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

func mapPage[R, T any](p kernel.Paged[R], f func(R) T) kernel.Paged[T] {
	out := kernel.Paged[T]{Next: p.Next, Items: make([]T, len(p.Items))}
	for i, r := range p.Items {
		out.Items[i] = f(r)
	}
	return out
}

func toTransactionType(r db.TransactionType) TransactionType {
	return TransactionType{ID: r.ID, Name: r.Name, Category: r.Category, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}

func toIngredientCategory(r db.IngredientCategory) IngredientCategory {
	return IngredientCategory{ID: r.ID, Name: r.Name, DefaultTransactionTypeID: r.DefaultTransactionTypeID, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}

func toIngredient(r db.Ingredient) Ingredient {
	return Ingredient{ID: r.ID, Name: r.Name, CategoryID: r.CategoryID, UomID: r.UomID, Track: r.Track, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}

// mapErr turns database errors into the kernel errors the HTTP layer understands. A reference to a
// row of another business fails its composite foreign key, so it reads as one that does not exist.
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
			return fmt.Errorf("%w: a referenced category, transaction type or unit does not exist", kernel.ErrValidation)
		case "23514":
			return fmt.Errorf("%w: a value is out of range (%s)", kernel.ErrValidation, pgErr.ConstraintName)
		}
	}
	return err
}

var conflictText = map[string]string{
	"transaction_type_name_idx":    "a transaction type with this name already exists",
	"ingredient_category_name_idx": "an ingredient category with this name already exists",
	"uom_category_name_idx":        "a unit category with this name already exists",
	"uom_name_idx":                 "a unit with this name already exists in this category",
	"ingredient_name_idx":          "an ingredient with this name already exists",
}
