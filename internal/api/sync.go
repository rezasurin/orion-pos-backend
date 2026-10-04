package api

import (
	"context"
	"fmt"

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
	in := sync.PushRequest{Events: make([]sync.Event, len(b.Events)), ClientTime: b.ClientTime, AppVersion: b.AppVersion}
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
