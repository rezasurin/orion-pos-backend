package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/rezasurin/orion-pos-backend/internal/api"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/httpserver"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const password = "correct horse battery"

// env is the whole service wired the way cmd/orion wires it, behind an in-memory HTTP server.
type env struct {
	d       *testdb.DB
	handler http.Handler
	ids     *identity.Service
	tenants *tenancy.Service
}

type tenantFixture struct {
	tenant tenancy.Tenant
	outlet tenancy.Outlet
	owner  identity.User
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	tenants := tenancy.NewService(d.App)
	jobs, err := river.NewClient(riverpgxv5.New(d.App), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ents := entitlements.NewResolver(d.App, nil)
	ids, err := identity.NewService(identity.Deps{
		Entitlements: ents, Pool: d.App, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		Jobs: jobs, Gate: tenants.CheckActive,
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := api.New(api.Deps{Identity: ids, Entitlements: ents, Tenancy: tenants, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	handler := httpserver.NewRouter(httpserver.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Routes: func(r chi.Router) { srv.Routes(r) },
	})
	return &env{d: d, handler: handler, ids: ids, tenants: tenants}
}

// business creates a tenant with one outlet and a verified owner.
func (e *env) business(t *testing.T, slug, code, email string) tenantFixture {
	t.Helper()
	ctx := context.Background()
	tn, o, err := e.tenants.CreateTenant(ctx, tenancy.NewTenant{
		Name: strings.ToUpper(slug), Slug: slug, Outlet: tenancy.NewOutlet{Name: "Main " + slug, Code: code},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.ids.CreateMember(ctx, identity.NewMember{
		TenantID: tn.ID, Email: email, Password: password, DisplayName: "Owner", IsOwner: true, EmailVerified: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tenantFixture{tenant: tn, outlet: o, owner: u}
}

type response struct {
	*httptest.ResponseRecorder
}

func (e *env) do(t *testing.T, method, path, token string, body any) response {
	t.Helper()
	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return response{rec}
}

func (r response) decode(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), into); err != nil {
		t.Fatalf("response body %q: %v", r.Body.String(), err)
	}
}

// problem asserts the response is a problem+json document with the given status and code.
func (r response) problem(t *testing.T, status int, code string) map[string]any {
	t.Helper()
	if r.Code != status {
		t.Fatalf("status = %d, want %d; body %s", r.Code, status, r.Body.String())
	}
	if ct := r.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var p map[string]any
	r.decode(t, &p)
	if p["code"] != code {
		t.Errorf("code = %v, want %s; body %s", p["code"], code, r.Body.String())
	}
	if p["request_id"] == nil || p["status"] != float64(status) || p["title"] == "" {
		t.Errorf("problem is missing request_id, status or title: %v", p)
	}
	return p
}

type sessionBody struct {
	TokenType    string `json:"token_type"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TenantID     string `json:"tenant_id"`
	UserID       string `json:"user_id"`
}

func (e *env) login(t *testing.T, email string) sessionBody {
	t.Helper()
	r := e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": email, "password": password})
	if r.Code != http.StatusOK {
		t.Fatalf("login %s: %d %s", email, r.Code, r.Body.String())
	}
	var s sessionBody
	r.decode(t, &s)
	return s
}

func TestLoginAndMe(t *testing.T) {
	e := newEnv(t)
	b := e.business(t, "kopi", "JKT1", "owner@kopi.test")

	s := e.login(t, "owner@kopi.test")
	if s.TokenType != "Bearer" || s.AccessToken == "" || s.RefreshToken == "" || s.TenantID != b.tenant.ID.String() {
		t.Fatalf("session = %+v", s)
	}

	r := e.do(t, "GET", "/v1/me", s.AccessToken, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("GET /v1/me: %d %s", r.Code, r.Body.String())
	}
	var me struct {
		User struct {
			ID            string `json:"id"`
			Email         string `json:"email"`
			EmailVerified bool   `json:"email_verified"`
			Locale        string `json:"locale"`
		} `json:"user"`
		Tenant struct {
			ID                 string `json:"id"`
			Slug               string `json:"slug"`
			SubscriptionStatus string `json:"subscription_status"`
		} `json:"tenant"`
		IsOwner bool `json:"is_owner"`
	}
	r.decode(t, &me)
	if me.User.ID != b.owner.ID.String() || me.User.Email != "owner@kopi.test" || !me.User.EmailVerified || me.User.Locale != "id-ID" ||
		me.Tenant.ID != b.tenant.ID.String() || me.Tenant.Slug != "kopi" || me.Tenant.SubscriptionStatus != "early_access" || !me.IsOwner {
		t.Errorf("me = %+v", me)
	}
}

func TestAuthenticationIsRequired(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	s := e.login(t, "owner@kopi.test")

	for name, token := range map[string]string{
		"no token":      "",
		"garbage":       "not-a-token",
		"refresh token": s.RefreshToken,
	} {
		r := e.do(t, "GET", "/v1/me", token, nil)
		r.problem(t, http.StatusUnauthorized, "invalid_token")
		if r.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: missing WWW-Authenticate", name)
		}
	}

	req := httptest.NewRequest("GET", "/v1/me", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	response{rec}.problem(t, http.StatusUnauthorized, "invalid_token")

	// Every non-public route demands a token.
	for _, path := range []string{"/v1/me", "/v1/outlets", "/v1/outlets/" + uuid.NewString()} {
		e.do(t, "GET", path, "", nil).problem(t, http.StatusUnauthorized, "invalid_token")
	}
}

func TestLoginErrors(t *testing.T) {
	e := newEnv(t)
	b := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	if _, err := e.ids.CreateMember(context.Background(), identity.NewMember{
		TenantID: b.tenant.ID, Email: "new@kopi.test", Password: password, DisplayName: "New",
	}); err != nil {
		t.Fatal(err)
	}

	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": "wrong password"}).
		problem(t, http.StatusUnauthorized, "invalid_credentials")
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "nobody@kopi.test", "password": password}).
		problem(t, http.StatusUnauthorized, "invalid_credentials")
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "new@kopi.test", "password": password}).
		problem(t, http.StatusForbidden, "email_not_verified")
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "", "password": ""}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/auth/login", "", "{not json").
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/auth/login", "", `{"email":"a@b.test","password":"x","tenant_id":"nope"}`).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/auth/login", "", `{"email":"`+strings.Repeat("x", 2<<20)+`"}`).
		problem(t, http.StatusRequestEntityTooLarge, "payload_too_large")
}

func TestLoginAsksWhichTenant(t *testing.T) {
	e := newEnv(t)
	a := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	b := e.business(t, "tea", "BDG1", "other@tea.test")
	e.d.Exec(t, `INSERT INTO tenant_member (tenant_id, user_id) VALUES ($1, $2)`, b.tenant.ID, a.owner.ID)

	p := e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": password}).
		problem(t, http.StatusConflict, "tenant_required")
	if ids, _ := p["tenant_ids"].([]any); len(ids) != 2 {
		t.Errorf("tenant_ids = %v, want both tenants", p["tenant_ids"])
	}
	r := e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": password, "tenant_id": b.tenant.ID.String()})
	if r.Code != http.StatusOK {
		t.Fatalf("with tenant_id: %d %s", r.Code, r.Body.String())
	}
}

func TestRefreshAndLogout(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	s := e.login(t, "owner@kopi.test")

	r := e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken})
	if r.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", r.Code, r.Body.String())
	}
	var next sessionBody
	r.decode(t, &next)
	if e.do(t, "GET", "/v1/me", next.AccessToken, nil).Code != http.StatusOK {
		t.Error("the refreshed access token does not work")
	}

	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken}).
		problem(t, http.StatusUnauthorized, "token_reused")
	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": next.RefreshToken}).
		problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": ""}).
		problem(t, http.StatusBadRequest, "validation_failed")

	s2 := e.login(t, "owner@kopi.test")
	if r := e.do(t, "POST", "/v1/auth/logout", "", map[string]string{"refresh_token": s2.RefreshToken}); r.Code != http.StatusNoContent {
		t.Errorf("logout: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s2.RefreshToken}).
		problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestVerifyEmailAndResend(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")

	e.do(t, "POST", "/v1/auth/verify-email", "", map[string]string{"token": "vt1.nope"}).
		problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "POST", "/v1/auth/verify-email", "", map[string]string{"token": ""}).
		problem(t, http.StatusBadRequest, "validation_failed")

	// Resend never reveals whether the address exists.
	for _, email := range []string{"owner@kopi.test", "nobody@kopi.test"} {
		if r := e.do(t, "POST", "/v1/auth/resend-verification", "", map[string]string{"email": email}); r.Code != http.StatusAccepted {
			t.Errorf("resend %s: %d %s", email, r.Code, r.Body.String())
		}
	}
}

func TestOutlets(t *testing.T) {
	e := newEnv(t)
	a := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	b := e.business(t, "tea", "BDG1", "owner@tea.test")
	sa, sb := e.login(t, "owner@kopi.test"), e.login(t, "owner@tea.test")

	var list struct {
		Items []struct {
			ID       string `json:"id"`
			Code     string `json:"code"`
			Settings struct {
				Timezone string `json:"timezone"`
				TaxRate  int    `json:"tax_rate_bp"`
			} `json:"settings"`
		} `json:"items"`
	}
	r := e.do(t, "GET", "/v1/outlets", sa.AccessToken, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("list: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].ID != a.outlet.ID.String() || list.Items[0].Code != "JKT1" || list.Items[0].Settings.Timezone != "Asia/Jakarta" {
		t.Errorf("tenant A's outlets = %+v", list.Items)
	}

	if r := e.do(t, "GET", "/v1/outlets/"+a.outlet.ID.String(), sa.AccessToken, nil); r.Code != http.StatusOK {
		t.Errorf("get own outlet: %d %s", r.Code, r.Body.String())
	}
	// Another business's outlet is indistinguishable from one that does not exist.
	e.do(t, "GET", "/v1/outlets/"+b.outlet.ID.String(), sa.AccessToken, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "GET", "/v1/outlets/"+uuid.NewString(), sa.AccessToken, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "GET", "/v1/outlets/not-a-uuid", sa.AccessToken, nil).problem(t, http.StatusBadRequest, "validation_failed")
	if r := e.do(t, "GET", "/v1/outlets/"+b.outlet.ID.String(), sb.AccessToken, nil); r.Code != http.StatusOK {
		t.Errorf("tenant B reading its own outlet: %d", r.Code)
	}
}

func TestSuspendedTenantIsLockedOut(t *testing.T) {
	e := newEnv(t)
	b := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	s := e.login(t, "owner@kopi.test")

	e.d.Exec(t, `UPDATE tenant SET suspended_at = now() WHERE id = $1`, b.tenant.ID)

	e.do(t, "GET", "/v1/me", s.AccessToken, nil).problem(t, http.StatusForbidden, "tenant_suspended")
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": password}).
		problem(t, http.StatusForbidden, "tenant_suspended")
	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": s.RefreshToken}).
		problem(t, http.StatusForbidden, "tenant_suspended")
}

func TestLoginIsRateLimited(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")

	bad := map[string]string{"email": "owner@kopi.test", "password": "wrong password"}
	for range 5 {
		e.do(t, "POST", "/v1/auth/login", "", bad).problem(t, http.StatusUnauthorized, "invalid_credentials")
	}
	r := e.do(t, "POST", "/v1/auth/login", "", bad)
	r.problem(t, http.StatusTooManyRequests, "rate_limited")
	if r.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	// The limit follows the account, so even the right password waits.
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "OWNER@kopi.test", "password": password}).
		problem(t, http.StatusTooManyRequests, "rate_limited")
	// Other accounts are unaffected.
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "other@kopi.test", "password": password}).
		problem(t, http.StatusUnauthorized, "invalid_credentials")
}

func TestSessionsAreTenantScoped(t *testing.T) {
	// A token for tenant A, carried to tenant B's data, finds nothing: the claims name the
	// tenant and the database enforces it.
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	b := e.business(t, "tea", "BDG1", "owner@tea.test")
	sa := e.login(t, "owner@kopi.test")

	var list struct {
		Items []struct{ ID string }
	}
	e.do(t, "GET", "/v1/outlets", sa.AccessToken, nil).decode(t, &list)
	for _, o := range list.Items {
		if o.ID == b.outlet.ID.String() {
			t.Fatal("tenant A listed tenant B's outlet")
		}
	}
}
