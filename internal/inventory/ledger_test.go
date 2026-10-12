package inventory_test

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/inventory"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
)

// ledger is a fixture with a staff member to post as and a tracked item based on grams.
type ledger struct {
	fixture
	staff uuid.UUID
	item  uuid.UUID
}

func newLedger(t *testing.T) ledger {
	t.Helper()
	f := newFixture(t)
	l := ledger{fixture: f, staff: uuid.New()}
	if _, err := f.d.Owner.Exec(context.Background(), `INSERT INTO staff (id, tenant_id, display_name) VALUES ($1, $2, 'Gudang')`, l.staff, f.tenant); err != nil {
		t.Fatal(err)
	}
	l.item = l.newItem(t, "Biji kopi")
	return l
}

func (l ledger) newItem(t *testing.T, name string) uuid.UUID {
	t.Helper()
	_, units := l.standard(t)
	it, err := l.svc.CreateStockItem(context.Background(), l.tenant, inventory.NewStockItem{
		Name: name, Type: inventory.TypeIngredient, CategoryID: l.category(t, "Kat "+name).ID, BaseUomID: units["g"].ID, Track: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return it.ID
}

func (l ledger) mv(kind string, qty int64) inventory.NewMovement {
	now := time.Now()
	return inventory.NewMovement{OutletID: l.outlet, StockItemID: l.item, Kind: kind, QuantityScaled: qty, StaffID: l.staff, OccurredAt: now, BusinessDate: now}
}

func priced(m inventory.NewMovement, value int64) inventory.NewMovement { m.Value = &value; return m }

func (l ledger) post(ctx context.Context, in ...inventory.NewMovement) ([]inventory.Movement, error) {
	var out []inventory.Movement
	err := kernel.TenantTx(ctx, l.d.App, l.tenant, func(tx pgx.Tx) error {
		var err error
		out, err = l.svc.Post(ctx, tx, l.tenant, in)
		return err
	})
	return out, err
}

func (l ledger) mustPost(t *testing.T, in ...inventory.NewMovement) []inventory.Movement {
	t.Helper()
	out, err := l.post(context.Background(), in...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type balance struct{ qty, value int64 }

func (l ledger) balance(t *testing.T, item uuid.UUID) balance {
	t.Helper()
	var b balance
	if err := l.d.Owner.QueryRow(context.Background(), `SELECT quantity_scaled, value FROM stock_balance WHERE tenant_id = $1 AND outlet_id = $2 AND stock_item_id = $3`,
		l.tenant, l.outlet, item).Scan(&b.qty, &b.value); err != nil {
		t.Fatal(err)
	}
	return b
}

func (l ledger) noDrift(t *testing.T) {
	t.Helper()
	drift, err := inventory.CheckBalances(context.Background(), l.d.Platform, nil)
	if err != nil || len(drift) != 0 {
		t.Fatalf("drift = %+v, %v", drift, err)
	}
}

// BR-CST-01 to 03: receipts add their cost, consumption removes round(quantity × average),
// rounding half away from zero. The figures are worked by hand.
func TestWeightedAverageCost(t *testing.T) {
	l := newLedger(t)
	l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 10_000_000), 150_000)) // 10 kg for Rp 150.000
	l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 5_000_000), 90_000))   // 5 kg for Rp 90.000
	if b := l.balance(t, l.item); b != (balance{15_000_000, 240_000}) {
		t.Fatalf("after receipts = %+v", b)
	}
	m := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -1_000_000)) // 1 kg at Rp 16 a gram
	if m[0].Value != -16_000 || m[0].Seq != 3 {
		t.Errorf("1 kg consumed = %+v, want Rp -16.000 as the third movement", m[0])
	}
	m = l.mustPost(t, l.mv(inventory.KindUsage, -333)) // 0.333 g × Rp 16 = 5.328
	if m[0].Value != -5 {
		t.Errorf("0.333 g used = Rp %d, want -5", m[0].Value)
	}
	// A receipt without a price is valued at the average; a remainder of exactly half rounds away
	// from zero.
	half := newLedger(t)
	half.mustPost(t, priced(half.mv(inventory.KindOpening, 2000), 3)) // 2 g for Rp 3
	if m := half.mustPost(t, half.mv(inventory.KindWaste, -1000)); m[0].Value != -2 {
		t.Errorf("1 g of Rp 1,5 = Rp %d, want -2", m[0].Value)
	}
	if m := half.mustPost(t, half.mv(inventory.KindAdjustment, 3000)); m[0].Value != 3 {
		t.Errorf("3 g found at Rp 1 a gram = Rp %d, want 3", m[0].Value)
	}
	l.noDrift(t)
}

