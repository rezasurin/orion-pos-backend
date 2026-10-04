package api_test

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
)

const newPassword = "a different long password"

// resetToken runs the queued reset email job like the worker and returns the token in the email.
func (e *env) resetToken(t *testing.T) string {
	t.Helper()
	var tenantID, userID string
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT args->>'tenant_id', args->>'user_id' FROM river_job
		WHERE kind = 'password_reset_email' ORDER BY id DESC LIMIT 1`).Scan(&tenantID, &userID); err != nil {
		t.Fatalf("no reset email was queued: %v", err)
	}
	mail := &notify.MemorySender{}
	jobs := identity.NewJobs(identity.JobsDeps{Platform: e.d.Platform, Sender: mail, PublicURL: "https://app.example.test"})
	if err := jobs.SendPasswordResetEmail(t.Context(), identity.PasswordResetArgs{TenantID: uuid.MustParse(tenantID), UserID: uuid.MustParse(userID)}); err != nil {
		t.Fatal(err)
	}
	sent := mail.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d emails", len(sent))
	}
	return regexp.MustCompile(`token=(\S+)`).FindStringSubmatch(sent[0].Text)[1]
}

func TestForgotAndResetPasswordOverHTTP(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	old := e.login(t, "owner@kopi.test")

	if r := e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": "Owner@Kopi.test"}); r.Code != http.StatusAccepted || r.Body.Len() != 0 {
		t.Fatalf("forgot-password: %d %q, want 202 with no body", r.Code, r.Body.String())
	}
	token := e.resetToken(t)

	e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": token, "password": "short"}).problem(t, http.StatusBadRequest, "validation_failed")
	if r := e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": token, "password": newPassword}); r.Code != http.StatusNoContent || r.Body.Len() != 0 {
		t.Fatalf("reset-password: %d %q, want 204 (and a weak password must not have spent the link)", r.Code, r.Body.String())
	}
	e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": token, "password": newPassword}).problem(t, http.StatusUnauthorized, "invalid_token")

	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": password}).problem(t, http.StatusUnauthorized, "invalid_credentials")
	if r := e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": newPassword}); r.Code != http.StatusOK {
		t.Errorf("login with the new password: %d %s", r.Code, r.Body.String())
	}
	// A session from before the reset cannot be renewed.
	e.do(t, "POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": old.RefreshToken}).problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestForgotPasswordDoesNotRevealWhoIsRegistered(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	known := e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": "owner@kopi.test"})
	unknown := e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": "nobody@kopi.test"})
	for name, r := range map[string]response{"known": known, "unknown": unknown} {
		if r.Code != http.StatusAccepted || r.Body.Len() != 0 {
			t.Errorf("%s: %d %q", name, r.Code, r.Body.String())
		}
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Set-Cookie"} {
		if known.Header().Get(h) != unknown.Header().Get(h) {
			t.Errorf("header %s differs: %q vs %q", h, known.Header().Get(h), unknown.Header().Get(h))
		}
	}
	var n int
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM river_job WHERE kind = 'password_reset_email'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("reset emails queued = %d (%v), want 1: only for the account that exists", n, err)
	}
}

func TestPasswordResetEndpointsValidateAndRateLimit(t *testing.T) {
	e := newEnv(t)
	e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": ""}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": "", "password": newPassword}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": "garbage", "password": newPassword}).problem(t, http.StatusUnauthorized, "invalid_token")

	// Per address: three emails to one address, then a wait.
	e = newEnv(t)
	for range 3 {
		if r := e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": "a@kopi.test"}); r.Code != http.StatusAccepted {
			t.Fatalf("within the limit: %d", r.Code)
		}
	}
	r := e.do(t, "POST", "/v1/auth/forgot-password", "", map[string]string{"email": "a@kopi.test"})
	r.problem(t, http.StatusTooManyRequests, "rate_limited")
	if r.Header().Get("Retry-After") == "" {
		t.Error("a 429 must say when to retry")
	}

	// Per caller: ten attempts at a token, then a wait.
	e = newEnv(t)
	for range 10 {
		e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": "garbage", "password": newPassword}).problem(t, http.StatusUnauthorized, "invalid_token")
	}
	e.do(t, "POST", "/v1/auth/reset-password", "", map[string]string{"token": "garbage", "password": newPassword}).problem(t, http.StatusTooManyRequests, "rate_limited")
}
