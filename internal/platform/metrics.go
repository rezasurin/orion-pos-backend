package platform

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform/db"
)

// Usage metrics for the operator console (task B2.9, ADR 0008): per business and day, how many sales
// and events, never amounts or names.

// MetricsZone is the day the sync figures are counted in. Sales count by their outlet's business
// date.
var MetricsZone = time.FixedZone("WIB", 7*3600)

// metricsLookback is how many days each run recomputes, today included: a tablet offline for a few
// days delivers sales that belong to earlier days.
const metricsLookback = 7

// DailyMetrics is one business's usage on one day.
type DailyMetrics struct {
	Day            time.Time
	Sales          int
	VoidedSales    int
	Events         int
	RejectedEvents int
	DevicesSynced  int
	Flags          int
	ComputedAt     time.Time
}

// MetricsArgs is the nightly job that computes the metrics.
type MetricsArgs struct{}

func (MetricsArgs) Kind() string { return "tenant_daily_metrics" }

// MetricsJobs computes the metrics in `orion worker`, on a pool whose role is a member of
// orion_platform.
type MetricsJobs struct {
	Platform *pgxpool.Pool
	Clock    kernel.Clock
	Logger   *slog.Logger
}

// AddWorkers registers the job handler.
func (m *MetricsJobs) AddWorkers(w *river.Workers) { river.AddWorker(w, &metricsWorker{m: m}) }

// PeriodicJobs runs the job at 01:00 in Jakarta, and when the worker starts.
func (m *MetricsJobs) PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{river.NewPeriodicJob(dailyAt{hour: 1}, func() (river.JobArgs, *river.InsertOpts) { return MetricsArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true})}
}

type metricsWorker struct {
	river.WorkerDefaults[MetricsArgs]
	m *MetricsJobs
}

func (w *metricsWorker) Work(ctx context.Context, _ *river.Job[MetricsArgs]) error {
	n, err := w.m.Compute(ctx)
	if err == nil && w.m.Logger != nil {
		w.m.Logger.Info("tenant metrics computed", slog.Int64("rows", n))
	}
	return err
}

// Compute recomputes the last week of metrics for every business, today included, and returns how
// many rows it wrote. Running it again gives the same rows.
func (m *MetricsJobs) Compute(ctx context.Context) (int64, error) {
	now := m.Clock.Now()
	today := dateIn(now, MetricsZone)
	return db.New(m.Platform).ComputeDailyMetrics(ctx, db.ComputeDailyMetricsParams{
		Now: now, FromDay: pgDate(today.AddDate(0, 0, -(metricsLookback - 1))), ToDay: pgDate(today),
	})
}

// dailyAt is a river schedule that fires once a day at an hour in MetricsZone.
type dailyAt struct{ hour int }

func (d dailyAt) Next(t time.Time) time.Time {
	l := t.In(MetricsZone)
	next := time.Date(l.Year(), l.Month(), l.Day(), d.hour, 0, 0, 0, MetricsZone)
	if !next.After(t) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// TenantMetrics returns a business's metrics from the last `days` days, oldest first. Days the job
// has not reached yet are missing.
func (s *Service) TenantMetrics(ctx context.Context, tenantSlug string, days int) ([]DailyMetrics, error) {
	if days < 1 || days > 366 {
		return nil, fmt.Errorf("%w: days must be between 1 and 366", kernel.ErrValidation)
	}
	var out []DailyMetrics
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.FindBySlug(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		today := dateIn(s.Clock.Now(), MetricsZone)
		rows, err := db.New(tx).ListDailyMetrics(ctx, db.ListDailyMetricsParams{
			TenantID: t.ID, FromDay: pgDate(today.AddDate(0, 0, -(days - 1))), ToDay: pgDate(today),
		})
		if err != nil {
			return err
		}
		out = make([]DailyMetrics, len(rows))
		for i, r := range rows {
			out[i] = DailyMetrics{
				Day: r.Day.Time, Sales: int(r.Sales), VoidedSales: int(r.VoidedSales), Events: int(r.Events),
				RejectedEvents: int(r.RejectedEvents), DevicesSynced: int(r.DevicesSynced), Flags: int(r.Flags), ComputedAt: r.ComputedAt,
			}
		}
		return nil
	})
	return out, err
}

// QuietDevice is a tablet that has stopped syncing.
type QuietDevice struct {
	TenantSlug, TenantName, OutletCode, Name string
	DeviceID                                 uuid.UUID
	Code                                     int
	PairedAt                                 time.Time
	LastSyncAt, LastSeenAt                   *time.Time // last seen without a sync: the app runs but cannot sync
	AppVersion                               *string
	UnsyncedEvents                           int
	OldestUnsyncedAt                         *time.Time
}

// stoppedSyncingWindow keeps tablets that went quiet long ago (a spare in a drawer) off the list.
const stoppedSyncingWindow = 30 * 24 * time.Hour

// StoppedSyncing lists the tablets, across every business that is not suspended, that synced (or were
// paired) in the last 30 days but not in the last `quiet`. A paired tablet that never synced counts
// from its pairing. At most 500, the longest silent first.
func (s *Service) StoppedSyncing(ctx context.Context, quiet time.Duration) ([]QuietDevice, error) {
	if quiet < time.Minute || quiet > stoppedSyncingWindow {
		return nil, fmt.Errorf("%w: quiet must be between a minute and 30 days", kernel.ErrValidation)
	}
	now := s.Clock.Now()
	rows, err := db.New(s.Pool).StoppedSyncing(ctx, db.StoppedSyncingParams{QuietSince: now.Add(-quiet), WindowStart: now.Add(-stoppedSyncingWindow)})
	if err != nil {
		return nil, err
	}
	out := make([]QuietDevice, len(rows))
	for i, r := range rows {
		out[i] = QuietDevice{
			TenantSlug: r.TenantSlug, TenantName: r.TenantName, OutletCode: r.OutletCode, Name: r.DeviceName, DeviceID: r.DeviceID,
			Code: int(r.DeviceCode), PairedAt: r.PairedAt, LastSyncAt: r.LastSyncAt, LastSeenAt: r.LastSeenAt, AppVersion: r.AppVersion,
			UnsyncedEvents: int(r.UnsyncedEvents), OldestUnsyncedAt: r.OldestUnsyncedAt,
		}
	}
	return out, nil
}

func dateIn(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}

func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: true} }
