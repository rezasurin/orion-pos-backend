package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// NewModifier describes a choice to create.
type NewModifier struct {
	Name       string
	PriceDelta kernel.Rupiah
}

// NewModifierGroup describes a group to create, with its first choices.
type NewModifierGroup struct {
	Name      string
	MinSelect int
	MaxSelect int
	Required  bool
	Modifiers []NewModifier
}

// validateSelection checks the selection rules of a group: at least one choice may be picked, no
// fewer than the minimum, and a required group needs a minimum of one.
func validateSelection(min, max int, required bool) error {
	switch {
	case max < 1 || max > 100:
		return fmt.Errorf("%w: max_select must be between 1 and 100", kernel.ErrValidation)
	case min < 0 || min > max:
		return fmt.Errorf("%w: min_select must be between 0 and max_select", kernel.ErrValidation)
	case required && min < 1:
		return fmt.Errorf("%w: a required group needs min_select of at least 1", kernel.ErrValidation)
	}
	return nil
}

func modifierColumns(ms []NewModifier) (db.InsertModifiersParams, error) {
	var p db.InsertModifiersParams
	for _, m := range ms {
		name := strings.TrimSpace(m.Name)
		if err := validName("modifier name", name, maxName); err != nil {
			return p, err
		}
		if err := validPrice("price_delta", m.PriceDelta, true); err != nil {
			return p, err
		}
		p.Ids = append(p.Ids, kernel.NewID())
		p.Names = append(p.Names, name)
		p.PriceDeltas = append(p.PriceDeltas, int64(m.PriceDelta))
	}
	return p, nil
}

// CreateModifierGroup adds a group and its choices in one transaction.
func (s *Service) CreateModifierGroup(ctx context.Context, tenantID uuid.UUID, in NewModifierGroup) (ModifierGroup, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := validName("name", in.Name, maxName); err != nil {
		return ModifierGroup{}, err
	}
	if err := validateSelection(in.MinSelect, in.MaxSelect, in.Required); err != nil {
		return ModifierGroup{}, err
	}
	if len(in.Modifiers) > maxModifiers {
		return ModifierGroup{}, fmt.Errorf("%w: a group has at most %d modifiers", kernel.ErrValidation, maxModifiers)
	}
	cols, err := modifierColumns(in.Modifiers)
	if err != nil {
		return ModifierGroup{}, err
	}

	id := kernel.NewID()
	var out ModifierGroup
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		if _, err := q.InsertModifierGroup(ctx, db.InsertModifierGroupParams{
			ID: id, TenantID: tenantID, Name: in.Name, MinSelect: int32(in.MinSelect), MaxSelect: int32(in.MaxSelect), //nolint:gosec // validated above
			Required: in.Required,
		}); err != nil {
			return mapErr(err)
		}
		if len(cols.Ids) > 0 {
			cols.TenantID, cols.GroupID = tenantID, id
			if _, err := q.InsertModifiers(ctx, cols); err != nil {
				return mapErr(err)
			}
		}
		if _, err := kernel.RecordChange(ctx, tx, EntityModifierGroup, id, "upsert", nil); err != nil {
			return err
		}
		out, err = loadGroup(ctx, q, tenantID, id)
		return err
	})
	return out, err
}

// ListModifierGroups returns a page of groups ordered by id, with their modifiers from one more
// query.
func (s *Service) ListModifierGroups(ctx context.Context, tenantID uuid.UUID, page kernel.Page, includeArchived bool) (kernel.Paged[ModifierGroup], error) {
	var out kernel.Paged[ModifierGroup]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListModifierGroups(ctx, db.ListModifierGroupsParams{
			TenantID: tenantID, After: page.After, IncludeArchived: includeArchived, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.ModifierGroup) uuid.UUID { return r.ID })
		groups, err := assembleGroups(ctx, q, tenantID, paged.Items)
		if err != nil {
			return err
		}
		out = kernel.Paged[ModifierGroup]{Items: groups, Next: paged.Next}
		return nil
	})
	return out, err
}

// UpdateModifierGroup changes a group. Nil fields stay as they are.
type UpdateModifierGroup struct {
	Name      *string
	MinSelect *int
	MaxSelect *int
	Required  *bool
	Archived  *bool
}

// UpdateModifierGroup applies changes to one group, checking the selection rules on the result.
func (s *Service) UpdateModifierGroup(ctx context.Context, tenantID, id uuid.UUID, in UpdateModifierGroup) (ModifierGroup, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := validName("name", n, maxName); err != nil {
			return ModifierGroup{}, err
		}
		in.Name = &n
	}
	var out ModifierGroup
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetModifierGroupForUpdate(ctx, db.GetModifierGroupForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateModifierGroupParams{
			TenantID: tenantID, ID: id, Name: cur.Name, MinSelect: cur.MinSelect, MaxSelect: cur.MaxSelect, Required: cur.Required,
			ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived),
		}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.MinSelect != nil {
			p.MinSelect = int32(clampInt(*in.MinSelect)) //nolint:gosec // clamped, then validated below
		}
		if in.MaxSelect != nil {
			p.MaxSelect = int32(clampInt(*in.MaxSelect)) //nolint:gosec // clamped, then validated below
		}
		if in.Required != nil {
			p.Required = *in.Required
		}
		if err := validateSelection(int(p.MinSelect), int(p.MaxSelect), p.Required); err != nil {
			return err
		}
		if _, err := q.UpdateModifierGroup(ctx, p); err != nil {
			return mapErr(err)
		}
		if _, err := kernel.RecordChange(ctx, tx, EntityModifierGroup, id, "upsert", nil); err != nil {
			return err
		}
		out, err = loadGroup(ctx, q, tenantID, id)
		return err
	})
	return out, err
}

