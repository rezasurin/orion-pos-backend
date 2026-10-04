// Package api is the HTTP layer: it implements the operations of api/openapi.yaml by calling the
// modules, and it is the only place that knows both HTTP and the modules. Handlers stay thin:
// decode, call one module method, encode. Rules belong in the modules.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/reporting"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	"github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// Deps are the modules and settings the API needs.
type Deps struct {
	Catalog      *catalog.Service
	Identity     *identity.Service
	Entitlements *entitlements.Resolver
	// Platform serves the operator console. Nil when the server has no platform database; operator
	// routes then answer 503 admin_disabled.
	Platform  *platform.Service
	Sync      *sync.Service
	Reporting *reporting.Service
	Sales     *sales.Service
	Tenancy   *tenancy.Service
	Logger    *slog.Logger
}

// Server implements openapi.StrictServerInterface.
type Server struct {
	Deps
	policies map[string]policy

	// Rate limits (BACKEND_PLAN.md section 4.3). Sign-in is limited by caller address and by
	// account, so neither one address spraying many accounts nor many addresses hammering one
	// account gets through.
	loginByIP      *httpserver.Limiter
	loginByAccount *httpserver.Limiter
	tokenByIP      *httpserver.Limiter
	emailByIP      *httpserver.Limiter
	emailByAccount *httpserver.Limiter
	pinByUser      *httpserver.Limiter
	pairByUser     *httpserver.Limiter

	adminByIP        *httpserver.Limiter
	adminByAccount   *httpserver.Limiter
	adminByChallenge *httpserver.Limiter
}

var _ openapi.StrictServerInterface = (*Server)(nil)

// New reads the access rules from the embedded OpenAPI document and returns a Server.
func New(d Deps) (*Server, error) {
	doc, err := openapi.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("api: load spec: %w", err)
	}
	policies, err := policiesFromSpec(doc)
	if err != nil {
		return nil, fmt.Errorf("api: %w", err)
	}
	return &Server{
		Deps:           d,
		policies:       policies,
		loginByIP:      httpserver.NewLimiter(3*time.Second, 20),
		loginByAccount: httpserver.NewLimiter(time.Minute, 5),
		tokenByIP:      httpserver.NewLimiter(time.Second, 30),
		emailByIP:      httpserver.NewLimiter(10*time.Second, 5),
		emailByAccount: httpserver.NewLimiter(5*time.Minute, 3),
		pinByUser:      httpserver.NewLimiter(6*time.Second, 10),
		pairByUser:     httpserver.NewLimiter(12*time.Second, 5),

		adminByIP:        httpserver.NewLimiter(6*time.Second, 10),
		adminByAccount:   httpserver.NewLimiter(time.Minute, 5),
		adminByChallenge: httpserver.NewLimiter(time.Minute, 5),
	}, nil
}

// Routes mounts every operation of the contract on r.
func (s *Server) Routes(r chi.Router) {
	h := openapi.NewStrictHandlerWithOptions(s,
		[]openapi.StrictMiddlewareFunc{s.authenticate},
		openapi.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  s.requestError,
			ResponseErrorHandlerFunc: s.responseError,
		})
	openapi.HandlerWithOptions(h, openapi.ChiServerOptions{
		BaseRouter:       r,
		ErrorHandlerFunc: s.requestError,
	})
}

// allow takes a token from l for key, or returns a rate limit error.
func allow(l *httpserver.Limiter, key string) error {
	if ok, retry := l.Allow(key); !ok {
		return &rateLimitError{RetryAfter: retry}
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}