// BR-STK-04 and BR-CST-04: a sale may take the balance below 0 and keeps using the last known
// average; manual removals are refused instead, and nothing of a refused posting stays.
func TestNegativeStock(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	// No cost known yet: a sale from an empty shelf is valued at 0.
	if m := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -500)); m[0].Value != 0 {
		t.Errorf("a sale with no cost known = Rp %d", m[0].Value)
	}
	l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 2500), 2500)) // back to 2 g, Rp 2.500 total
	if b := l.balance(t, l.item); b != (balance{2000, 2500}) {
		t.Fatalf("after the receipt = %+v", b)
	}
	// 3 g sold from 2 g at Rp 1.250 a gram: the balance goes to -1 g; the next sale still uses
	// Rp 1.250 a gram, the last known average.
	if m := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -3000)); m[0].Value != -3750 {
		t.Errorf("3 g sold = Rp %d, want -3.750", m[0].Value)
	}
	if m := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -1000)); m[0].Value != -1250 {
		t.Errorf("1 g sold below 0 = Rp %d, want -1.250 (last known average)", m[0].Value)
	}

	for _, kind := range []string{inventory.KindUsage, inventory.KindWaste, inventory.KindSupplierReturn, inventory.KindTransferOut, inventory.KindAdjustment, inventory.KindProductionOut} {
		if _, err := l.post(ctx, l.mv(kind, -1)); !errors.Is(err, inventory.ErrInsufficientStock) {
			t.Errorf("%s below 0: %v", kind, err)
		}
	}
	// A whole posting is refused when one line would go below 0, and leaves nothing behind.
	other := l.newItem(t, "Susu")
	in := l.mv(inventory.KindPurchaseReceipt, 1000)
	in.StockItemID = other
	if _, err := l.post(ctx, in, l.mv(inventory.KindWaste, -1)); !errors.Is(err, inventory.ErrInsufficientStock) {
		t.Fatalf("mixed posting: %v", err)
	}
	var n int
	_ = l.d.Owner.QueryRow(ctx, `SELECT count(*) FROM stock_movement WHERE stock_item_id = $1`, other).Scan(&n)
	if n != 0 {
		t.Errorf("a refused posting left %d movements", n)
	}
	// Stock taken exactly to 0 is fine; an opname may set any count.
	l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 3000), 3000))
	l.mustPost(t, l.mv(inventory.KindUsage, -1000))
	if b := l.balance(t, l.item); b.qty != 0 {
		t.Fatalf("balance = %+v, want 0", b)
	}
	l.mustPost(t, l.mv(inventory.KindOpnameAdjustment, -200))
	l.noDrift(t)
}

