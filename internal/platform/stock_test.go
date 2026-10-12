package platform_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
)

// An operator checks a business's balances against the ledger and repairs them, audited both times.
func TestRebuildStock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	actor := platform.Actor{OperatorID: &op.Operator.ID}
	tenantID := e.tenant(t, "kopi")

	inv := inventory.NewService(e.d.App)
	staff := uuid.New()
	var outlet, base uuid.UUID
	if _, err := e.d.Owner.Exec(ctx, `INSERT INTO staff (id, tenant_id, display_name) VALUES ($1, $2, 'Gudang')`, staff, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := e.d.Owner.QueryRow(ctx, `SELECT o.id, u.id FROM outlet o, uom u WHERE o.tenant_id = $1 AND u.tenant_id = $1 AND u.symbol = 'g'`, tenantID).Scan(&outlet, &base); err != nil {
		t.Fatal(err)
	}
	cat, err := inv.CreateStockCategory(ctx, tenantID, inventory.NewStockCategory{Name: "Kopi"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := inv.CreateStockItem(ctx, tenantID, inventory.NewStockItem{Name: "Biji kopi", Type: inventory.TypeIngredient, CategoryID: cat.ID, BaseUomID: base, Track: true})
	if err != nil {
		t.Fatal(err)
	}
	value := int64(1000)
	if err := kernel.TenantTx(ctx, e.d.App, tenantID, func(tx pgx.Tx) error {
		_, err := inv.Post(ctx, tx, tenantID, []inventory.NewMovement{{
			OutletID: outlet, StockItemID: item.ID, Kind: inventory.KindOpening, QuantityScaled: 5000, Value: &value,
			StaffID: staff, OccurredAt: time.Now(), BusinessDate: time.Now(),
		}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Owner.Exec(ctx, `UPDATE stock_balance SET quantity_scaled = 1 WHERE tenant_id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}

	for _, fix := range []bool{false, true} {
		drift, err := e.svc.RebuildStock(ctx, actor, "kopi", fix, "a balance looks wrong")
		if err != nil || len(drift) != 1 || drift[0].LedgerQuantity != 5000 || *drift[0].StoredQuantity != 1 {
			t.Fatalf("fix=%v: %+v, %v", fix, drift, err)
		}
	}
	if drift, err := e.svc.RebuildStock(ctx, actor, "kopi", false, "check again"); err != nil || len(drift) != 0 {
		t.Errorf("after the repair: %+v, %v", drift, err)
	}
	if acts := e.actions(t); !slices.Contains(acts, "stock.checked") || !slices.Contains(acts, "stock.rebuilt") {
		t.Errorf("audit = %v", acts)
	}
	if _, err := e.svc.RebuildStock(ctx, actor, "kopi", true, ""); err == nil {
		t.Error("a rebuild without a reason was accepted")
	}
}
