package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rezasurin/orion-pos-backend/internal/inventory/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Movement kinds (BR-STK-02).
const (
	KindOpening          = "opening"
	KindPurchaseReceipt  = "purchase_receipt"
	KindSupplierReturn   = "supplier_return"
	KindSaleConsumption  = "sale_consumption"
	KindProductionIn     = "production_in"
	KindProductionOut    = "production_out"
	KindUsage            = "usage"
	KindWaste            = "waste"
	KindAdjustment       = "adjustment"
	KindOpnameAdjustment = "opname_adjustment"
	KindTransferOut      = "transfer_out"
	KindTransferIn       = "transfer_in"
	KindReversal         = "reversal"
)

// direction is +1 for stock in, -1 for stock out, 0 for either.
var direction = map[string]int{
	KindOpening: 1, KindPurchaseReceipt: 1, KindProductionIn: 1, KindTransferIn: 1,
	KindSupplierReturn: -1, KindSaleConsumption: -1, KindProductionOut: -1, KindUsage: -1, KindWaste: -1, KindTransferOut: -1,
	KindAdjustment: 0, KindOpnameAdjustment: 0, KindReversal: 0,
}

// mayGoNegative lists the kinds that may take a balance below 0 (BR-STK-04): a sale is never
// blocked, and an opname sets stock to what was counted. Every other removal is refused instead.
var mayGoNegative = map[string]bool{KindSaleConsumption: true, KindOpnameAdjustment: true}

// futureSlack is how far ahead of the server clock a movement may be dated; more is refused
// (BR-STK-05). Sale consumption is dated by the tablet's clock and never refused.
const futureSlack = 5 * time.Minute

// ErrInsufficientStock is returned when a manual removal would take a balance below 0
// (BR-STK-04). Its message names the item and what is on hand.
var ErrInsufficientStock = errors.New("inventory: insufficient stock")

// NewMovement is one change to post. For a reversal set only Reverses, StaffID, the dates and the
// reason: the rest comes from the movement it reverses.
type NewMovement struct {
	OutletID       uuid.UUID
	StockItemID    uuid.UUID
	Kind           string
	QuantityScaled int64  // thousandths of the item's base unit, signed
	Value          *int64 // rupiah; stock in may carry its cost, otherwise the average cost is used
	Reason         *string
	Note           *string
	StaffID        uuid.UUID
	SourceType     *string // the document that posts it, such as "sale"
	SourceID       *uuid.UUID
	Reverses       *uuid.UUID
	OccurredAt     time.Time
	BusinessDate   time.Time // the outlet's business day of OccurredAt, computed by the caller
}

// Movement is a posted ledger row.
type Movement struct {
	ID             uuid.UUID
	OutletID       uuid.UUID
	StockItemID    uuid.UUID
	Seq            int64
	Kind           string
	QuantityScaled int64
	Value          int64
	Reverses       *uuid.UUID
	OccurredAt     time.Time
}

// Post adds movements to the ledger and updates the balances, inside the caller's transaction (a
// document and its movements commit together). Each line's value follows BR-CST-03: stock in adds
// the cost given, or the average cost; stock out removes round(quantity × average cost), using the
// last known average while the balance is not above 0 (BR-CST-04). A reversal mirrors its movement
// exactly. Balances are locked in item order, so concurrent postings queue instead of deadlocking.
//
// ponytail: three statements per line (lock, insert, update). Batch them if a sale with many
// recipe lines measures slow in the load check (B3.4).
func (s *Service) Post(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in []NewMovement) ([]Movement, error) {
	if len(in) == 0 {
		return nil, nil
	}
	q := db.New(tx)
	lines := slices.Clone(in)
	if err := resolveReversals(ctx, q, tenantID, lines); err != nil {
		return nil, err
	}
	if err := s.checkLines(ctx, q, tenantID, lines); err != nil {
		return nil, err
	}

	order := make([]int, len(lines))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Or(slices.Compare(lines[a].OutletID[:], lines[b].OutletID[:]), slices.Compare(lines[a].StockItemID[:], lines[b].StockItemID[:]))
	})
	out := make([]Movement, len(lines))
	for _, i := range order {
		m, err := postLine(ctx, q, tenantID, lines[i])
		if err != nil {
			return nil, err
		}
		out[i] = m
	}
	return out, nil
}

