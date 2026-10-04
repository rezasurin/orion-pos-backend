package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
)

type operatorSession struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	OperatorID  string `json:"operator_id"`
}

func (e *env) operator(t *testing.T) platform.NewOperator {
	t.Helper()
	op, err := e.platform.CreateOperator(context.Background(), platform.Actor{}, "ops@orion.test", "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (e *env) operatorSignIn(t *testing.T, op platform.NewOperator) operatorSession {
	t.Helper()
	r := e.do(t, "POST", "/admin/auth/login", "", map[string]string{"email": op.Operator.Email, "password": op.Password})
	if r.Code != http.StatusOK {
		t.Fatalf("admin login: %d %s", r.Code, r.Body.String())
	}
	var ch struct {
		Token string `json:"challenge_token"`
	}
	r.decode(t, &ch)
	code, _ := totp.GenerateCode(op.TOTPSecret, time.Now())
	r = e.do(t, "POST", "/admin/auth/totp/verify", "", map[string]string{"challenge_token": ch.Token, "code": code})
	if r.Code != http.StatusOK {
		t.Fatalf("admin verify: %d %s", r.Code, r.Body.String())
	}
	var s operatorSession
	r.decode(t, &s)
	return s
}

func TestOperatorSignInAndAuditLog(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	op := e.operator(t)
	sess := e.operatorSignIn(t, op)
	if sess.TokenType != "Bearer" || sess.OperatorID != op.Operator.ID.String() {
		t.Fatalf("session = %+v", sess)
	}

	if err := e.platform.SetTenantSuspended(context.Background(), platform.Actor{OperatorID: &op.Operator.ID}, "kopi", true, "test"); err != nil {
		t.Fatal(err)
	}
	var log struct {
		Items []struct {
			Action string         `json:"action"`
			Reason string         `json:"reason"`
			After  map[string]any `json:"after"`
			IP     string         `json:"ip"`
		} `json:"items"`
	}
	r := e.do(t, "GET", "/admin/audit-log?limit=2", sess.AccessToken, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("audit log: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &log)
	if len(log.Items) != 2 || log.Items[0].Action != "tenant.suspended" || log.Items[0].After["suspended"] != true || log.Items[1].Action != "operator.signed_in" {
		t.Errorf("audit log = %+v", log.Items)
	}
	if log.Items[1].IP != "192.0.2.1" {
		t.Errorf("sign-in audited from %q, want the caller's address", log.Items[1].IP)
	}
	e.do(t, "GET", "/admin/audit-log?limit=0", sess.AccessToken, nil).problem(t, http.StatusBadRequest, "validation_failed")

	// The suspension the operator made is real: the tenant's owner is locked out.
	_ = entitlements.LimitStaff
	e.do(t, "POST", "/v1/auth/login", "", map[string]string{"email": "owner@kopi.test", "password": password}).
		problem(t, http.StatusForbidden, "tenant_suspended")
}

func TestOperatorAndTenantTokensStayApart(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	op := e.operator(t)
	operator := e.operatorSignIn(t, op).AccessToken
	owner := e.login(t, "owner@kopi.test").AccessToken

	e.do(t, "GET", "/admin/audit-log", owner, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/admin/audit-log", "", nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/me", operator, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/pos/roster", operator, nil).problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestOperatorSignInFailures(t *testing.T) {
	e := newEnv(t)
	op := e.operator(t)

	e.do(t, "POST", "/admin/auth/login", "", map[string]string{"email": "ops@orion.test", "password": "wrong"}).
		problem(t, http.StatusUnauthorized, "invalid_credentials")
	e.do(t, "POST", "/admin/auth/login", "", map[string]string{"email": "", "password": ""}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/admin/auth/totp/verify", "", map[string]string{"challenge_token": "x"}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/admin/auth/totp/verify", "", map[string]string{"challenge_token": "garbage", "code": "123456"}).
		problem(t, http.StatusUnauthorized, "invalid_token")

	// Guessing codes against one challenge is limited to a few attempts.
	var ch struct {
		Token string `json:"challenge_token"`
	}
	e.do(t, "POST", "/admin/auth/login", "", map[string]string{"email": "ops@orion.test", "password": op.Password}).decode(t, &ch)
	limited := false
	for range 10 {
		r := e.do(t, "POST", "/admin/auth/totp/verify", "", map[string]string{"challenge_token": ch.Token, "code": "000000"})
		if r.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
		r.problem(t, http.StatusUnauthorized, "invalid_credentials")
	}
	if !limited {
		t.Error("ten wrong codes on one challenge were all examined")
	}
}

func TestConsoleIsOffWithoutAPlatformDatabase(t *testing.T) {
	e := newEnvWith(t, false)
	e.do(t, "POST", "/admin/auth/login", "", map[string]string{"email": "a@b.test", "password": "x"}).problem(t, http.StatusServiceUnavailable, "admin_disabled")
	e.do(t, "GET", "/admin/audit-log", "some-token", nil).problem(t, http.StatusServiceUnavailable, "admin_disabled")
}
