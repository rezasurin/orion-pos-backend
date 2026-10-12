package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/rezasurin/orion-pos-backend/internal/notify"
)

// CheckArgs is the nightly comparison of every stock balance with its ledger (BR-STK-08).
type CheckArgs struct{}

func (CheckArgs) Kind() string { return "stock_balance_check" }

// CheckJobs runs the check in `orion worker`, on a pool whose role is a member of orion_platform.
// A mismatch is a bug: it is logged at error level and, when OperatorEmail is set, emailed there.
// Nothing is repaired automatically; an operator looks first and runs `orion admin rebuild-stock`.
type CheckJobs struct {
	Platform      *pgxpool.Pool
	Sender        notify.Sender
	OperatorEmail string
	Logger        *slog.Logger
}

// AddWorkers registers the job handler.
func (c *CheckJobs) AddWorkers(w *river.Workers) { river.AddWorker(w, &checkWorker{c: c}) }

// PeriodicJobs runs the check daily, and when the worker starts.
//
// ponytail: compares every balance of every business in one query; check a sample per night if
// that query grows slow.
func (c *CheckJobs) PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) { return CheckArgs{}, nil }, &river.PeriodicJobOpts{RunOnStart: true})}
}

type checkWorker struct {
	river.WorkerDefaults[CheckArgs]
	c *CheckJobs
}

func (w *checkWorker) Work(ctx context.Context, _ *river.Job[CheckArgs]) error {
	_, err := w.c.Run(ctx)
	return err
}

// Run checks every balance and reports what drifted.
func (c *CheckJobs) Run(ctx context.Context) ([]Drift, error) {
	drift, err := CheckBalances(ctx, c.Platform, nil)
	if err != nil || len(drift) == 0 {
		return drift, err
	}
	var b strings.Builder
	for _, d := range drift {
		c.Logger.ErrorContext(ctx, "stock balance differs from its ledger", slog.String("tenant_id", d.TenantID.String()),
			slog.String("outlet_id", d.OutletID.String()), slog.String("stock_item_id", d.StockItemID.String()),
			slog.Int64("ledger_quantity", d.LedgerQuantity), slog.Any("stored_quantity", d.StoredQuantity))
		fmt.Fprintf(&b, "business %s, outlet %s, item %s: ledger %d thousandths and Rp %d, stored %s\n",
			d.TenantID, d.OutletID, d.StockItemID, d.LedgerQuantity, d.LedgerValue, stored(d))
	}
	if c.OperatorEmail != "" {
		err = c.Sender.Send(ctx, notify.Message{
			To:      c.OperatorEmail,
			Subject: fmt.Sprintf("Orion: %d stock balances differ from the ledger", len(drift)),
			Text: "The nightly check found stock balances that do not match their ledger. This is a bug; nothing was changed.\n" +
				"Check with `orion admin rebuild-stock --tenant SLUG`, then repair with --fix.\n\n" + b.String(),
		})
	}
	return drift, err
}

func stored(d Drift) string {
	if d.StoredQuantity == nil {
		return "nothing (no balance row)"
	}
	return fmt.Sprintf("%d thousandths and Rp %d", *d.StoredQuantity, *d.StoredValue)
}