func postLine(ctx context.Context, q *db.Queries, tenantID uuid.UUID, l NewMovement) (Movement, error) {
	b, err := q.LockBalance(ctx, db.LockBalanceParams{TenantID: tenantID, OutletID: l.OutletID, StockItemID: l.StockItemID})
	if err != nil {
		return Movement{}, mapErr(err)
	}
	value := l.Value
	if value == nil {
		v := atAverageCost(b, l.QuantityScaled)
		value = &v
	}
	qty, val := b.QuantityScaled+l.QuantityScaled, b.Value+*value
	if qty < 0 && l.QuantityScaled < 0 && !mayGoNegative[l.Kind] {
		return Movement{}, fmt.Errorf("%w: item %s has %d thousandths on hand", ErrInsufficientStock, l.StockItemID, b.QuantityScaled)
	}
	row, err := q.InsertMovement(ctx, db.InsertMovementParams{
		ID: kernel.NewID(), TenantID: tenantID, OutletID: l.OutletID, StockItemID: l.StockItemID, Seq: b.Seq + 1, Kind: l.Kind,
		QuantityScaled: l.QuantityScaled, Value: *value, Reason: l.Reason, Note: l.Note, StaffID: l.StaffID,
		SourceType: l.SourceType, SourceID: l.SourceID, ReversesID: l.Reverses, OccurredAt: l.OccurredAt,
		BusinessDate: pgtype.Date{Time: l.BusinessDate, Valid: true},
	})
	if err != nil {
		return Movement{}, mapErr(err)
	}
	upd := db.UpdateBalanceParams{
		TenantID: tenantID, OutletID: l.OutletID, StockItemID: l.StockItemID, QuantityScaled: qty, Value: val,
		CostQuantity: b.CostQuantity, CostValue: b.CostValue, Seq: row.Seq,
	}
	if qty > 0 {
		upd.CostQuantity, upd.CostValue = &qty, &val
	}
	if err := q.UpdateBalance(ctx, upd); err != nil {
		return Movement{}, mapErr(err)
	}
	return Movement{
		ID: row.ID, OutletID: row.OutletID, StockItemID: row.StockItemID, Seq: row.Seq, Kind: row.Kind,
		QuantityScaled: row.QuantityScaled, Value: row.Value, Reverses: row.ReversesID, OccurredAt: row.OccurredAt,
	}, nil
}

// atAverageCost values a quantity at the balance's average cost: the current one while the
// quantity is above 0, else the last known one, else nothing (0). Rounded half away from zero.
func atAverageCost(b db.StockBalance, quantity int64) int64 {
	num, den := b.Value, b.QuantityScaled
	if den <= 0 {
		if b.CostQuantity == nil {
			return 0
		}
		num, den = *b.CostValue, *b.CostQuantity
	}
	p := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(num))
	d := big.NewInt(den)
	r, m := new(big.Int).QuoRem(p, d, new(big.Int))
	if m.Sign() != 0 && new(big.Int).Abs(new(big.Int).Mul(m, big.NewInt(2))).Cmp(d) >= 0 {
		r.Add(r, big.NewInt(int64(p.Sign())))
	}
	return r.Int64() // |quantity| and |value| are bounded to 10^15, so the result fits
}

