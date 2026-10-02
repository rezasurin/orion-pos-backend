package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// rateLimitError is returned by handlers when a limiter refuses a request.
type rateLimitError struct{ RetryAfter time.Duration }

func (e *rateLimitError) Error() string { return "rate limit exceeded" }

// requestError handles a request the generated code could not decode: bad JSON, a bad path
// parameter, a body over the size limit.
func (s *Server) requestError(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		httpserver.WriteProblem(w, r, httpserver.Problem{
			Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Detail: "request body is too large",
		})
		return
	}
	httpserver.WriteProblem(w, r, httpserver.Problem{
		Status: http.StatusBadRequest, Code: "validation_failed", Detail: "the request could not be read: " + err.Error(),
	})
}

// responseError turns an error returned by a handler or middleware into a problem+json response.
// This table is the only place domain errors become HTTP statuses and codes.
func (s *Server) responseError(w http.ResponseWriter, r *http.Request, err error) {
	p := httpserver.Problem{}

	var (
		tenantRequired *identity.TenantRequiredError
		forbidden      *identity.ForbiddenError
		limited        *rateLimitError
	)
	switch {
	case errors.As(err, &limited):
		p.Status, p.Code = http.StatusTooManyRequests, "rate_limited"
		p.Detail = "too many requests; try again later"
		w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
	case errors.Is(err, kernel.ErrValidation):
		p.Status, p.Code = http.StatusBadRequest, "validation_failed"
		p.Detail = strings.TrimPrefix(err.Error(), kernel.ErrValidation.Error()+": ")
	case errors.Is(err, identity.ErrInvalidCredentials):
		p.Status, p.Code, p.Detail = http.StatusUnauthorized, "invalid_credentials", "email or password is wrong"
	case errors.Is(err, identity.ErrTokenReuse):
		p.Status, p.Code, p.Detail = http.StatusUnauthorized, "token_reused", "this refresh token was already used; sign in again"
	case errors.Is(err, identity.ErrInvalidToken):
		p.Status, p.Code, p.Detail = http.StatusUnauthorized, "invalid_token", "the token is invalid, expired or already used"
	case errors.Is(err, identity.ErrEmailNotVerified):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "email_not_verified", "verify your email address first"
	case errors.Is(err, identity.ErrNoTenant):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "no_tenant", "this account does not belong to a business"
	case errors.Is(err, tenancy.ErrSuspended):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "tenant_suspended", "this business is suspended"
	case errors.Is(err, identity.ErrDeviceRevoked):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "device_revoked", "this device has been revoked"
	case errors.As(err, &tenantRequired):
		p.Status, p.Code, p.Detail = http.StatusConflict, "tenant_required", "choose which business to sign in to"
		p.TenantIDs = tenantRequired.TenantIDs
	case errors.As(err, &forbidden):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "forbidden", "you do not have permission: "+forbidden.Permission
	case errors.Is(err, identity.ErrForbidden):
		p.Status, p.Code, p.Detail = http.StatusForbidden, "forbidden", "you do not have permission"
	case errors.Is(err, kernel.ErrNotFound):
		p.Status, p.Code, p.Detail = http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, kernel.ErrConflict):
		p.Status, p.Code = http.StatusConflict, "conflict"
		p.Detail = strings.TrimPrefix(err.Error(), kernel.ErrConflict.Error()+": ")
	default:
		s.logInternal(r.Context(), r, err)
		p.Status, p.Code = http.StatusInternalServerError, "internal"
	}
	if p.Status == http.StatusUnauthorized {
		w.Header().Add("WWW-Authenticate", `Bearer`)
	}
	httpserver.WriteProblem(w, r, p)
}

// logInternal records an unexpected error and, when Sentry is on, reports it. The response
// carries the request id and nothing else, so internals do not leak.
func (s *Server) logInternal(ctx context.Context, r *http.Request, err error) {
	if ctx.Err() != nil {
		return // the client went away; not our failure
	}
	s.Logger.ErrorContext(ctx, "request failed",
		slog.String("request_id", middleware.GetReqID(ctx)),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Any("error", err),
	)
	if hub := sentry.GetHubFromContext(ctx); hub != nil {
		hub.CaptureException(err)
	}
}
