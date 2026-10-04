package catalog

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
)

// PullIDs names the catalog entities a device needs again, taken from the change log: items stand
// for their variants and modifier group links, groups for their modifiers (see the package
// comment).
type PullIDs struct {
	Categories []uuid.UUID
	Items      []uuid.UUID
	Groups     []uuid.UUID
	Variants   []uuid.UUID // variants whose setting at the device's outlet changed
}

// PullState is the current state of those entities, archived ones included so a device can still
// show what an old sale was.
type PullState struct {
	Categories     []Category
	Items          []Item
	Groups         []ModifierGroup
	OutletVariants []OutletVariant
}

// PullState reads the catalog for the POS pull inside the caller's transaction, in a fixed number
// of queries however much changed. With all set it returns the whole catalog and the outlet's
// price overrides (a full snapshot) and ignores ids.
func (s *Service) PullState(ctx context.Context, tx pgx.Tx, tenantID, outletID uuid.UUID, ids PullIDs, all bool) (PullState, error) {
	q := db.New(tx)
	var out PullState

	var cats []db.Category
	var items []db.Item
	var groups []db.ModifierGroup
	var ovs []db.OutletVariant
	var err error
	if all {
		if cats, err = q.ListAllCategories(ctx, tenantID); err != nil {
			return out, err
		}
		if items, err = q.ListAllItems(ctx, tenantID); err != nil {
			return out, err
		}
		if groups, err = q.ListAllModifierGroups(ctx, tenantID); err != nil {
			return out, err
		}
		if ovs, err = q.ListAllOutletVariants(ctx, db.ListAllOutletVariantsParams{TenantID: tenantID, OutletID: outletID}); err != nil {
			return out, err
		}
	} else {
		if len(ids.Categories) > 0 {
			if cats, err = q.ListCategoriesByIDs(ctx, db.ListCategoriesByIDsParams{TenantID: tenantID, Ids: ids.Categories}); err != nil {
				return out, err
			}
		}
		if len(ids.Items) > 0 {
			if items, err = q.ListItemsByIDs(ctx, db.ListItemsByIDsParams{TenantID: tenantID, Ids: ids.Items}); err != nil {
				return out, err
			}
		}
		if len(ids.Groups) > 0 {
			if groups, err = q.ListModifierGroupsByIDs(ctx, db.ListModifierGroupsByIDsParams{TenantID: tenantID, Ids: ids.Groups}); err != nil {
				return out, err
			}
		}
		if len(ids.Variants) > 0 {
			if ovs, err = q.ListOutletVariantsByVariantIDs(ctx, db.ListOutletVariantsByVariantIDsParams{TenantID: tenantID, OutletID: outletID, VariantIds: ids.Variants}); err != nil {
				return out, err
			}
		}
	}

	out.Categories = make([]Category, len(cats))
	for i, c := range cats {
		out.Categories[i] = toCategory(c)
	}
	if out.Items, err = assembleItems(ctx, q, tenantID, items); err != nil {
		return out, err
	}
	if out.Groups, err = assembleGroups(ctx, q, tenantID, groups); err != nil {
		return out, err
	}
	out.OutletVariants = make([]OutletVariant, len(ovs))
	for i, o := range ovs {
		out.OutletVariants[i] = toOutletVariant(o)
	}
	return out, nil
}
