package platform

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform/db"
)

// The operator console (task B2.8): the business list and detail, and the changes an operator makes
// to a business. Every change is audited with its reason, in the same transaction. Methods that
// take a tenant accept its id or its slug.

// AdminTenant is a business as the console lists it. Sales7d and Events7d come from the nightly
// metrics, so they lag by up to a day.
type AdminTenant struct {
	ID                 uuid.UUID
	Slug, Name         string
	PlanCode           string
	SubscriptionStatus string
	SuspendedAt        *time.Time
	CreatedAt          time.Time
	Outlets, Devices   int
	LastSyncAt         *time.Time
	Sales7d, Events7d  int
}

// TenantDetail is one business for the console. Reading it is not audited: it holds counts, plan
// settings and device metadata, not the business's sales or people (the support report, which
// does, is).
type TenantDetail struct {
	Tenant       AdminTenant
	Entitlements []entitlements.AdminEntitlement
	Devices      []SupportDevice
	Metrics      []DailyMetrics // the last 30 days computed
}

// ListTenants pages through businesses by id. q, if set, matches the name or slug.
func (s *Service) ListTenants(ctx context.Context, page kernel.Page, q string) (kernel.Paged[AdminTenant], error) {
	rows, err := db.New(s.Pool).AdminTenants(ctx, db.AdminTenantsParams{
		After: page.After, Q: strings.TrimSpace(q), SinceDay: s.since7d(), PageSize: page.Fetch(),
	})
	if err != nil {
		return kernel.Paged[AdminTenant]{}, err
	}
	paged := kernel.Trim(page, rows, func(r db.AdminTenantsRow) uuid.UUID { return r.ID })
	out := kernel.Paged[AdminTenant]{Next: paged.Next, Items: make([]AdminTenant, len(paged.Items))}
	for i, r := range paged.Items {
		out.Items[i] = toAdminTenant(r)
	}
	return out, nil
}

// TenantDetail returns one business with its entitlements, devices and last 30 days of metrics.
func (s *Service) TenantDetail(ctx context.Context, tenant string) (TenantDetail, error) {
	var out TenantDetail
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenant)
		if err != nil {
			return err
		}
		q := db.New(tx)
		rows, err := q.AdminTenants(ctx, db.AdminTenantsParams{ID: &t.ID, SinceDay: s.since7d(), PageSize: 1})
		if err != nil || len(rows) == 0 {
			return fmt.Errorf("tenant %s vanished: %w", t.ID, err)
		}
		out.Tenant = toAdminTenant(rows[0])
		if out.Entitlements, err = s.Entitlements.TenantViewTx(ctx, tx, t.ID, s.Clock.Now()); err != nil {
			return err
		}
		devs, err := q.SupportDevices(ctx, t.ID)
		if err != nil {
			return err
		}
		out.Devices = make([]SupportDevice, len(devs))
		for i, d := range devs {
			out.Devices[i] = SupportDevice{
				ID: d.ID, Name: d.Name, OutletCode: d.OutletCode, Code: int(d.DeviceCode), Revoked: d.RevokedAt != nil, LastSeenAt: d.LastSeenAt,
				LastSyncAt: d.LastSyncAt, AppVersion: d.AppVersion, ClockSkewMs: d.ClockSkewMs, UnsyncedEvents: d.UnsyncedEvents,
				OldestUnsyncedAt: d.OldestUnsyncedAt, OpenAlerts: d.OpenAlerts,
			}
		}
		out.Metrics, err = s.metrics(ctx, q, t.ID, 30)
		return err
	})
	return out, err
}

// SetTenantPlan moves a business to another plan, for example a design partner to early_access.
func (s *Service) SetTenantPlan(ctx context.Context, a Actor, tenant, planCode, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenant)
		if err != nil {
			return err
		}
		before, err := s.Tenants.PlanCodeTx(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if before == planCode {
			return fmt.Errorf("%w: %s is already on %s", kernel.ErrConflict, t.Slug, planCode)
		}
		if err := s.Tenants.SetPlanTx(ctx, tx, t.ID, planCode); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "tenant.plan_changed", TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID,
			Before: map[string]any{"plan": before}, After: map[string]any{"plan": planCode}, Reason: reason,
		})
	})
}