func TestPostingRefusesWhatItCannotRecord(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	untracked, err := l.svc.UpdateStockItem(ctx, l.tenant, l.newItem(t, "Es batu"), inventory.UpdateStockItem{Track: ptr(false)})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := l.svc.UpdateStockItem(ctx, l.tenant, l.newItem(t, "Sirup lama"), inventory.UpdateStockItem{Archived: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	future := l.mv(inventory.KindPurchaseReceipt, 1000)
	future.OccurredAt = time.Now().Add(time.Hour)
	withItem := func(m inventory.NewMovement, id uuid.UUID) inventory.NewMovement { m.StockItemID = id; return m }
	for name, m := range map[string]inventory.NewMovement{
		"unknown kind":        l.mv("theft", -1),
		"receipt going out":   l.mv(inventory.KindPurchaseReceipt, -1000),
		"waste coming in":     l.mv(inventory.KindWaste, 1000),
		"zero":                l.mv(inventory.KindAdjustment, 0),
		"too much":            l.mv(inventory.KindPurchaseReceipt, 1_000_000_000_000_001),
		"value on stock out":  priced(l.mv(inventory.KindWaste, -1), 5),
		"negative value in":   priced(l.mv(inventory.KindPurchaseReceipt, 1000), -5),
		"in the future":       future,
		"reversal of nothing": {Kind: inventory.KindReversal, StaffID: l.staff, OccurredAt: time.Now(), Reverses: ptr(uuid.New())},
		"reversal without id": l.mv(inventory.KindReversal, 1000),
		"untracked item":      withItem(l.mv(inventory.KindPurchaseReceipt, 1000), untracked.ID),
		"archived item":       withItem(l.mv(inventory.KindPurchaseReceipt, 1000), archived.ID),
		"missing item":        withItem(l.mv(inventory.KindPurchaseReceipt, 1000), uuid.New()),
	} {
		if _, err := l.post(ctx, m); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A sale may still consume an archived item (an old recipe), and is dated by the tablet.
	sale := withItem(l.mv(inventory.KindSaleConsumption, -1000), archived.ID)
	sale.OccurredAt = time.Now().Add(time.Hour)
	if _, err := l.post(ctx, sale); err != nil {
		t.Errorf("a sale of an archived item: %v", err)
	}
}

// BR-GEN-05: a mistake is corrected by a reversal, which mirrors the movement exactly, once.
func TestReversal(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	rcpt := l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 10_000), 1000))[0]
	sale := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -3_000))[0] // Rp -300
	rev := func(id uuid.UUID) inventory.NewMovement {
		return inventory.NewMovement{Reverses: &id, StaffID: l.staff, OccurredAt: time.Now(), BusinessDate: time.Now(), Reason: ptr("void")}
	}
	got := l.mustPost(t, rev(sale.ID))[0]
	if got.Kind != inventory.KindReversal || got.QuantityScaled != 3_000 || got.Value != 300 || *got.Reverses != sale.ID {
		t.Errorf("reversal = %+v", got)
	}
	if b := l.balance(t, l.item); b != (balance{10_000, 1000}) {
		t.Errorf("after reversing the sale = %+v", b)
	}
	if _, err := l.post(ctx, rev(sale.ID)); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("reversing twice: %v", err)
	}
	if _, err := l.post(ctx, rev(got.ID)); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("reversing a reversal: %v", err)
	}
	// Reversing a receipt whose stock was used is a manual removal below 0.
	l.mustPost(t, l.mv(inventory.KindUsage, -1))
	if _, err := l.post(ctx, rev(rcpt.ID)); !errors.Is(err, inventory.ErrInsufficientStock) {
		t.Errorf("reversing a used receipt: %v", err)
	}
	l.noDrift(t)
}

