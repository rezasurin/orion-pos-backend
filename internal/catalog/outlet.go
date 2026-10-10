package catalog

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// SetOutletVariant is the price and availability of a variant at an outlet. It replaces whatever
// was set before: a nil PriceOverride means the base price.
type SetOutletVariant struct {
	PriceOverride *kernel.Rupiah
	Available     bool
}

// SetOutletVariant sets a variant's price override and availability at one outlet. The change is
// recorded for that outlet only, so other outlets' devices do not download it, and a changed price
// is written to the audit log. A missing outlet or variant, including another business's, is
// kernel.ErrNotFound.
func (s *Service) SetOutletVariant(ctx context.Context, tenantID, outletID, variantID uuid.UUID, in SetOutletVariant) (OutletVariant, error) {
	var override *int64
	if in.PriceOverride != nil {
		if err := validPrice("price_override", *in.PriceOverride, false); err != nil {
			return OutletVariant{}, err
		}
		v := int64(*in.PriceOverride)
		override = &v
	}
	var out OutletVariant
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		before, err := q.GetOutletVariantForUpdate(ctx, db.GetOutletVariantForUpdateParams{TenantID: tenantID, OutletID: outletID, VariantID: variantID})
		hadRow := err == nil
		if err != nil && !errors.Is(mapErr(err), kernel.ErrNotFound) {
			return err
		}
		row, err := q.UpsertOutletVariant(ctx, db.UpsertOutletVariantParams{
			TenantID: tenantID, OutletID: outletID, VariantID: variantID, PriceOverride: override, Available: in.Available,
		})
		if err != nil {
			return mapRefErr(err)
		}
		out = toOutletVariant(row)
		if _, err := kernel.RecordChange(ctx, tx, EntityOutletVariant, variantID, "upsert", &outletID); err != nil {
			return err
		}
		if !hadRow && override == nil || hadRow && equalPtr(before.PriceOverride, override) {
			return nil
		}
		var was any
		if hadRow && before.PriceOverride != nil {
			was = *before.PriceOverride
		}
		var now any
		if override != nil {
			now = *override
		}
		return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "catalog.price_changed", TargetType: "variant", TargetID: variantID,
			Detail: map[string]any{"scope": "outlet", "outlet_id": outletID, "before": was, "after": now},
		})
	})
	return out, err
}

// ListOutletVariants returns the variants with a row at the outlet (an override or a changed
// availability), ordered by variant id. Variants not listed are available at their base price.
func (s *Service) ListOutletVariants(ctx context.Context, tenantID, outletID uuid.UUID, page kernel.Page) (kernel.Paged[OutletVariant], error) {
	var out kernel.Paged[OutletVariant]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListOutletVariants(ctx, db.ListOutletVariantsParams{
			TenantID: tenantID, OutletID: outletID, After: page.After, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.OutletVariant) uuid.UUID { return r.VariantID })
		out = kernel.Paged[OutletVariant]{Next: paged.Next, Items: make([]OutletVariant, len(paged.Items))}
		for i, r := range paged.Items {
			out.Items[i] = toOutletVariant(r)
		}
		return nil
	})
	return out, err
}

func toOutletVariant(r db.OutletVariant) OutletVariant {
	ov := OutletVariant{OutletID: r.OutletID, VariantID: r.VariantID, Available: r.Available}
	if r.PriceOverride != nil {
		p := kernel.Rupiah(*r.PriceOverride)
		ov.PriceOverride = &p
	}
	return ov
}

func equalPtr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
