package api_test

import (
	"net/http"
	"testing"
)

func signupForm(email string) map[string]any {
	return map[string]any{"business_name": "Kopi Senja", "owner_name": "Sari", "email": email, "password": password}
}

func (e *env) tenantCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM tenant`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSignUpCreatesABusinessThatItsOwnerCanUseOnceVerified(t *testing.T) {
	e := newEnv(t)

	r := e.do(t, "POST", "/v1/signup", "", signupForm("Sari@Kopi.test"))
	if r.Code != http.StatusAccepted || r.Body.Len() != 0 {
		t.Fatalf("signup: %d %q, want 202 with no body", r.Code, r.Body.String())
	}

	// Not before the address is verified.
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "sari@kopi.test", "password": password}).problem(t, http.StatusForbidden, "email_not_verified")

	e.d.Exec(t, `UPDATE user_account SET email_verified_at = now() WHERE email = 'sari@kopi.test'`)
	s := e.login(t, "sari@kopi.test")

	var me struct {
		User   struct{ Email string }
		Tenant struct {
			Name               string
			SubscriptionStatus string `json:"subscription_status"`
		}
		IsOwner     bool     `json:"is_owner"`
		Permissions []string `json:"permissions"`
	}
	e.do(t, "GET", "/v1/me", s.AccessToken, nil).decode(t, &me)
	if me.User.Email != "sari@kopi.test" || me.Tenant.Name != "Kopi Senja" || !me.IsOwner || len(me.Permissions) == 0 {
		t.Errorf("me = %+v", me)
	}

	var outlets struct{ Items []struct{ Code, Name string } }
	e.do(t, "GET", "/v1/outlets", s.AccessToken, nil).decode(t, &outlets)
	if len(outlets.Items) != 1 || outlets.Items[0].Code != "KOPI1" || outlets.Items[0].Name != "Kopi Senja" {
		t.Errorf("outlets = %+v, want the one first outlet", outlets)
	}
	var staff struct {
		Items []struct {
			DisplayName string `json:"display_name"`
			IsOwner     bool   `json:"is_owner"`
		}
	}
	e.do(t, "GET", "/v1/staff", s.AccessToken, nil).decode(t, &staff)
	if len(staff.Items) != 1 || staff.Items[0].DisplayName != "Sari" || !staff.Items[0].IsOwner {
		t.Errorf("staff = %+v", staff)
	}
	var roles struct{ Items []struct{ Name string } }
	e.do(t, "GET", "/v1/roles", s.AccessToken, nil).decode(t, &roles)
	if len(roles.Items) != 4 {
		t.Errorf("roles = %+v, want Owner, Manager, Cashier and Kitchen", roles)
	}
}

// What the caller sees must not depend on whether the address was already registered.
func TestSignUpDoesNotRevealWhoIsRegistered(t *testing.T) {
	e := newEnv(t)
	first := e.do(t, "POST", "/v1/signup", "", signupForm("sari@kopi.test"))
	e.d.Exec(t, `UPDATE user_account SET email_verified_at = now()`)
	known := e.do(t, "POST", "/v1/signup", "", signupForm("sari@kopi.test"))
	other := e.do(t, "POST", "/v1/signup", "", signupForm("budi@kopi.test"))

	for name, r := range map[string]response{"first": first, "known address": known, "new address": other} {
		if r.Code != http.StatusAccepted || r.Body.Len() != 0 {
			t.Errorf("%s: %d %q", name, r.Code, r.Body.String())
		}
	}
	for _, h := range []string{"Content-Type", "Content-Length", "Set-Cookie", "Location"} {
		if known.Header().Get(h) != other.Header().Get(h) {
			t.Errorf("header %s differs between a known and a new address: %q vs %q", h, known.Header().Get(h), other.Header().Get(h))
		}
	}
	if n := e.tenantCount(t); n != 2 {
		t.Errorf("tenants = %d, want 2 (the repeat created nothing)", n)
	}
}

func TestSignUpIgnoresTheHoneypot(t *testing.T) {
	e := newEnv(t)
	form := signupForm("bot@spam.test")
	form["website"] = "http://spam.test"
	r := e.do(t, "POST", "/v1/signup", "", form)
	if r.Code != http.StatusAccepted || r.Body.Len() != 0 {
		t.Errorf("honeypot: %d %q, want the same 202 a person gets", r.Code, r.Body.String())
	}
	if n := e.tenantCount(t); n != 0 {
		t.Errorf("a bot's sign-up created %d tenants", n)
	}
	// An empty or blank honeypot is an ordinary sign-up.
	form["website"] = "  "
	e.do(t, "POST", "/v1/signup", "", form)
	if n := e.tenantCount(t); n != 1 {
		t.Errorf("a blank honeypot blocked a real sign-up: tenants = %d", n)
	}
}

func TestSignUpValidation(t *testing.T) {
	// Each case gets a fresh server: invalid forms use up rate limit like any other.
	for name, mutate := range map[string]func(map[string]any){
		"no business name": func(f map[string]any) { delete(f, "business_name") },
		"no owner name":    func(f map[string]any) { f["owner_name"] = "" },
		"short password":   func(f map[string]any) { f["password"] = "short" },
		"bad email":        func(f map[string]any) { f["email"] = "nope" },
		"bad outlet code":  func(f map[string]any) { f["outlet_code"] = "k" },
		"bad time zone":    func(f map[string]any) { f["timezone"] = "Europe/Paris" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			f := signupForm("sari@kopi.test")
			mutate(f)
			e.do(t, "POST", "/v1/signup", "", f).problem(t, http.StatusBadRequest, "validation_failed")
			if n := e.tenantCount(t); n != 0 {
				t.Errorf("an invalid sign-up left %d tenants", n)
			}
		})
	}
	t.Run("not json", func(t *testing.T) {
		e := newEnv(t)
		e.do(t, "POST", "/v1/signup", "", "{").problem(t, http.StatusBadRequest, "validation_failed")
	})
}

func TestSignUpIsRateLimited(t *testing.T) {
	// Per email address: three forms, then a wait.
	e := newEnv(t)
	for range 3 {
		if r := e.do(t, "POST", "/v1/signup", "", signupForm("sari@kopi.test")); r.Code != http.StatusAccepted {
			t.Fatalf("within the limit: %d", r.Code)
		}
	}
	r := e.do(t, "POST", "/v1/signup", "", signupForm("sari@kopi.test"))
	r.problem(t, http.StatusTooManyRequests, "rate_limited")
	if r.Header().Get("Retry-After") == "" {
		t.Error("a 429 must say when to retry")
	}

	// Per caller address: five forms for different addresses, then a wait.
	e = newEnv(t)
	for i := range 5 {
		if r := e.do(t, "POST", "/v1/signup", "", signupForm(string(rune('a'+i))+"@kopi.test")); r.Code != http.StatusAccepted {
			t.Fatalf("within the address limit: %d", r.Code)
		}
	}
	e.do(t, "POST", "/v1/signup", "", signupForm("z@kopi.test")).problem(t, http.StatusTooManyRequests, "rate_limited")
	if n := e.tenantCount(t); n != 5 {
		t.Errorf("tenants = %d, want 5: the rate-limited request must create nothing", n)
	}
}
