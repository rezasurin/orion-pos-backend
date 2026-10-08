package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// UomCategory is a family of units that convert into each other, such as weight. Exactly one of
// its units is the reference, in which its ingredients' quantities are stored (ADR 0009).
type UomCategory struct {
	ID         uuid.UUID
	Name       string
	ArchivedAt *time.Time
	CreatedAt  time.Time
	Units      []Uom // the reference first, then from small to large
}

// Uom is a unit: one of it is RatioNum/RatioDen reference units, a reduced fraction.
type Uom struct {
	ID             uuid.UUID
	Name           string
	IsReference    bool
	RatioNum       int64
	RatioDen       int64
	RoundingScaled int64 // display and entry precision, in thousandths of this unit
	Active         bool
}

// NewUomCategory describes a unit category to create, with its units. Exactly one unit must be the
// reference, with a ratio of 1/1.
type NewUomCategory struct {
	Name  string
	Units []UnitInput
}

// UnitInput is the whole state of a unit as the client sends it. ID is nil for a new unit.
type UnitInput struct {
	ID             *uuid.UUID
	Name           string
	IsReference    bool // only when creating a category
	RatioNum       int64
	RatioDen       int64
	RoundingScaled int64
	Active         bool
}

// UpdateUomCategory changes a unit category. Nil fields stay as they are. Units listed with an id
// are replaced by what is sent, units without one are added, and units left out stay. A unit is
// never deleted, and the reference unit stays the reference with a ratio of 1/1.
type UpdateUomCategory struct {
	Name     *string
	Archived *bool
	Units    []UnitInput
}

// CreateUomCategory adds a unit category with its units.
func (s *Service) CreateUomCategory(ctx context.Context, tenantID uuid.UUID, in NewUomCategory) (UomCategory, error) {
	name, err := validName("name", in.Name, maxName)
	if err != nil {
		return UomCategory{}, err
	}
	if len(in.Units) == 0 || len(in.Units) > maxUnits {
		return UomCategory{}, fmt.Errorf("%w: a unit category needs 1 to %d units", kernel.ErrValidation, maxUnits)
	}
	refs := 0
	for i := range in.Units {
		if in.Units[i].ID != nil {
			return UomCategory{}, fmt.Errorf("%w: a new category's units have no id", kernel.ErrValidation)
		}
		if err := normalizeUnit(&in.Units[i]); err != nil {
			return UomCategory{}, err
		}
		if in.Units[i].IsReference {
			refs++
		}
	}
	if refs != 1 {
		return UomCategory{}, fmt.Errorf("%w: exactly one unit must be the reference", kernel.ErrValidation)
	}
	var out UomCategory
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.InsertUomCategory(ctx, db.InsertUomCategoryParams{ID: kernel.NewID(), TenantID: tenantID, Name: name})
		if err != nil {
			return mapErr(err)
		}
		if err := insertUnits(ctx, q, tenantID, row.ID, in.Units); err != nil {
			return err
		}
		out, err = loadCategory(ctx, q, tenantID, row)
		return err
	})
	return out, err
}

// ListUomCategories returns a page of unit categories ordered by id, each with all its units.
func (s *Service) ListUomCategories(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[UomCategory], error) {
	var out kernel.Paged[UomCategory]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListUomCategories(ctx, db.ListUomCategoriesParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.UomCategory) uuid.UUID { return r.ID })
		out, err = withUnits(ctx, q, tenantID, paged)
		return err
	})
	return out, err
}

// UpdateUomCategory applies changes to a unit category and its units.
func (s *Service) UpdateUomCategory(ctx context.Context, tenantID, id uuid.UUID, in UpdateUomCategory) (UomCategory, error) {
	if len(in.Units) > maxUnits {
		return UomCategory{}, fmt.Errorf("%w: at most %d units", kernel.ErrValidation, maxUnits)
	}
	for i := range in.Units {
		if in.Units[i].IsReference {
			return UomCategory{}, fmt.Errorf("%w: the reference unit is chosen when the category is created", kernel.ErrValidation)
		}
		if err := normalizeUnit(&in.Units[i]); err != nil {
			return UomCategory{}, err
		}
	}
	var out UomCategory
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		cur, err := q.GetUomCategoryForUpdate(ctx, db.GetUomCategoryForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateUomCategoryParams{TenantID: tenantID, ID: id, Name: cur.Name, ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived)}
		if in.Name != nil {
			if p.Name, err = validName("name", *in.Name, maxName); err != nil {
				return err
			}
		}
		row, err := q.UpdateUomCategory(ctx, p)
		if err != nil {
			return mapErr(err)
		}

		units, err := q.ListUomsByCategories(ctx, db.ListUomsByCategoriesParams{TenantID: tenantID, CategoryIds: []uuid.UUID{id}})
		if err != nil {
			return err
		}
		existing := make(map[uuid.UUID]db.Uom, len(units))
		for _, u := range units {
			existing[u.ID] = u
		}
		var added []UnitInput
		for _, u := range in.Units {
			if u.ID == nil {
				added = append(added, u)
				continue
			}
			old, ok := existing[*u.ID]
			if !ok {
				return fmt.Errorf("%w: unit %s is not in this category", kernel.ErrValidation, u.ID)
			}
			if old.IsReference && (u.RatioNum != 1 || u.RatioDen != 1 || !u.Active) {
				return fmt.Errorf("%w: the reference unit keeps a ratio of 1/1 and stays active", kernel.ErrValidation)
			}
			// ponytail: one statement per changed unit; a category has at most 50, edited by hand.
			if _, err := q.UpdateUom(ctx, db.UpdateUomParams{
				TenantID: tenantID, CategoryID: id, ID: *u.ID, Name: u.Name,
				RatioNum: u.RatioNum, RatioDen: u.RatioDen, RoundingScaled: u.RoundingScaled, Active: u.Active,
			}); err != nil {
				return mapErr(err)
			}
		}
		if len(units)+len(added) > maxUnits {
			return fmt.Errorf("%w: a unit category has at most %d units", kernel.ErrValidation, maxUnits)
		}
		if err := insertUnits(ctx, q, tenantID, id, added); err != nil {
			return err
		}
		out, err = loadCategory(ctx, q, tenantID, row)
		return err
	})
	return out, err
}