// AddModifier adds a choice to a group.
func (s *Service) AddModifier(ctx context.Context, tenantID, groupID uuid.UUID, in NewModifier) (Modifier, error) {
	cols, err := modifierColumns([]NewModifier{in})
	if err != nil {
		return Modifier{}, err
	}
	var out Modifier
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		if _, err := q.GetModifierGroupForUpdate(ctx, db.GetModifierGroupForUpdateParams{TenantID: tenantID, ID: groupID}); err != nil {
			return mapErr(err)
		}
		existing, err := q.ListModifiersByGroups(ctx, db.ListModifiersByGroupsParams{TenantID: tenantID, GroupIds: []uuid.UUID{groupID}})
		if err != nil {
			return err
		}
		if len(existing) >= maxModifiers {
			return fmt.Errorf("%w: a group has at most %d modifiers", kernel.ErrValidation, maxModifiers)
		}
		row, err := q.InsertModifier(ctx, db.InsertModifierParams{
			ID: cols.Ids[0], TenantID: tenantID, GroupID: groupID, Name: cols.Names[0], PriceDelta: cols.PriceDeltas[0],
			SortOrder: int32(len(existing)), //nolint:gosec // at most maxModifiers
		})
		if err != nil {
			return mapErr(err)
		}
		out = toModifier(row)
		_, err = kernel.RecordChange(ctx, tx, EntityModifierGroup, groupID, "upsert", nil)
		return err
	})
	return out, err
}

// UpdateModifier changes a choice. Nil fields stay as they are.
type UpdateModifier struct {
	Name       *string
	PriceDelta *kernel.Rupiah
	SortOrder  *int
	Archived   *bool
}

// UpdateModifier applies changes to one choice. A changed price is written to the audit log.
func (s *Service) UpdateModifier(ctx context.Context, tenantID, id uuid.UUID, in UpdateModifier) (Modifier, error) {
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if err := validName("name", n, maxName); err != nil {
			return Modifier{}, err
		}
		in.Name = &n
	}
	if in.PriceDelta != nil {
		if err := validPrice("price_delta", *in.PriceDelta, true); err != nil {
			return Modifier{}, err
		}
	}
	if in.SortOrder != nil {
		if err := validSort(*in.SortOrder); err != nil {
			return Modifier{}, err
		}
	}
	var out Modifier
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetModifierForUpdate(ctx, db.GetModifierForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateModifierParams{
			TenantID: tenantID, ID: id, Name: cur.Name, PriceDelta: cur.PriceDelta, SortOrder: cur.SortOrder,
			ArchivedAt: s.archiveState(cur.ArchivedAt, in.Archived),
		}
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.PriceDelta != nil {
			p.PriceDelta = int64(*in.PriceDelta)
		}
		if in.SortOrder != nil {
			p.SortOrder = int32(*in.SortOrder) //nolint:gosec // validated above
		}
		row, err := q.UpdateModifier(ctx, p)
		if err != nil {
			return mapErr(err)
		}
		out = toModifier(row)
		if _, err := kernel.RecordChange(ctx, tx, EntityModifierGroup, cur.GroupID, "upsert", nil); err != nil {
			return err
		}
		if cur.PriceDelta != row.PriceDelta {
			return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
				Action: "catalog.price_changed", TargetType: "modifier", TargetID: id,
				Detail: map[string]any{"scope": "modifier", "before": cur.PriceDelta, "after": row.PriceDelta},
			})
		}
		return nil
	})
	return out, err
}

func loadGroup(ctx context.Context, q *db.Queries, tenantID, id uuid.UUID) (ModifierGroup, error) {
	rows, err := q.ListModifierGroupsByIDs(ctx, db.ListModifierGroupsByIDsParams{TenantID: tenantID, Ids: []uuid.UUID{id}})
	if err != nil {
		return ModifierGroup{}, err
	}
	if len(rows) == 0 {
		return ModifierGroup{}, kernel.ErrNotFound
	}
	groups, err := assembleGroups(ctx, q, tenantID, rows)
	if err != nil {
		return ModifierGroup{}, err
	}
	return groups[0], nil
}

// assembleGroups attaches modifiers to groups with one query, however many groups there are.
func assembleGroups(ctx context.Context, q *db.Queries, tenantID uuid.UUID, rows []db.ModifierGroup) ([]ModifierGroup, error) {
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	mods, err := q.ListModifiersByGroups(ctx, db.ListModifiersByGroupsParams{TenantID: tenantID, GroupIds: ids})
	if err != nil {
		return nil, err
	}
	byGroup := map[uuid.UUID][]Modifier{}
	for _, m := range mods {
		byGroup[m.GroupID] = append(byGroup[m.GroupID], toModifier(m))
	}
	out := make([]ModifierGroup, len(rows))
	for i, r := range rows {
		out[i] = ModifierGroup{
			ID: r.ID, Name: r.Name, MinSelect: int(r.MinSelect), MaxSelect: int(r.MaxSelect), Required: r.Required,
			ArchivedAt: r.ArchivedAt, CreatedAt: r.CreatedAt, Modifiers: orEmpty(byGroup[r.ID]),
		}
	}
	return out, nil
}

func toModifier(r db.Modifier) Modifier {
	return Modifier{
		ID: r.ID, GroupID: r.GroupID, Name: r.Name, PriceDelta: kernel.Rupiah(r.PriceDelta), SortOrder: int(r.SortOrder),
		ArchivedAt: r.ArchivedAt,
	}
}

// clampInt keeps a caller's number inside int32 range before it is validated, so a huge value
// fails validation instead of wrapping.
func clampInt(v int) int {
	const lim = 1 << 20
	switch {
	case v > lim:
		return lim
	case v < -lim:
		return -lim
	}
	return v
}
