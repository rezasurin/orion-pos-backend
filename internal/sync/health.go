package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
	"github.com/rezasurin/orion-pos-backend/internal/sync/db"
)

// Alert kinds.
const (
	AlertUnsyncedEvents  = "unsynced_events"   // the device reported events older than the threshold still in its outbox
	AlertSilentOpenShift = "silent_open_shift" // a shift is open but the device has not been heard from
)

const (
	// DefaultUnsyncedAfter is how old the oldest undelivered event may be before the owner is told.
	DefaultUnsyncedAfter = 30 * time.Minute
	// DefaultSilentAfter is how long a device with an open shift may stay silent.
	DefaultSilentAfter = 3 * time.Hour
	// activeWindow limits the monitor to devices seen in the last week, so a tablet left in a drawer
	// does not alert forever.
	activeWindow = 7 * 24 * time.Hour
	// openShiftWindow limits the silent-device check to shifts that arrived in the last day.
	openShiftWindow = 24 * time.Hour
	// MonitorInterval is how often the monitor runs.
	MonitorInterval = 5 * time.Minute
)

// HealthArgs is the periodic job that checks every device.
type HealthArgs struct{}

func (HealthArgs) Kind() string { return "device_health" }

// HealthDeps are what the monitor needs. It belongs to `orion worker`.
type HealthDeps struct {
	Platform *pgxpool.Pool // a member of orion_platform: the monitor spans tenants
	Sender   notify.Sender
	Clock    kernel.Clock
	Logger   *slog.Logger
	// UnsyncedAfter and SilentAfter are the thresholds; zero takes the defaults.
	UnsyncedAfter time.Duration
	SilentAfter   time.Duration
	// OperatorEmail, if set, receives a copy of every alert, so Orion staff hear about a stuck pilot
	// device as soon as its owner does.
	OperatorEmail string
}

// HealthJobs runs the device monitor.
type HealthJobs struct{ HealthDeps }

// NewHealthJobs returns the monitor.
func NewHealthJobs(d HealthDeps) *HealthJobs {
	if d.Clock == nil {
		d.Clock = kernel.SystemClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.UnsyncedAfter == 0 {
		d.UnsyncedAfter = DefaultUnsyncedAfter
	}
	if d.SilentAfter == 0 {
		d.SilentAfter = DefaultSilentAfter
	}
	return &HealthJobs{d}
}

// AddWorkers registers the job handler.
func (h *HealthJobs) AddWorkers(w *river.Workers) { river.AddWorker(w, &healthWorker{h: h}) }

// PeriodicJobs schedules the monitor.
func (h *HealthJobs) PeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(MonitorInterval),
			func() (river.JobArgs, *river.InsertOpts) { return HealthArgs{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true},
		),
	}
}

type healthWorker struct {
	river.WorkerDefaults[HealthArgs]
	h *HealthJobs
}

func (w *healthWorker) Work(ctx context.Context, _ *river.Job[HealthArgs]) error {
	_, err := w.h.Check(ctx)
	return err
}

// HealthReport says what one run did.
type HealthReport struct {
	Opened   int // new incidents
	Resolved int // incidents whose condition cleared
	Notified int // incidents whose owners were emailed
}

// Check looks at every device once. It opens an incident for each device that newly meets a
// condition, emails the owners once per incident, and resolves incidents whose condition has
// cleared (the device delivered its events, or spoke up, or was revoked or its shift closed).
// Running it again changes nothing until something else does.
func (h *HealthJobs) Check(ctx context.Context) (HealthReport, error) {
	q := db.New(h.Platform)
	now := h.Clock.Now()
	var rep HealthReport

	type key struct {
		device uuid.UUID
		kind   string
	}
	type found struct {
		tenant uuid.UUID
		detail map[string]any
	}
	detected := map[key]found{}

	unsynced, err := q.FindUnsyncedDevices(ctx, db.FindUnsyncedDevicesParams{
		UnsyncedBefore: ptr(now.Add(-h.UnsyncedAfter)), ActiveAfter: ptr(now.Add(-activeWindow)),
	})
	if err != nil {
		return rep, err
	}
	for _, d := range unsynced {
		detected[key{d.ID, AlertUnsyncedEvents}] = found{d.TenantID, map[string]any{
			"unsynced_events": d.UnsyncedEvents, "oldest_unsynced_at": d.OldestUnsyncedAt, "last_sync_at": d.LastSyncAt,
		}}
	}
	silent, err := q.FindSilentOpenShiftDevices(ctx, db.FindSilentOpenShiftDevicesParams{
		OpenedAfter: now.Add(-openShiftWindow), SilentBefore: ptr(now.Add(-h.SilentAfter)),
	})
	if err != nil {
		return rep, err
	}
	for _, d := range silent {
		detected[key{d.ID, AlertSilentOpenShift}] = found{d.TenantID, map[string]any{
			"shift_id": d.ShiftID, "shift_opened_at": d.OpenedAt, "last_contact": d.LastContact,
		}}
	}

	open, err := q.ListOpenAlerts(ctx)
	if err != nil {
		return rep, err
	}
	have := map[key]bool{}
	for _, a := range open {
		k := key{a.DeviceID, a.Kind}
		have[k] = true
		if _, still := detected[k]; !still {
			if err := q.ResolveAlert(ctx, db.ResolveAlertParams{TenantID: a.TenantID, ID: a.ID, ResolvedAt: &now}); err != nil {
				return rep, err
			}
			rep.Resolved++
		}
	}
	for k, f := range detected {
		if have[k] {
			continue
		}
		raw, err := json.Marshal(f.detail)
		if err != nil {
			return rep, err
		}
		n, err := q.InsertAlert(ctx, db.InsertAlertParams{
			ID: kernel.NewID(), TenantID: f.tenant, DeviceID: k.device, Kind: k.kind, OpenedAt: now, Detail: raw,
		})
		if err != nil {
			return rep, err
		}
		rep.Opened += int(n)
	}

	// Tell people about every open incident not yet told, which also retries one whose email failed.
	open, err = q.ListOpenAlerts(ctx)
	if err != nil {
		return rep, err
	}
	for _, a := range open {
		if a.NotifiedAt != nil {
			continue
		}
		sent, err := h.notify(ctx, q, a)
		if err != nil {
			h.Logger.ErrorContext(ctx, "device health: could not send the alert", slog.String("device_id", a.DeviceID.String()), slog.Any("error", err))
			continue // notified_at stays empty, so the next run tries again
		}
		if err := q.MarkAlertNotified(ctx, db.MarkAlertNotifiedParams{TenantID: a.TenantID, ID: a.ID, NotifiedAt: &now}); err != nil {
			return rep, err
		}
		if sent > 0 {
			rep.Notified++
		}
	}
	return rep, nil
}

