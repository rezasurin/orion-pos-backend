package platform

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory"
)

// RebuildStock compares a business's stock balances with their ledger (BR-STK-08) and, with fix,
// sets the ones that differ to what the ledger says. The ledger is never changed. Audited either
// way, since it reads a business's stock.
func (s *Service) RebuildStock(ctx context.Context, a Actor, tenantSlug string, fix bool, reason string) ([]inventory.Drift, error) {
	var out []inventory.Drift
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		action := "stock.checked"
		if fix {
			action = "stock.rebuilt"
			out, err = inventory.RepairBalances(ctx, tx, t.ID)
		} else {
			out, err = inventory.CheckBalances(ctx, tx, &t.ID)
		}
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: action, TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID,
			After: map[string]any{"balances_differing": len(out), "repaired": fix}, Reason: reason,
		})
	})
	return out, err
}
