package catalog

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// PriceCheck is what the server knows about one variant a sale was rung up with.
type PriceCheck struct {
	Exists bool
	// Current is what the variant costs at the outlet now (an override there, or the base price).
	Current kernel.Rupiah
	// MissedUpdate is true when the variant or its item changed after the device's catalog cursor
	// but before the sale: an update the device could have had and did not.
	MissedUpdate bool
}

// CheckPrices looks up the variants of a sale for the sales projector, inside the event's
// transaction. catalogSeq is the change number the device had pulled up to; at is when the sale
// happened. Variants that do not exist are absent from the result.
func (s *Service) CheckPrices(ctx context.Context, tx pgx.Tx, tenantID, outletID uuid.UUID, variantIDs []uuid.UUID, catalogSeq int64, at time.Time) (map[uuid.UUID]PriceCheck, error) {
	q := db.New(tx)
	prices, err := q.EffectivePrices(ctx, db.EffectivePricesParams{TenantID: tenantID, OutletID: outletID, VariantIds: variantIDs})
	if err != nil {
		return nil, err
	}
	itemIDs := make([]uuid.UUID, 0, len(prices))
	itemOf := make(map[uuid.UUID]uuid.UUID, len(prices))
	for _, p := range prices {
		itemIDs = append(itemIDs, p.ItemID)
		itemOf[p.ID] = p.ItemID
	}
	changes, err := q.CatalogChangesBefore(ctx, db.CatalogChangesBeforeParams{
		TenantID: tenantID, OutletID: &outletID, AfterSeq: catalogSeq, At: at, ItemIds: itemIDs, VariantIds: variantIDs,
	})
	if err != nil {
		return nil, err
	}
	changed := make(map[uuid.UUID]bool, len(changes)) // item ids and variant ids that missed an update
	for _, c := range changes {
		changed[c.EntityID] = true
	}
	out := make(map[uuid.UUID]PriceCheck, len(prices))
	for _, p := range prices {
		out[p.ID] = PriceCheck{Exists: true, Current: kernel.Rupiah(p.Price), MissedUpdate: changed[p.ID] || changed[itemOf[p.ID]]}
	}
	return out, nil
}
