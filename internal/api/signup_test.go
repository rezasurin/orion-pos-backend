package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
)

func signupBody(email string) map[string]any {
	return map[string]any{
		"business_name": "Kopi Senja!", "owner_name": "Sari", "email": email, "password": password,
		"terms_version": identity.TermsVersion,
	}
}

// count runs a count(*) query as the schema owner, which sees every tenant.
func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSignupCreatesBusinessOutletOwnerAndTerms(t *testing.T) {
	e := newEnv(t)
	body := signupBody("Sari@Senja.test")
	body["outlet_name"], body["locale"] = "Senja Kemang", "en"
	if r := e.do(t, "POST", "/v1/auth/signup", "", body); r.Code != http.StatusAccepted {
		t.Fatalf("signup: %d %s", r.Code, r.Body.String())
	}

	// Not signed in until the email is verified, and the verification email is queued.
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "sari@senja.test", "password": password}).
		problem(t, http.StatusForbidden, "email_not_verified")
	if n := e.count(t, `SELECT count(*) FROM river_job WHERE kind = 'verify_email'`); n != 1 {
		t.Errorf("%d verification jobs, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM terms_acceptance a JOIN user_account u ON u.id = a.user_id
		WHERE u.email = 'sari@senja.test' AND a.version = $1`, identity.TermsVersion); n != 1 {
		t.Errorf("%d terms acceptances, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tenant WHERE name = 'Kopi Senja!' AND slug LIKE 'kopi-senja-%'`); n != 1 {
		t.Errorf("%d tenants with the generated slug, want 1", n)
	}

	e.d.Exec(t, `UPDATE user_account SET email_verified_at = now() WHERE email = 'sari@senja.test'`)
	s := e.login(t, "sari@senja.test")
	var me struct {
		User struct {
			Locale string `json:"locale"`
		} `json:"user"`
		IsOwner bool `json:"is_owner"`
	}
	e.do(t, "GET", "/v1/me", s.AccessToken, nil).decode(t, &me)
	if me.User.Locale != "en" || !me.IsOwner {
		t.Errorf("me = %+v, want an English-speaking owner", me)
	}
	var outlets struct {
		Items []struct {
			Name string `json:"name"`
			Code string `json:"code"`
		} `json:"items"`
	}
	e.do(t, "GET", "/v1/outlets", s.AccessToken, nil).decode(t, &outlets)
	if o := outlets.Items; len(o) != 1 || o[0].Name != "Senja Kemang" || o[0].Code != "OUT1" {
		t.Errorf("outlets = %+v", o)
	}
	if roles := e.roles(t, s.AccessToken); roles["Cashier"].ID == "" || roles["Manager"].ID == "" {
		t.Errorf("system roles missing: %v", roles)
	}
}

// None of these may create anything, and the first three must look exactly like a real signup.
func TestSignupRefusalsCreateNothing(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tenants := e.count(t, `SELECT count(*) FROM tenant`)

	taken := signupBody("OWNER@kopi.test") // an existing account: the transaction rolls back
	honeypot := signupBody("bot@spam.test")
	honeypot["website"] = "http://spam.test"
	for name, body := range map[string]map[string]any{"email taken": taken, "honeypot": honeypot} {
		if r := e.do(t, "POST", "/v1/auth/signup", "", body); r.Code != http.StatusAccepted || r.Body.Len() != 0 {
			t.Errorf("%s: %d %q, want an empty 202", name, r.Code, r.Body.String())
		}
	}

	old := signupBody("new@senja.test")
	old["terms_version"] = "2020-01-01"
	e.do(t, "POST", "/v1/auth/signup", "", old).problem(t, http.StatusConflict, "terms_outdated")
	noTerms := signupBody("new@senja.test")
	delete(noTerms, "terms_version")
	e.do(t, "POST", "/v1/auth/signup", "", noTerms).problem(t, http.StatusBadRequest, "validation_failed")
	badCode := signupBody("new@senja.test")
	badCode["outlet_code"] = "x"
	e.do(t, "POST", "/v1/auth/signup", "", badCode).problem(t, http.StatusBadRequest, "validation_failed")

	if n := e.count(t, `SELECT count(*) FROM tenant`); n != tenants {
		t.Errorf("%d tenants, want %d", n, tenants)
	}
	if n := e.count(t, `SELECT count(*) FROM user_account`); n != 1 {
		t.Errorf("%d users, want 1", n)
	}
}

func TestSignupIsRateLimitedByAddress(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		if r := e.do(t, "POST", "/v1/auth/signup", "", signupBody(fmt.Sprintf("owner%d@senja.test", i))); r.Code != http.StatusAccepted {
			t.Fatalf("signup %d: %d %s", i, r.Code, r.Body.String())
		}
	}
	e.do(t, "POST", "/v1/auth/signup", "", signupBody("owner5@senja.test")).problem(t, http.StatusTooManyRequests, "rate_limited")
	if n := e.count(t, `SELECT count(*) FROM tenant`); n != 5 {
		t.Errorf("%d tenants, want 5", n)
	}
}
