package api

import (
	"context"
	"fmt"

	openapi_types "github.com/oapi-codegen/runtime/types"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
)

func (s *Server) PushEvents(ctx context.Context, req openapi.PushEventsRequestObject) (openapi.PushEventsResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	p := principalFrom(ctx)
	if b.DeviceId != p.DeviceID {
		return nil, fmt.Errorf("%w: device_id is not the device this token belongs to", kernel.ErrValidation)
	}
	in := sync.PushRequest{
		Events: make([]sync.Event, len(b.Events)),
		Health: sync.DeviceHealth{ClientTime: b.ClientTime, AppVersion: b.AppVersion, Unsynced: b.UnsyncedEvents, OldestUnsynced: b.OldestUnsyncedAt},
	}
	for i, e := range b.Events {
		in.Events[i] = sync.Event{
			ID: e.Id, IdempotencyKey: e.IdempotencyKey, Type: e.Type, StaffID: e.StaffId,
			DeviceTime: e.DeviceTime, SchemaVersion: e.SchemaVersion, Payload: e.Payload,
		}
	}
	res, err := s.Sync.Push(ctx, p, in)
	if err != nil {
		return nil, err
	}
	out := openapi.PushEvents200JSONResponse{Results: make([]openapi.SyncResult, len(res.Results)), ServerTime: res.ServerTime}
	for i, r := range res.Results {
		out.Results[i] = openapi.SyncResult{Id: r.ID, Status: openapi.SyncResultStatus(r.Status)}
		if r.Code != "" {
			code := r.Code
			out.Results[i].Code = &code
		}
		if r.Detail != "" {
			detail := r.Detail
			out.Results[i].Detail = &detail
		}
	}
	return out, nil
}

func (s *Server) PullChanges(ctx context.Context, req openapi.PullChangesRequestObject) (openapi.PullChangesResponseObject, error) {
	in := sync.PullRequest{Health: sync.DeviceHealth{
		ClientTime: req.Params.ClientTime, AppVersion: req.Params.AppVersion,
		Unsynced: req.Params.UnsyncedEvents, OldestUnsynced: req.Params.OldestUnsyncedAt,
	}}
	if req.Params.Cursor != nil {
		in.Cursor = *req.Params.Cursor
	}
	if req.Params.Limit != nil {
		if *req.Params.Limit < 1 || *req.Params.Limit > sync.MaxPullLimit {
			return nil, fmt.Errorf("%w: limit must be between 1 and %d", kernel.ErrValidation, sync.MaxPullLimit)
		}
		in.Limit = *req.Params.Limit
	}
	res, err := s.Sync.Pull(ctx, principalFrom(ctx), in)
	if err != nil {
		return nil, err
	}

	out := openapi.PullChanges200JSONResponse{
		Cursor: res.Cursor, Snapshot: res.Snapshot, HasMore: res.HasMore, ServerTime: res.ServerTime, Suspended: suspendedFrom(ctx),
		Categories:      make([]openapi.Category, len(res.Catalog.Categories)),
		KitchenStations: make([]openapi.KitchenStation, len(res.Catalog.Stations)),
		Items:           make([]openapi.Item, len(res.Catalog.Items)),
		ModifierGroups:  make([]openapi.ModifierGroup, len(res.Catalog.Groups)),
		OutletVariants:  make([]openapi.OutletVariant, len(res.Catalog.OutletVariants)),
		Staff:           make([]openapi.RosterStaff, len(res.Staff)),
		RemovedStaffIds: res.RemovedStaff,
		Deleted: make([]struct {
			Id   openapi_types.UUID `json:"id"`
			Type string             `json:"type"`
		}, len(res.Deleted)),
	}
	if res.Outlet != nil {
		o := toOutlet(*res.Outlet)
		out.Outlet = &o
	}
	for i, c := range res.Catalog.Categories {
		out.Categories[i] = toCategoryBody(c)
	}
	for i, st := range res.Catalog.Stations {
		out.KitchenStations[i] = toStationBody(st)
	}
	for i, it := range res.Catalog.Items {
		out.Items[i] = toItemBody(it)
	}
	for i, g := range res.Catalog.Groups {
		out.ModifierGroups[i] = toGroupBody(g)
	}
	for i, v := range res.Catalog.OutletVariants {
		out.OutletVariants[i] = toOutletVariantBody(v)
	}
	for i, st := range res.Staff {
		perms := make([]string, len(st.Permissions))
		for j, p := range st.Permissions {
			perms[j] = string(p)
		}
		out.Staff[i] = openapi.RosterStaff{Id: st.StaffID, DisplayName: st.DisplayName, PinHash: st.PINHash, Permissions: perms}
	}
	for i, d := range res.Deleted {
		out.Deleted[i].Id, out.Deleted[i].Type = d.ID, d.Type
	}
	out.Entitlements.GeneratedAt = res.Entitlements.GeneratedAt
	out.Entitlements.ExpiresAt = res.EntitlementsExpiresAt
	out.Entitlements.Items = make([]openapi.Entitlement, len(res.Entitlements.Items))
	for i, e := range res.Entitlements.Items {
		out.Entitlements.Items[i] = openapi.Entitlement{
			Key: string(e.Key), Kind: openapi.EntitlementKind(e.Kind), Category: openapi.EntitlementCategory(e.Category),
			Value: e.Value, Source: openapi.EntitlementSource(e.Source),
		}
	}
	return out, nil
}