// BR-STK-07: postings to one item queue on its balance; none is lost and the order is gapless.
func TestConcurrentPostingsQueue(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	second := l.newItem(t, "Gula")
	l.mustPost(t, priced(l.mv(inventory.KindOpening, 1_000_000), 100_000))
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 40 {
		wg.Go(func() {
			// Two items in alternating order in one posting: locking in item order avoids deadlocks.
			a, b := l.mv(inventory.KindSaleConsumption, -1000), l.mv(inventory.KindPurchaseReceipt, 500)
			b.StockItemID = second
			if i%2 == 0 {
				a, b = b, a
			}
			_, err := l.post(ctx, a, b)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b := l.balance(t, l.item); b.qty != 1_000_000-40*1000 {
		t.Errorf("balance = %+v", b)
	}
	l.noDrift(t)
}

// Whatever is posted, in whatever order, the stored balance equals the ledger replayed.
func TestBalancesAlwaysMatchTheLedger(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	items := []uuid.UUID{l.item, l.newItem(t, "Gula"), l.newItem(t, "Susu")}
	rng := rand.New(rand.NewPCG(7, 9))
	var posted []inventory.Movement
	for range 300 {
		m := l.mv(inventory.KindPurchaseReceipt, rng.Int64N(5_000_000)+1)
		m.StockItemID = items[rng.IntN(len(items))]
		switch r := rng.IntN(10); {
		case r < 3:
			m = priced(m, rng.Int64N(200_000))
		case r < 7:
			m.Kind, m.QuantityScaled = inventory.KindSaleConsumption, -m.QuantityScaled
		case r < 9:
			m.Kind = inventory.KindOpnameAdjustment
			if rng.IntN(2) == 0 {
				m.QuantityScaled = -m.QuantityScaled
			}
		default:
			if len(posted) > 0 {
				m = inventory.NewMovement{Reverses: &posted[rng.IntN(len(posted))].ID, StaffID: l.staff, OccurredAt: time.Now(), BusinessDate: time.Now()}
			}
		}
		out, err := l.post(ctx, m)
		if err != nil && !errors.Is(err, kernel.ErrConflict) && !errors.Is(err, inventory.ErrInsufficientStock) && !errors.Is(err, kernel.ErrValidation) {
			t.Fatal(err)
		}
		posted = append(posted, out...)
	}
	l.noDrift(t)
}

func TestRebuildFindsAndRepairsDrift(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	other := l.newItem(t, "Gula")
	l.mustPost(t, priced(l.mv(inventory.KindPurchaseReceipt, 10_000), 1000), l.mv(inventory.KindSaleConsumption, -12_000))
	in := priced(l.mv(inventory.KindPurchaseReceipt, 4000), 400)
	in.StockItemID = other
	l.mustPost(t, in)
	want := l.balance(t, l.item)

	// A bug corrupts one balance and loses the other.
	if _, err := l.d.Owner.Exec(ctx, `UPDATE stock_balance SET quantity_scaled = quantity_scaled + 1, cost_value = 7 WHERE stock_item_id = $1`, l.item); err != nil {
		t.Fatal(err)
	}
	if _, err := l.d.Owner.Exec(ctx, `DELETE FROM stock_balance WHERE stock_item_id = $1`, other); err != nil {
		t.Fatal(err)
	}
	sender := &notify.MemorySender{}
	job := &inventory.CheckJobs{Platform: l.d.Platform, Sender: sender, OperatorEmail: "ops@orion.test", Logger: discard()}
	drift, err := job.Run(ctx)
	if err != nil || len(drift) != 2 {
		t.Fatalf("drift = %+v, %v", drift, err)
	}
	if sent := sender.Sent(); len(sent) != 1 || !strings.Contains(sent[0].Text, "nothing (no balance row)") {
		t.Errorf("alert = %+v", sent)
	}
	var repaired []inventory.Drift
	if err := kernel.TenantTx(ctx, l.d.App, l.tenant, func(tx pgx.Tx) error {
		repaired, err = inventory.RepairBalances(ctx, tx, l.tenant)
		return err
	}); err != nil || len(repaired) != 2 {
		t.Fatalf("repaired = %+v, %v", repaired, err)
	}
	l.noDrift(t)
	if b := l.balance(t, l.item); b != want {
		t.Errorf("repaired balance = %+v, want %+v", b, want)
	}
	// The repaired balance carries the last known average: the next sale below 0 uses it.
	if m := l.mustPost(t, l.mv(inventory.KindSaleConsumption, -1000)); m[0].Value != -100 {
		t.Errorf("a sale after repair = Rp %d, want -100", m[0].Value)
	}
	// Repair sees only its own business.
	if err := kernel.TenantTx(ctx, l.d.App, uuid.New(), func(tx pgx.Tx) error {
		repaired, err = inventory.RepairBalances(ctx, tx, l.tenant)
		return err
	}); err != nil || len(repaired) != 0 {
		t.Errorf("another business's repair touched %d balances (%v)", len(repaired), err)
	}
}

// BR-ITM-07 and BR-ITM-08.
func TestItemsWithStockKeepTheirShape(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	_, units := l.standard(t)
	l.mustPost(t, priced(l.mv(inventory.KindOpening, 1000), 10))
	if _, err := l.svc.UpdateStockItem(ctx, l.tenant, l.item, inventory.UpdateStockItem{BaseUomID: ptr(units["ml"].ID)}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("changing the base unit after a movement: %v", err)
	}
	if _, err := l.svc.UpdateStockItem(ctx, l.tenant, l.item, inventory.UpdateStockItem{Archived: ptr(true)}); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("archiving with stock on hand: %v", err)
	}
	l.mustPost(t, l.mv(inventory.KindWaste, -1000))
	if _, err := l.svc.UpdateStockItem(ctx, l.tenant, l.item, inventory.UpdateStockItem{Archived: ptr(true)}); err != nil {
		t.Errorf("archiving at 0: %v", err)
	}
}

func TestTheLedgerIsAppendOnly(t *testing.T) {
	l := newLedger(t)
	ctx := context.Background()
	id := l.mustPost(t, priced(l.mv(inventory.KindOpening, 1000), 10))[0].ID
	for _, stmt := range []string{
		`UPDATE stock_movement SET quantity_scaled = 2000 WHERE id = $1`,
		`DELETE FROM stock_movement WHERE id = $1`,
	} {
		for name, pool := range map[string]func(func(pgx.Tx) error) error{
			"app":      func(fn func(pgx.Tx) error) error { return kernel.TenantTx(ctx, l.d.App, l.tenant, fn) },
			"platform": func(fn func(pgx.Tx) error) error { return pgx.BeginFunc(ctx, l.d.Platform, fn) },
		} {
			if err := pool(func(tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt, id); return err }); err == nil {
				t.Errorf("%s ran %q", name, stmt)
			}
		}
	}
}