// normalizeUnit checks a unit and reduces its ratio. A reference unit is 1/1 and active.
func normalizeUnit(u *UnitInput) error {
	name, err := validName("unit name", u.Name, maxUnitName)
	if err != nil {
		return err
	}
	u.Name = name
	if u.RatioNum < 1 || u.RatioNum > maxRatioPart || u.RatioDen < 1 || u.RatioDen > maxRatioPart {
		return fmt.Errorf("%w: ratio_num and ratio_den of %q are whole numbers from 1 to %d", kernel.ErrValidation, name, maxRatioPart)
	}
	if u.RoundingScaled < 1 || u.RoundingScaled > maxRounding {
		return fmt.Errorf("%w: rounding_scaled of %q is from 1 to %d thousandths", kernel.ErrValidation, name, maxRounding)
	}
	g := gcd(u.RatioNum, u.RatioDen)
	u.RatioNum, u.RatioDen = u.RatioNum/g, u.RatioDen/g
	if u.IsReference && (u.RatioNum != 1 || u.RatioDen != 1 || !u.Active) {
		return fmt.Errorf("%w: the reference unit %q has a ratio of 1/1 and is active", kernel.ErrValidation, name)
	}
	return nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func insertUnits(ctx context.Context, q *db.Queries, tenantID, categoryID uuid.UUID, units []UnitInput) error {
	if len(units) == 0 {
		return nil
	}
	p := db.InsertUomsParams{TenantID: tenantID, CategoryID: categoryID}
	for _, u := range units {
		p.Ids = append(p.Ids, kernel.NewID())
		p.Names = append(p.Names, u.Name)
		p.IsReference = append(p.IsReference, u.IsReference)
		p.RatioNum = append(p.RatioNum, u.RatioNum)
		p.RatioDen = append(p.RatioDen, u.RatioDen)
		p.RoundingScaled = append(p.RoundingScaled, u.RoundingScaled)
		p.Active = append(p.Active, u.Active)
	}
	return mapErr(q.InsertUoms(ctx, p))
}

func loadCategory(ctx context.Context, q *db.Queries, tenantID uuid.UUID, row db.UomCategory) (UomCategory, error) {
	p, err := withUnits(ctx, q, tenantID, kernel.Paged[db.UomCategory]{Items: []db.UomCategory{row}})
	if err != nil {
		return UomCategory{}, err
	}
	return p.Items[0], nil
}

// withUnits attaches every category's units with one query, whatever the page size.
func withUnits(ctx context.Context, q *db.Queries, tenantID uuid.UUID, page kernel.Paged[db.UomCategory]) (kernel.Paged[UomCategory], error) {
	out := kernel.Paged[UomCategory]{Next: page.Next, Items: make([]UomCategory, len(page.Items))}
	ids := make([]uuid.UUID, len(page.Items))
	at := make(map[uuid.UUID]int, len(page.Items))
	for i, r := range page.Items {
		out.Items[i] = UomCategory{ID: r.ID, Name: r.Name, ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt, Units: []Uom{}}
		ids[i], at[r.ID] = r.ID, i
	}
	if len(ids) == 0 {
		return out, nil
	}
	units, err := q.ListUomsByCategories(ctx, db.ListUomsByCategoriesParams{TenantID: tenantID, CategoryIds: ids})
	if err != nil {
		return out, err
	}
	for _, u := range units {
		c := &out.Items[at[u.CategoryID]]
		c.Units = append(c.Units, Uom{ID: u.ID, Name: u.Name, IsReference: u.IsReference, RatioNum: u.RatioNum, RatioDen: u.RatioDen, RoundingScaled: u.RoundingScaled, Active: u.Active})
	}
	return out, nil
}
