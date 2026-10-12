package api

import (
	"context"
	"log/slog"
	"net/http"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// authenticate is the strict-server middleware that enforces each operation's policy before its
// handler runs: a valid token of the right audience, a caller who is still allowed in their
// tenant, a tenant that is not suspended (for a person's write), the permission named by
// x-permission, and the module named by x-module.
//
// It runs inside the generated wrapper, so it knows the operation id. A route missing from the
// policy table is a programming error and fails closed.
func (s *Server) authenticate(next openapi.StrictHandlerFunc, operationID string) openapi.StrictHandlerFunc {
	pol, known := s.policies[operationID]
	if !known {
		panic("api: no access policy for operation " + operationID)
	}
	if pol.public {
		return next
	}
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, req any) (any, error) {
		bearer, ok := bearerToken(r)
		if !ok {
			return nil, identity.ErrInvalidToken
		}
		if pol.operator {
			plat, err := s.platformOrErr()
			if err != nil {
				return nil, err
			}
			op, err := plat.Authenticate(ctx, bearer)
			if err != nil {
				return nil, err
			}
			httpserver.AddLogAttrs(ctx, slog.String("principal_type", "operator"), slog.String("principal_id", op.ID.String()))
			return next(context.WithValue(ctx, operatorKey{}, op.ID), w, r, req)
		}
		p, err := s.Identity.Authenticate(pol.audience, bearer)
		if err != nil {
			return nil, err
		}
		// From here on every database call runs as this tenant.
		ctx = identity.WithPrincipal(kernel.WithTenant(ctx, p.TenantID), p)
		ctx = kernel.WithActor(ctx, kernel.Actor{Type: string(p.Type), ID: p.ID(), IP: httpserver.ClientIP(ctx)})
		httpserver.AddLogAttrs(ctx,
			slog.String("tenant_id", p.TenantID.String()),
			slog.String("principal_type", string(p.Type)),
			slog.String("principal_id", p.ID().String()),
		)

		// A suspended business is read-only for its people. Its tablets keep syncing, so a shift
		// in progress is never cut off and no sale is lost; the pull tells them to stop after it.
		suspendedAt, err := s.Tenancy.SuspendedAt(ctx, p.TenantID)
		if err != nil {
			return nil, err
		}
		if suspendedAt != nil && p.Type == identity.PrincipalUser && r.Method != http.MethodGet {
			return nil, tenancy.ErrSuspended
		}
		ctx = context.WithValue(ctx, suspendedKey{}, suspendedAt != nil)
		switch p.Type {
		case identity.PrincipalUser:
			access, err := s.Identity.LoadAccess(ctx, p)
			if err != nil {
				return nil, err
			}
			if pol.permission != "" && !access.Has(identity.Permission(pol.permission)) {
				return nil, &identity.ForbiddenError{Permission: pol.permission}
			}
			ctx = identity.WithAccess(ctx, access)
		case identity.PrincipalDevice:
			// A revoked device is cut off here, not when its access token expires.
			if err := s.Identity.CheckDevice(ctx, p); err != nil {
				return nil, err
			}
		default:
			return nil, identity.ErrInvalidToken
		}
		if pol.module != "" {
			if err := s.Entitlements.RequireModule(ctx, p.TenantID, pol.module); err != nil {
				return nil, err
			}
		}
		return next(ctx, w, r, req)
	}
}

type (
	suspendedKey struct{}
	operatorKey  struct{}
)

// suspendedFrom says whether the caller's business is suspended, as authenticate found it.
func suspendedFrom(ctx context.Context) bool {
	v, _ := ctx.Value(suspendedKey{}).(bool)
	return v
}

func principalFrom(ctx context.Context) identity.Principal {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		// authenticate puts it there for every non-public operation.
		panic("api: handler ran without an authenticated principal")
	}
	return p
}
