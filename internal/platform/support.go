package platform

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform/db"
)

// supportWindow is how far back the support report counts rejected events and flags.
const supportWindow = 7 * 24 * time.Hour

// SupportDevice is one tablet's health.
type SupportDevice struct {
	ID               uuid.UUID
	Name             string
	OutletCode       string
	Code             int
	Revoked          bool
	LastSeenAt       *time.Time
	LastSyncAt       *time.Time
	AppVersion       *string
	ClockSkewMs      *int32
	UnsyncedEvents   int32
	OldestUnsyncedAt *time.Time
	OpenAlerts       []string
}

// ParkedEvent is an event waiting for a record that has not arrived.
type ParkedEvent struct {
	ID         uuid.UUID
	Type       string
	DependsOn  uuid.UUID
	ReceivedAt time.Time
	Device     string
}

// CodeCount is how many of something carry a code.
type CodeCount struct {
	Code  string
	Count int64
}

// FlaggedSale is a sale with review flags.
type FlaggedSale struct {
	ID           uuid.UUID
	Receipt      string
	OutletCode   string
	BusinessDate time.Time
	Total        int64
	Status       string
	Codes        []string
	FlaggedAt    time.Time
}

// SupportReport is what an operator needs to see when a business says "my sales are not showing".
type SupportReport struct {
	TenantID      uuid.UUID
	TenantName    string
	Suspended     bool
	Devices       []SupportDevice
	Parked        []ParkedEvent
	Rejected      []CodeCount // events rejected in the last 7 days, by code
	Flags         []CodeCount // review flags raised in the last 7 days, by code
	FlaggedSales  []FlaggedSale
	GeneratedAt   time.Time
	WindowStartAt time.Time
}

// SupportReport reads one business's sync health: its devices (last contact, skew, undelivered events,
// open alerts), events parked waiting for a record, rejected events and review flags of the last week
// by code, and the most recently flagged sales. It changes nothing, but reading a business's data is
// an operator action, so it is audited with the reason.
func (s *Service) SupportReport(ctx context.Context, a Actor, tenantSlug, reason string) (SupportReport, error) {
	var out SupportReport
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		since := now.Add(-supportWindow)
		q := db.New(tx)

		devs, err := q.SupportDevices(ctx, t.ID)
		if err != nil {
			return err
		}
		parked, err := q.SupportParked(ctx, t.ID)
		if err != nil {
			return err
		}
		rejected, err := q.SupportRejected(ctx, db.SupportRejectedParams{TenantID: t.ID, Since: since})
		if err != nil {
			return err
		}
		flags, err := q.SupportFlagCounts(ctx, db.SupportFlagCountsParams{TenantID: t.ID, Since: since})
		if err != nil {
			return err
		}
		flagged, err := q.SupportFlaggedSales(ctx, db.SupportFlaggedSalesParams{TenantID: t.ID, Since: since})
		if err != nil {
			return err
		}

		out = SupportReport{
			TenantID: t.ID, TenantName: t.Name, Suspended: t.SuspendedAt != nil, GeneratedAt: now, WindowStartAt: since,
			Devices: make([]SupportDevice, len(devs)), Parked: make([]ParkedEvent, len(parked)),
			Rejected: make([]CodeCount, len(rejected)), Flags: make([]CodeCount, len(flags)), FlaggedSales: make([]FlaggedSale, len(flagged)),
		}
		for i, d := range devs {
			out.Devices[i] = SupportDevice{
				ID: d.ID, Name: d.Name, OutletCode: d.OutletCode, Code: int(d.DeviceCode), Revoked: d.RevokedAt != nil, LastSeenAt: d.LastSeenAt,
				LastSyncAt: d.LastSyncAt, AppVersion: d.AppVersion, ClockSkewMs: d.ClockSkewMs, UnsyncedEvents: d.UnsyncedEvents,
				OldestUnsyncedAt: d.OldestUnsyncedAt, OpenAlerts: d.OpenAlerts,
			}
		}
		for i, p := range parked {
			dep := uuid.Nil
			if p.DependsOn != nil {
				dep = *p.DependsOn
			}
			out.Parked[i] = ParkedEvent{ID: p.ID, Type: p.EventType, DependsOn: dep, ReceivedAt: p.ReceivedAt, Device: p.DeviceName}
		}
		for i, r := range rejected {
			out.Rejected[i] = CodeCount{Code: r.Code, Count: r.Events}
		}
		for i, f := range flags {
			out.Flags[i] = CodeCount{Code: f.Code, Count: f.Flags}
		}
		for i, f := range flagged {
			out.FlaggedSales[i] = FlaggedSale{
				ID: f.ID, Receipt: f.ReceiptNumber, OutletCode: f.OutletCode, BusinessDate: f.BusinessDate.Time, Total: f.Total,
				Status: f.Status, Codes: f.Codes, FlaggedAt: f.FlaggedAt,
			}
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "support.report_viewed", TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID, Reason: reason,
		})
	})
	return out, err
}

// AbandonParkedEvent gives up on an event that is waiting for a record that will never arrive (for
// example the sale it voids was lost with a wiped tablet). The event becomes rejected with the code
// "abandoned" and your reason; the device is told so on its next push of it. It is audited with the
// event's type and what it was waiting for.
func (s *Service) AbandonParkedEvent(ctx context.Context, a Actor, tenantSlug string, eventID uuid.UUID, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		q := db.New(tx)
		ev, err := q.GetParkedEventForUpdate(ctx, db.GetParkedEventForUpdateParams{TenantID: t.ID, ID: eventID})
		if err != nil {
			return fmt.Errorf("%w: no parked event %s in %s", kernel.ErrNotFound, eventID, tenantSlug)
		}
		detail := "abandoned by an operator: " + reason
		if err := q.AbandonParkedEvent(ctx, db.AbandonParkedEventParams{
			TenantID: t.ID, DeviceID: ev.DeviceID, IdempotencyKey: ev.IdempotencyKey, Detail: &detail,
		}); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "sync.event_abandoned", TargetType: "sync_event", TargetID: &eventID, TenantID: &t.ID,
			Before: map[string]any{"status": "pending_dependency", "event_type": ev.EventType, "depends_on": ev.DependsOn},
			After:  map[string]any{"status": "rejected", "code": "abandoned"}, Reason: reason,
		})
	})
}
