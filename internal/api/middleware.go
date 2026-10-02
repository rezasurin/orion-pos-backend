package api

import (
	"context"
	"log/slog"
	"net/http"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// authenticate is the strict-server middleware that enforces each operation's policy before its
// handler runs: a valid token of the right audience, a caller who is still allowed in their
// tenant, a tenant that is not suspended, and the permission named by x-permission.
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
		p, err := s.Identity.Authenticate(pol.audience, bearer)
		if err != nil {
			return nil, err
		}
		// From here on every database call runs as this tenant.
		ctx = identity.WithPrincipal(kernel.WithTenant(ctx, p.TenantID), p)
		httpserver.AddLogAttrs(ctx,
			slog.String("tenant_id", p.TenantID.String()),
			slog.String("principal_type", string(p.Type)),
			slog.String("principal_id", p.ID().String()),
		)

		if err := s.Tenancy.CheckActive(ctx, p.TenantID); err != nil {
			return nil, err
		}
		switch p.Type {
		case identity.PrincipalUser:
			access, err := s.Identity.LoadAccess(ctx, p)
			if err != nil {
				return nil, err
			}
			if pol.permission != "" && !access.IsOwner {
				return nil, &identity.ForbiddenError{Permission: pol.permission}
			}
			ctx = identity.WithAccess(ctx, access)
		default:
			return nil, identity.ErrInvalidToken
		}
		return next(ctx, w, r, req)
	}
}

func principalFrom(ctx context.Context) identity.Principal {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		// authenticate puts it there for every non-public operation.
		panic("api: handler ran without an authenticated principal")
	}
	return p
}