// notify emails the owners (and the operator, if configured) and returns how many messages it sent.
func (h *HealthJobs) notify(ctx context.Context, q *db.Queries, a db.ListOpenAlertsRow) (int, error) {
	owners, err := q.ListOwnerContacts(ctx, a.TenantID)
	if err != nil {
		return 0, err
	}
	var detail map[string]any
	_ = json.Unmarshal(a.Detail, &detail)

	sent := 0
	for _, o := range owners {
		subject, text := alertEmail(o.Locale, a.Kind, a.DeviceName, a.OutletName, detail)
		if err := h.Sender.Send(ctx, notify.Message{To: o.Email, Subject: subject, Text: text}); err != nil {
			return sent, fmt.Errorf("send to an owner: %w", err)
		}
		sent++
	}
	if h.OperatorEmail != "" {
		subject, text := alertEmail("en", a.Kind, a.DeviceName, a.OutletName, detail)
		text = fmt.Sprintf("Business %s\n\n%s", a.TenantID, text)
		if err := h.Sender.Send(ctx, notify.Message{To: h.OperatorEmail, Subject: subject, Text: text}); err != nil {
			return sent, fmt.Errorf("send to the operator: %w", err)
		}
		sent++
	}
	if sent == 0 {
		h.Logger.WarnContext(ctx, "device health: an alert has nobody to tell (no owner with a verified email)",
			slog.String("tenant_id", a.TenantID.String()), slog.String("device_id", a.DeviceID.String()))
	}
	return sent, nil
}

// alertEmail renders an incident in the owner's language. It says what is wrong and what to do about
// it, and nothing a reader must decode.
func alertEmail(locale, kind, device, outlet string, detail map[string]any) (subject, text string) {
	count := ""
	if n, ok := detail["unsynced_events"].(float64); ok {
		count = fmt.Sprintf("%d", int(n))
	}
	if locale == "en" {
		switch kind {
		case AlertSilentOpenShift:
			return fmt.Sprintf("Orion POS: %s has an open shift but has not connected", device),
				strings.Join([]string{
					fmt.Sprintf("The device %q at %s has a shift open, but it has not connected to Orion for a while.", device, outlet),
					"",
					"Sales made on it are safe on the device, but they will not appear in your reports until it connects.",
					"Check that the tablet is on and has an internet connection, and open the Orion app.",
				}, "\n")
		}
		return fmt.Sprintf("Orion POS: %s has sales that have not synced", device),
			strings.Join([]string{
				fmt.Sprintf("The device %q at %s is holding %s sale or shift records that have not reached Orion for over half an hour.", device, outlet, count),
				"",
				"They are safe on the device, but your reports will be missing them until it syncs.",
				"Check that the tablet has an internet connection and open the Orion app. Do not clear the app's data or sign out.",
			}, "\n")
	}
	switch kind {
	case AlertSilentOpenShift:
		return fmt.Sprintf("Orion POS: %s memiliki shift terbuka tetapi belum terhubung", device),
			strings.Join([]string{
				fmt.Sprintf("Perangkat %q di %s memiliki shift yang masih terbuka, tetapi sudah beberapa waktu tidak terhubung ke Orion.", device, outlet),
				"",
				"Penjualan di perangkat itu aman, tetapi baru muncul di laporan setelah perangkat terhubung.",
				"Pastikan tablet menyala dan terhubung ke internet, lalu buka aplikasi Orion.",
			}, "\n")
	}
	return fmt.Sprintf("Orion POS: %s memiliki penjualan yang belum tersinkron", device),
		strings.Join([]string{
			fmt.Sprintf("Perangkat %q di %s menyimpan %s catatan penjualan atau shift yang belum sampai ke Orion selama lebih dari setengah jam.", device, outlet, count),
			"",
			"Data itu aman di perangkat, tetapi laporan Anda belum memuatnya sampai perangkat tersinkron.",
			"Pastikan tablet terhubung ke internet dan buka aplikasi Orion. Jangan hapus data aplikasi atau keluar dari akun.",
		}, "\n")
}

func ptr[T any](v T) *T { return &v }
