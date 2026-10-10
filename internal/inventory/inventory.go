// Package inventory owns stock: units of measure, stock items, and (from B3.3) the stock ledger
// (BACKEND_PLAN.md section 6.6, ADR 0009). Other modules use this package's Service and types,
// never the generated queries in ./db.
//
// The rules come from the back office's docs/BUSINESS_RULES.md; code and tests cite their ids
// (BR-UOM-06). Setup data is archived or deactivated, never deleted (BR-GEN-06), because ledger
// rows and recipes will point at it. None of it reaches tablets, so nothing here writes the change
// log.
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
	minName        = 2
	maxName        = 100 // the validation reference: any name is 2 to 100 characters
	maxUnitName    = 60
	maxSymbol      = 10
	maxDescription = 500
	maxUnits       = 50
	maxPacks       = 20
	maxRatioPart   = 1_000_000_000
	maxRounding    = 1_000_000 // thousandths of a unit: 1000 units
	maxQuantity    = 1_000_000_000_000_000
	maxShelfLife   = 3650
)

// Expense groups (BR-EXP-02). A classification only: whether money was paid, owed or is a debt
// payment is a field of the payment, not of the expense.
const (
	GroupCostOfGoods    = "cost_of_goods" // ingredients and supporting materials
	GroupOperating      = "operating"     // rent, utilities, salaries, supplies
	GroupMaintenance    = "maintenance"
	GroupMarketingEvent = "marketing_event"
	GroupCapital        = "capital" // equipment
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

// SeedStandardUnits gives a new business the standard units (BR-UOM-03): gram and kilogram,
// millilitre and litre, pieces, lusin and kodi. Call it inside the transaction that creates the
// business.
func SeedStandardUnits(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	return db.New(tx).SeedStandardUnits(ctx, tenantID)
}

// ExpenseType is what money spent is booked as, such as "Belanja Bahan Pasar" (BR-EXP-02).
type ExpenseType struct {
	ID          uuid.UUID
	Name        string
	Group       string
	Description *string
	ArchivedAt  *time.Time
	CreatedAt   time.Time
}

// NewExpenseType describes an expense type to create.
type NewExpenseType struct {
	Name        string
	Group       string
	Description *string
}

// UpdateExpenseType changes an expense type. Nil fields stay as they are; a blank description
// clears it.
type UpdateExpenseType struct {
	Name        *string
	Group       *string
	Description *string
	Archived    *bool
}

// CreateExpenseType adds an expense type.
func (s *Service) CreateExpenseType(ctx context.Context, tenantID uuid.UUID, in NewExpenseType) (ExpenseType, error) {
	name, desc, err := validNameDesc(in.Name, in.Description)
	if err != nil {
		return ExpenseType{}, err
	}
	if err := validGroup(in.Group); err != nil {
		return ExpenseType{}, err
	}
	var out ExpenseType
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		row, err := db.New(tx).InsertExpenseType(ctx, db.InsertExpenseTypeParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, ExpenseGroup: in.Group, Description: desc,
		})
		out = toExpenseType(row)
		return mapErr(err)
	})
	return out, err
}

// ListExpenseTypes returns a page of expense types ordered by id.
func (s *Service) ListExpenseTypes(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[ExpenseType], error) {
	var out kernel.Paged[ExpenseType]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListExpenseTypes(ctx, db.ListExpenseTypesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out = mapPage(kernel.Trim(page, rows, func(r db.ExpenseType) uuid.UUID { return r.ID }), toExpenseType)
		return nil
	})
	return out, err
}

// UpdateExpenseType applies changes to one expense type.
func (s *Service) UpdateExpenseType(ctx context.Context, tenantID, id uuid.UUID, in UpdateExpenseType) (ExpenseType, error) {
	if in.Group != nil {
		if err := validGroup(*in.Group); err != nil {
			return ExpenseType{}, err
		}
	}
	var out ExpenseType
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetExpenseTypeForUpdate(ctx, db.GetExpenseTypeForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateExpenseTypeParams{TenantID: tenantID, ID: id, ExpenseGroup: cur.ExpenseGroup, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description); err != nil {
			return err
		}
		if in.Group != nil {
			p.ExpenseGroup = *in.Group
		}
		row, err := q.UpdateExpenseType(ctx, p)
		out = toExpenseType(row)
		return mapErr(err)
	})
	return out, err
}

// StockCategory groups stock items for reports and counts (BR-CAT-01), with the expense type a
// purchase of its items defaults to (BR-CAT-02).
type StockCategory struct {
	ID                   uuid.UUID
	Name                 string
	DefaultExpenseTypeID *uuid.UUID
	Description          *string
	ArchivedAt           *time.Time
	CreatedAt            time.Time
}

// NewStockCategory describes a stock category to create.
type NewStockCategory struct {
	Name                 string
	DefaultExpenseTypeID *uuid.UUID
	Description          *string
}

// UpdateStockCategory changes a stock category. Nil fields stay as they are; a blank description
// clears it, and ClearDefaultExpenseType removes the default.
type UpdateStockCategory struct {
	Name                    *string
	DefaultExpenseTypeID    *uuid.UUID
	ClearDefaultExpenseType bool
	Description             *string
	Archived                *bool
}

