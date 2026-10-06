package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
)

// errAdminDisabled is returned by operator routes when the server has no platform database.
var errAdminDisabled = errors.New("api: operator console is not enabled")

func (s *Server) platformOrErr() (*platform.Service, error) {
	if s.Platform == nil {
		return nil, errAdminDisabled
	}
	return s.Platform, nil
}

func (s *Server) AdminLogin(ctx context.Context, req openapi.AdminLoginRequestObject) (openapi.AdminLoginResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil || b.Email == "" || b.Password == "" {
		return nil, fmt.Errorf("%w: email and password are required", kernel.ErrValidation)
	}
	if err := allow(s.adminByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	if err := allow(s.adminByAccount, strings.ToLower(strings.TrimSpace(b.Email))); err != nil {
		return nil, err
	}
	ch, err := p.Login(ctx, b.Email, b.Password)
	if err != nil {
		return nil, err
	}
	return openapi.AdminLogin200JSONResponse{ChallengeToken: ch.Token, ChallengeExpiresAt: ch.ExpiresAt, TotpEnrolled: ch.Enrolled}, nil
}

func (s *Server) AdminVerifyTotp(ctx context.Context, req openapi.AdminVerifyTotpRequestObject) (openapi.AdminVerifyTotpResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil || b.ChallengeToken == "" || (deref(b.Code) == "" && deref(b.RecoveryCode) == "") {
		return nil, fmt.Errorf("%w: challenge_token and a code or recovery_code are required", kernel.ErrValidation)
	}
	if err := allow(s.adminByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	// Each challenge allows a handful of guesses, whatever the address.
	sum := sha256.Sum256([]byte(b.ChallengeToken))
	if err := allow(s.adminByChallenge, hex.EncodeToString(sum[:8])); err != nil {
		return nil, err
	}
	sess, err := p.VerifySecondFactor(ctx, b.ChallengeToken, deref(b.Code), deref(b.RecoveryCode),
		platform.Meta{IP: httpserver.ClientIP(ctx), UserAgent: httpserver.UserAgent(ctx)})
	if err != nil {
		return nil, err
	}
	return openapi.AdminVerifyTotp200JSONResponse{
		TokenType: openapi.OperatorSessionTokenTypeBearer, AccessToken: sess.AccessToken,
		AccessExpiresAt: sess.ExpiresAt, OperatorId: sess.OperatorID,
	}, nil
}

func (s *Server) AdminListAuditLog(ctx context.Context, req openapi.AdminListAuditLogRequestObject) (openapi.AdminListAuditLogResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := p.ListAuditLog(ctx, page, req.Params.TenantId)
	if err != nil {
		return nil, err
	}
	out := openapi.AdminListAuditLog200JSONResponse{Items: make([]openapi.AuditEntry, len(res.Items))}
	for i, e := range res.Items {
		ip := e.IP
		out.Items[i] = openapi.AuditEntry{
			Id: e.ID, OperatorId: e.OperatorID, Action: e.Action, TargetType: e.TargetType, TargetId: e.TargetID,
			TenantId: e.TenantID, Before: rawOrNil(e.Before), After: rawOrNil(e.After), Reason: e.Reason,
			Ip: &ip, UserAgent: e.UserAgent, CreatedAt: e.CreatedAt,
		}
	}
	if res.Next != uuid.Nil {
		next := res.Next
		out.NextCursor = &next
	}
	return out, nil
}

func rawOrNil(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// operatorActor is the signed-in operator, for the audit log. authenticate puts the id there for
// every operator route.
func operatorActor(ctx context.Context) platform.Actor {
	id, ok := ctx.Value(operatorKey{}).(uuid.UUID)
	if !ok {
		panic("api: operator handler ran without an operator")
	}
	return platform.Actor{OperatorID: &id, Meta: platform.Meta{IP: httpserver.ClientIP(ctx), UserAgent: httpserver.UserAgent(ctx)}}
}

func (s *Server) AdminListTenants(ctx context.Context, req openapi.AdminListTenantsRequestObject) (openapi.AdminListTenantsResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := p.ListTenants(ctx, page, deref(req.Params.Q))
	if err != nil {
		return nil, err
	}
	out := openapi.AdminListTenants200JSONResponse{Items: make([]openapi.AdminTenant, len(res.Items)), NextCursor: nextCursor(res.Next)}
	for i, t := range res.Items {
		out.Items[i] = toAdminTenant(t)
	}
	return out, nil
}

func (s *Server) AdminGetTenant(ctx context.Context, req openapi.AdminGetTenantRequestObject) (openapi.AdminGetTenantResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	d, err := p.TenantDetail(ctx, req.TenantId.String())
	if err != nil {
		return nil, err
	}
	out := openapi.AdminGetTenant200JSONResponse{
		Tenant:       toAdminTenant(d.Tenant),
		Entitlements: make([]openapi.AdminEntitlement, len(d.Entitlements)),
		Devices:      make([]openapi.AdminDevice, len(d.Devices)),
		Metrics:      make([]openapi.AdminDailyMetrics, len(d.Metrics)),
	}
	for i, e := range d.Entitlements {
		v := openapi.AdminEntitlement{
			Key: string(e.Key), Kind: openapi.AdminEntitlementKind(e.Kind), Category: openapi.AdminEntitlementCategory(e.Category),
			Value: e.Value, Source: openapi.AdminEntitlementSource(e.Source), Default: e.Default, PlanValue: e.PlanValue,
		}
		if e.OverrideValue != nil {
			v.Override = &struct {
				ExpiresAt *time.Time `json:"expires_at,omitempty"`
				InForce   bool       `json:"in_force"`
				Reason    string     `json:"reason"`
				Value     int64      `json:"value"`
			}{ExpiresAt: e.OverrideExpires, InForce: e.OverrideInForce, Reason: deref(e.OverrideReason), Value: *e.OverrideValue}
		}
		out.Entitlements[i] = v
	}
	for i, dv := range d.Devices {
		var skew *int
		if dv.ClockSkewMs != nil {
			v := int(*dv.ClockSkewMs)
			skew = &v
		}
		out.Devices[i] = openapi.AdminDevice{
			Id: dv.ID, Name: dv.Name, OutletCode: dv.OutletCode, Code: dv.Code, Revoked: dv.Revoked, LastSeenAt: dv.LastSeenAt,
			LastSyncAt: dv.LastSyncAt, AppVersion: dv.AppVersion, ClockSkewMs: skew, UnsyncedEvents: int(dv.UnsyncedEvents),
			OldestUnsyncedAt: dv.OldestUnsyncedAt, OpenAlerts: dv.OpenAlerts,
		}
	}
	for i, m := range d.Metrics {
		out.Metrics[i] = openapi.AdminDailyMetrics{
			Day: openapi_types.Date{Time: m.Day}, Sales: m.Sales, VoidedSales: m.VoidedSales, Events: m.Events,
			RejectedEvents: m.RejectedEvents, DevicesSynced: m.DevicesSynced, Flags: m.Flags,
		}
	}
	return out, nil
}

func (s *Server) AdminSuspendTenant(ctx context.Context, req openapi.AdminSuspendTenantRequestObject) (openapi.AdminSuspendTenantResponseObject, error) {
	if err := s.setSuspended(ctx, req.TenantId, req.Body, true); err != nil {
		return nil, err
	}
	return openapi.AdminSuspendTenant204Response{}, nil
}

func (s *Server) AdminReinstateTenant(ctx context.Context, req openapi.AdminReinstateTenantRequestObject) (openapi.AdminReinstateTenantResponseObject, error) {
	if err := s.setSuspended(ctx, req.TenantId, req.Body, false); err != nil {
		return nil, err
	}
	return openapi.AdminReinstateTenant204Response{}, nil
}

func (s *Server) setSuspended(ctx context.Context, tenantID uuid.UUID, body *openapi.OperatorReason, suspended bool) error {
	p, err := s.platformOrErr()
	if err != nil {
		return err
	}
	if body == nil {
		return errBodyRequired
	}
	return p.SetTenantSuspended(ctx, operatorActor(ctx), tenantID.String(), suspended, body.Reason)
}

func (s *Server) AdminSetTenantPlan(ctx context.Context, req openapi.AdminSetTenantPlanRequestObject) (openapi.AdminSetTenantPlanResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBodyRequired
	}
	if err := p.SetTenantPlan(ctx, operatorActor(ctx), req.TenantId.String(), req.Body.Plan, req.Body.Reason); err != nil {
		return nil, err
	}
	s.Entitlements.Invalidate(req.TenantId)
	return openapi.AdminSetTenantPlan204Response{}, nil
}

func (s *Server) AdminSetTenantEntitlement(ctx context.Context, req openapi.AdminSetTenantEntitlementRequestObject) (openapi.AdminSetTenantEntitlementResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	if err := p.SetEntitlement(ctx, operatorActor(ctx), req.TenantId.String(), entitlements.Key(req.Key), b.Value, b.ExpiresAt, b.Reason); err != nil {
		return nil, err
	}
	s.Entitlements.Invalidate(req.TenantId)
	return openapi.AdminSetTenantEntitlement204Response{}, nil
}

func (s *Server) AdminRevokeDevice(ctx context.Context, req openapi.AdminRevokeDeviceRequestObject) (openapi.AdminRevokeDeviceResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBodyRequired
	}
	if err := p.RevokeDevice(ctx, operatorActor(ctx), req.DeviceId, req.Body.Reason); err != nil {
		return nil, err
	}
	return openapi.AdminRevokeDevice204Response{}, nil
}

func (s *Server) AdminStoppedSyncing(ctx context.Context, req openapi.AdminStoppedSyncingRequestObject) (openapi.AdminStoppedSyncingResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	quiet := 24 * time.Hour
	if req.Params.QuietMinutes != nil {
		quiet = time.Duration(*req.Params.QuietMinutes) * time.Minute
	}
	ds, err := p.StoppedSyncing(ctx, quiet)
	if err != nil {
		return nil, err
	}
	out := openapi.AdminStoppedSyncing200JSONResponse{Items: make([]openapi.QuietDevice, len(ds))}
	for i, d := range ds {
		out.Items[i] = openapi.QuietDevice{
			TenantSlug: d.TenantSlug, TenantName: d.TenantName, OutletCode: d.OutletCode, DeviceId: d.DeviceID, Name: d.Name,
			Code: d.Code, PairedAt: d.PairedAt, LastSyncAt: d.LastSyncAt, LastSeenAt: d.LastSeenAt, AppVersion: d.AppVersion,
			UnsyncedEvents: d.UnsyncedEvents,
		}
	}
	return out, nil
}

func (s *Server) AdminListEntitlementKeys(ctx context.Context, _ openapi.AdminListEntitlementKeysRequestObject) (openapi.AdminListEntitlementKeysResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	keys, err := p.EntitlementKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := openapi.AdminListEntitlementKeys200JSONResponse{Items: make([]openapi.EntitlementKeyInfo, len(keys))}
	for i, k := range keys {
		out.Items[i] = openapi.EntitlementKeyInfo{
			Key: k.Key, Kind: openapi.EntitlementKeyInfoKind(k.Kind), Category: openapi.EntitlementKeyInfoCategory(k.Category),
			Description: k.Description, Owner: k.Owner, Temporary: k.Temporary, Default: k.Default, Plans: k.Plans,
		}
	}
	return out, nil
}

func (s *Server) AdminSetFlagDefault(ctx context.Context, req openapi.AdminSetFlagDefaultRequestObject) (openapi.AdminSetFlagDefaultResponseObject, error) {
	p, err := s.platformOrErr()
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errBodyRequired
	}
	if err := p.SetFlagDefault(ctx, operatorActor(ctx), entitlements.Key(req.Key), req.Body.DefaultValue, req.Body.Reason); err != nil {
		return nil, err
	}
	s.Entitlements.InvalidateAll()
	return openapi.AdminSetFlagDefault204Response{}, nil
}

func toAdminTenant(t platform.AdminTenant) openapi.AdminTenant {
	return openapi.AdminTenant{
		Id: t.ID, Slug: t.Slug, Name: t.Name, Plan: t.PlanCode, SubscriptionStatus: t.SubscriptionStatus, SuspendedAt: t.SuspendedAt,
		CreatedAt: t.CreatedAt, Outlets: t.Outlets, Devices: t.Devices, LastSyncAt: t.LastSyncAt, Sales7d: t.Sales7d, Events7d: t.Events7d,
	}
}