// resolveReversals fills each reversal line from the movement it reverses: same item and outlet,
// opposite quantity and value.
func resolveReversals(ctx context.Context, q *db.Queries, tenantID uuid.UUID, lines []NewMovement) error {
	var ids []uuid.UUID
	for _, l := range lines {
		if l.Reverses != nil {
			ids = append(ids, *l.Reverses)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.MovementsToReverse(ctx, db.MovementsToReverseParams{TenantID: tenantID, Ids: ids})
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]db.StockMovement, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	for i := range lines {
		l := &lines[i]
		if l.Reverses == nil {
			continue
		}
		orig, ok := byID[*l.Reverses]
		if !ok {
			return fmt.Errorf("%w: movement %s to reverse does not exist", kernel.ErrValidation, l.Reverses)
		}
		if orig.Kind == KindReversal {
			return fmt.Errorf("%w: a reversal is not reversed; post the movement again", kernel.ErrValidation)
		}
		v := -orig.Value
		l.OutletID, l.StockItemID, l.Kind, l.QuantityScaled, l.Value = orig.OutletID, orig.StockItemID, KindReversal, -orig.QuantityScaled, &v
	}
	return nil
}

// checkLines refuses what the database would not explain well: an unknown kind, the wrong sign, a
// date in the future, an untracked or archived item.
func (s *Service) checkLines(ctx context.Context, q *db.Queries, tenantID uuid.UUID, lines []NewMovement) error {
	now := s.Clock.Now()
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		dir, ok := direction[l.Kind]
		switch {
		case !ok:
			return fmt.Errorf("%w: unknown movement kind %q", kernel.ErrValidation, l.Kind)
		case l.Kind == KindReversal && l.Reverses == nil:
			return fmt.Errorf("%w: a reversal names the movement it reverses", kernel.ErrValidation)
		case l.QuantityScaled == 0 || l.QuantityScaled > maxQuantity || l.QuantityScaled < -maxQuantity:
			return fmt.Errorf("%w: a quantity is not 0 and at most %d thousandths", kernel.ErrValidation, int64(maxQuantity))
		case dir != 0 && (l.QuantityScaled > 0) != (dir > 0):
			return fmt.Errorf("%w: a %s moves stock %s", kernel.ErrValidation, l.Kind, map[int]string{1: "in", -1: "out"}[dir])
		case l.Value != nil && l.Kind != KindReversal && (l.QuantityScaled < 0 || *l.Value < 0 || *l.Value > maxQuantity):
			return fmt.Errorf("%w: only stock in carries a value, 0 or more rupiah", kernel.ErrValidation)
		case l.Kind != KindSaleConsumption && l.OccurredAt.After(now.Add(futureSlack)):
			return fmt.Errorf("%w: a movement cannot be dated in the future (BR-STK-05)", kernel.ErrValidation)
		}
		ids = append(ids, l.StockItemID)
	}
	items, err := q.ItemsForPosting(ctx, db.ItemsForPostingParams{TenantID: tenantID, Ids: ids})
	if err != nil {
		return err
	}
	type state struct{ track, archived bool }
	known := make(map[uuid.UUID]state, len(items))
	for _, it := range items {
		known[it.ID] = state{it.Track, it.ArchivedAt != nil}
	}
	for _, l := range lines {
		st, ok := known[l.StockItemID]
		switch {
		case !ok:
			return fmt.Errorf("%w: stock item %s does not exist", kernel.ErrValidation, l.StockItemID)
		case !st.track:
			return fmt.Errorf("%w: an untracked item has no stock (BR-ITM-03)", kernel.ErrValidation)
		case st.archived && l.Kind != KindSaleConsumption && l.Kind != KindReversal:
			return fmt.Errorf("%w: an archived item takes no new movements (BR-GEN-06)", kernel.ErrValidation)
		}
	}
	return nil
}

// Drift is a balance that does not match its ledger (BR-STK-08). Stored values are nil when the
// balance row is missing.
type Drift struct {
	TenantID, OutletID, StockItemID uuid.UUID
	Movements, LedgerSeq            int64
	LedgerQuantity, LedgerValue     int64
	StoredSeq                       *int64
	StoredQuantity, StoredValue     *int64
	ledgerCostQuantity              *int64
	ledgerCostValue                 *int64
}

// CheckBalances compares every balance with its ledger, for one business, or for all of them when
// tenantID is nil (which needs the platform role). A mismatch is a bug.
func CheckBalances(ctx context.Context, q db.DBTX, tenantID *uuid.UUID) ([]Drift, error) {
	rows, err := db.New(q).BalanceDrift(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Drift, len(rows))
	for i, r := range rows {
		out[i] = Drift{
			TenantID: r.TenantID, OutletID: r.OutletID, StockItemID: r.StockItemID, Movements: r.Movements, LedgerSeq: r.LedgerSeq,
			LedgerQuantity: r.LedgerQuantity, LedgerValue: r.LedgerValue, StoredSeq: r.StoredSeq, StoredQuantity: r.StoredQuantity,
			StoredValue: r.StoredValue, ledgerCostQuantity: r.LedgerCostQuantity, ledgerCostValue: r.LedgerCostValue,
		}
	}
	return out, nil
}

// RepairBalances sets every drifted balance of a business to what its ledger says and returns what
// was repaired. The ledger is not touched. A ledger with a gap in its posting order cannot be
// repaired this way and is returned as an error.
func RepairBalances(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Drift, error) {
	drift, err := CheckBalances(ctx, tx, &tenantID)
	if err != nil {
		return nil, err
	}
	q := db.New(tx)
	for _, d := range drift {
		if d.Movements != d.LedgerSeq {
			return nil, fmt.Errorf("inventory: the ledger of item %s at outlet %s has %d movements up to number %d", d.StockItemID, d.OutletID, d.Movements, d.LedgerSeq)
		}
		if err := q.RepairBalance(ctx, db.RepairBalanceParams{
			TenantID: d.TenantID, OutletID: d.OutletID, StockItemID: d.StockItemID, QuantityScaled: d.LedgerQuantity, Value: d.LedgerValue,
			CostQuantity: d.ledgerCostQuantity, CostValue: d.ledgerCostValue, Seq: d.LedgerSeq,
		}); err != nil {
			return nil, err
		}
	}
	return drift, nil
}