// CreateStockCategory adds a stock category. A default expense type must be live: archived
// records leave the pickers (BR-GEN-06).
func (s *Service) CreateStockCategory(ctx context.Context, tenantID uuid.UUID, in NewStockCategory) (StockCategory, error) {
	name, desc, err := validNameDesc(in.Name, in.Description)
	if err != nil {
		return StockCategory{}, err
	}
	var out StockCategory
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := liveExpenseType(ctx, q, tenantID, in.DefaultExpenseTypeID); err != nil {
			return err
		}
		row, err := q.InsertStockCategory(ctx, db.InsertStockCategoryParams{
			ID: kernel.NewID(), TenantID: tenantID, Name: name, DefaultExpenseTypeID: in.DefaultExpenseTypeID, Description: desc,
		})
		out = toStockCategory(row)
		return mapErr(err)
	})
	return out, err
}

// ListStockCategories returns a page of stock categories ordered by id.
func (s *Service) ListStockCategories(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[StockCategory], error) {
	var out kernel.Paged[StockCategory]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListStockCategories(ctx, db.ListStockCategoriesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		out = mapPage(kernel.Trim(page, rows, func(r db.StockCategory) uuid.UUID { return r.ID }), toStockCategory)
		return nil
	})
	return out, err
}

// UpdateStockCategory applies changes to one stock category. Archiving one keeps its items in it
// (BR-CAT-03); new items cannot be put in it.
func (s *Service) UpdateStockCategory(ctx context.Context, tenantID, id uuid.UUID, in UpdateStockCategory) (StockCategory, error) {
	var out StockCategory
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetStockCategoryForUpdate(ctx, db.GetStockCategoryForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateStockCategoryParams{TenantID: tenantID, ID: id, DefaultExpenseTypeID: cur.DefaultExpenseTypeID, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if p.Name, p.Description, err = patchNameDesc(cur.Name, cur.Description, in.Name, in.Description); err != nil {
			return err
		}
		switch {
		case in.ClearDefaultExpenseType:
			p.DefaultExpenseTypeID = nil
		case in.DefaultExpenseTypeID != nil:
			if err := liveExpenseType(ctx, q, tenantID, in.DefaultExpenseTypeID); err != nil {
				return err
			}
			p.DefaultExpenseTypeID = in.DefaultExpenseTypeID
		}
		row, err := q.UpdateStockCategory(ctx, p)
		out = toStockCategory(row)
		return mapErr(err)
	})
	return out, err
}

func liveExpenseType(ctx context.Context, q *db.Queries, tenantID uuid.UUID, id *uuid.UUID) error {
	if id == nil {
		return nil
	}
	ok, err := q.IsLiveExpenseType(ctx, db.IsLiveExpenseTypeParams{TenantID: tenantID, ID: *id})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: default_expense_type_id must be an expense type that is not archived", kernel.ErrValidation)
	}
	return nil
}

func validGroup(g string) error {
	switch g {
	case GroupCostOfGoods, GroupOperating, GroupMaintenance, GroupMarketingEvent, GroupCapital:
		return nil
	}
	return fmt.Errorf("%w: group must be %s, %s, %s, %s or %s", kernel.ErrValidation, GroupCostOfGoods, GroupOperating, GroupMaintenance, GroupMarketingEvent, GroupCapital)
}

// validName trims a name and checks its length (BR-GEN-10).
func validName(field, v string, max int) (string, error) {
	v = strings.TrimSpace(v)
	if n := len([]rune(v)); n < minName || n > max {
		return "", fmt.Errorf("%w: %s is required, %d to %d characters", kernel.ErrValidation, field, minName, max)
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

func validNameDesc(name string, desc *string) (string, *string, error) {
	n, err := validName("name", name, maxName)
	if err != nil {
		return "", nil, err
	}
	d, err := optDesc(desc)
	return n, d, err
}

// patchNameDesc applies an optional new name and description to the current ones.
func patchNameDesc(curName string, curDesc, name, desc *string) (string, *string, error) {
	n, d := curName, curDesc
	var err error
	if name != nil {
		if n, err = validName("name", *name, maxName); err != nil {
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

func toExpenseType(r db.ExpenseType) ExpenseType {
	return ExpenseType{ID: r.ID, Name: r.Name, Group: r.ExpenseGroup, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
}

func toStockCategory(r db.StockCategory) StockCategory {
	return StockCategory{ID: r.ID, Name: r.Name, DefaultExpenseTypeID: r.DefaultExpenseTypeID, Description: r.Description, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt}
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
			return fmt.Errorf("%w: a referenced category, expense type, unit or item does not exist", kernel.ErrValidation)
		case "23514":
			return fmt.Errorf("%w: a value is out of range (%s)", kernel.ErrValidation, pgErr.ConstraintName)
		}
	}
	return err
}

var conflictText = map[string]string{
	"expense_type_name_idx":    "an expense type with this name already exists",
	"stock_category_name_idx":  "a stock category with this name already exists",
	"uom_category_name_idx":    "a unit category with this name already exists",
	"uom_name_idx":             "a unit with this name already exists in this category",
	"uom_symbol_idx":           "a unit with this symbol already exists",
	"stock_item_name_idx":      "a stock item with this name already exists",
	"stock_item_pack_name_idx": "this item already has a pack with this name",
}