// RevokeDevice revokes a business's device for it, for example a stolen tablet the owner cannot
// reach. It is written to the platform audit log and, as done by Orion, to the business's own.
func (s *Service) RevokeDevice(ctx context.Context, a Actor, deviceID uuid.UUID, reason string) error {
	if a.OperatorID == nil {
		return fmt.Errorf("%w: revoking a device needs an operator", kernel.ErrValidation)
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		d, err := q.GetDeviceForOperator(ctx, deviceID)
		if err != nil {
			return fmt.Errorf("%w: no device %s", kernel.ErrNotFound, deviceID)
		}
		if d.RevokedAt != nil {
			return fmt.Errorf("%w: device %s is already revoked", kernel.ErrConflict, deviceID)
		}
		if err := q.RevokeDeviceByOperator(ctx, db.RevokeDeviceByOperatorParams{ID: deviceID, Now: ptr(s.Clock.Now()), OperatorID: a.OperatorID}); err != nil {
			return err
		}
		detail := fmt.Sprintf(`{"outlet_id": %q, "device_code": %d, "name": %q, "by": "orion_support"}`, d.OutletID, d.DeviceCode, d.Name)
		if err := q.InsertTenantAuditAsSystem(ctx, db.InsertTenantAuditAsSystemParams{
			ID: kernel.NewID(), TenantID: d.TenantID, Action: "device.revoked", TargetType: "device", TargetID: &deviceID, Detail: []byte(detail),
		}); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "device.revoked", TargetType: "device", TargetID: &deviceID, TenantID: &d.TenantID,
			Before: map[string]any{"revoked": false, "name": d.Name}, After: map[string]any{"revoked": true}, Reason: reason,
		})
	})
}

// SetEntitlement sets an override for a business, or clears it when value is nil.
func (s *Service) SetEntitlement(ctx context.Context, a Actor, tenant string, key entitlements.Key, value *int64, expires *time.Time, reason string) error {
	if value == nil {
		return s.ClearOverride(ctx, a, tenant, key, reason)
	}
	return s.SetOverride(ctx, a, tenant, key, *value, expires, reason)
}

// EntitlementKeys lists every key with its default and plan values.
func (s *Service) EntitlementKeys(ctx context.Context) ([]entitlements.KeyInfo, error) {
	return s.Entitlements.Keys(ctx)
}

// SetFlagDefault changes a flag's default for every business without a plan value or an override.
func (s *Service) SetFlagDefault(ctx context.Context, a Actor, key entitlements.Key, value int64, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		before, err := s.Entitlements.SetFlagDefaultTx(ctx, tx, key, value)
		if err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "entitlement.default_changed", TargetType: "entitlement_key",
			Before: map[string]any{"key": key, "default": before}, After: map[string]any{"key": key, "default": value}, Reason: reason,
		})
	})
}

// since7d is the first day of the last week of metrics, today included.
func (s *Service) since7d() pgtype.Date {
	return pgDate(dateIn(s.Clock.Now(), MetricsZone).AddDate(0, 0, -6))
}

func toAdminTenant(r db.AdminTenantsRow) AdminTenant {
	return AdminTenant{
		ID: r.ID, Slug: r.Slug, Name: r.Name, PlanCode: r.PlanCode, SubscriptionStatus: r.SubscriptionStatus,
		SuspendedAt: r.SuspendedAt, CreatedAt: r.CreatedAt, Outlets: int(r.Outlets), Devices: int(r.Devices),
		LastSyncAt: r.LastSyncAt, Sales7d: int(r.Sales7d), Events7d: int(r.Events7d),
	}
}

func ptr[T any](v T) *T { return &v }
