package api

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

func (s *Server) PairDevice(ctx context.Context, req openapi.PairDeviceRequestObject) (openapi.PairDeviceResponseObject, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("%w: a body is required", kernel.ErrValidation)
	}
	c := callerFrom(ctx)
	if err := allow(s.pairByUser, c.Principal.UserID.String()); err != nil {
		return nil, err
	}
	paired, err := s.Identity.PairDevice(ctx, c, identity.NewDevice{OutletID: req.Body.OutletId, Name: req.Body.Name})
	if err != nil {
		return nil, err
	}
	return openapi.PairDevice201JSONResponse{Device: toDevice(paired.Device), DeviceSecret: paired.Secret}, nil
}

func (s *Server) ExchangeDeviceToken(ctx context.Context, req openapi.ExchangeDeviceTokenRequestObject) (openapi.ExchangeDeviceTokenResponseObject, error) {
	b := req.Body
	if b == nil || b.DeviceSecret == "" {
		return nil, fmt.Errorf("%w: device_secret is required", kernel.ErrValidation)
	}
	if err := allow(s.tokenByIP, httpserver.ClientIP(ctx)); err != nil {
		return nil, err
	}
	in := identity.DeviceExchange{Secret: b.DeviceSecret, ClientTime: b.ClientTime}
	if b.AppVersion != nil {
		in.AppVersion = *b.AppVersion
	}
	sess, err := s.Identity.ExchangeDeviceSecret(ctx, in)
	if err != nil {
		return nil, err
	}
	return openapi.ExchangeDeviceToken200JSONResponse{
		TokenType:       openapi.DeviceSessionTokenTypeBearer,
		AccessToken:     sess.AccessToken,
		AccessExpiresAt: sess.AccessExpiresAt,
		DeviceId:        sess.DeviceID,
		OutletId:        sess.OutletID,
		TenantId:        sess.TenantID,
	}, nil
}

func (s *Server) ListDevices(ctx context.Context, req openapi.ListDevicesRequestObject) (openapi.ListDevicesResponseObject, error) {
	page, err := pageFrom(req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	res, err := s.Identity.ListDevices(ctx, callerFrom(ctx), page)
	if err != nil {
		return nil, err
	}
	out := openapi.ListDevices200JSONResponse{Items: make([]openapi.Device, len(res.Items))}
	for i, d := range res.Items {
		out.Items[i] = toDevice(d)
	}
	if res.Next != uuid.Nil {
		next := res.Next
		out.NextCursor = &next
	}
	return out, nil
}

func (s *Server) RevokeDevice(ctx context.Context, req openapi.RevokeDeviceRequestObject) (openapi.RevokeDeviceResponseObject, error) {
	if err := s.Identity.RevokeDevice(ctx, callerFrom(ctx), req.DeviceId); err != nil {
		return nil, err
	}
	return openapi.RevokeDevice204Response{}, nil
}

func (s *Server) GetRoster(ctx context.Context, _ openapi.GetRosterRequestObject) (openapi.GetRosterResponseObject, error) {
	p := principalFrom(ctx)
	roster, err := s.Identity.GetRoster(ctx, p)
	if err != nil {
		return nil, err
	}
	outlet, err := s.Tenancy.GetOutlet(ctx, p.TenantID, p.OutletID)
	if err != nil {
		return nil, err
	}
	out := openapi.GetRoster200JSONResponse{
		Device:      toDevice(roster.Device),
		Outlet:      toOutlet(outlet),
		GeneratedAt: time.Now().UTC(),
		Staff:       make([]openapi.RosterStaff, len(roster.Staff)),
	}
	for i, st := range roster.Staff {
		perms := make([]string, len(st.Permissions))
		for j, p := range st.Permissions {
			perms[j] = string(p)
		}
		out.Staff[i] = openapi.RosterStaff{Id: st.StaffID, DisplayName: st.DisplayName, PinHash: st.PINHash, Permissions: perms}
	}
	return out, nil
}

func toDevice(d identity.Device) openapi.Device {
	return openapi.Device{
		Id: d.ID, OutletId: d.OutletID, DeviceCode: d.Code, Name: d.Name, PairedBy: d.PairedBy, PairedAt: d.PairedAt,
		RevokedAt: d.RevokedAt, LastSeenAt: d.LastSeenAt, LastSyncAt: d.LastSyncAt, AppVersion: d.AppVersion, ClockSkewMs: d.ClockSkewMs,
		UnsyncedEvents: d.UnsyncedEvents, OldestUnsyncedAt: d.OldestUnsyncedAt, HealthReportedAt: d.HealthReportedAt,
	}
}
