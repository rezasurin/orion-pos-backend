package api

import (
	"context"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
)

func (s *Server) GetEntitlements(ctx context.Context, _ openapi.GetEntitlementsRequestObject) (openapi.GetEntitlementsResponseObject, error) {
	snap, err := s.Entitlements.Snapshot(ctx, principalFrom(ctx).TenantID)
	if err != nil {
		return nil, err
	}
	out := openapi.GetEntitlements200JSONResponse{GeneratedAt: snap.GeneratedAt, Items: make([]openapi.Entitlement, len(snap.Items))}
	for i, e := range snap.Items {
		out.Items[i] = openapi.Entitlement{
			Key: string(e.Key), Kind: openapi.EntitlementKind(e.Kind), Category: openapi.EntitlementCategory(e.Category),
			Value: e.Value, Source: openapi.EntitlementSource(e.Source),
		}
	}
	return out, nil
}
