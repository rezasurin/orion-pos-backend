package platform_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pquerna/otp/totp"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.t = c.t.Add(d) }

type env struct {
	d     *testdb.DB
	svc   *platform.Service
	clock *clock
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := kernel.NewBox(base64.RawURLEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC().Truncate(time.Second)}
	svc, err := platform.NewService(platform.Deps{
		Pool: d.Platform, Box: box, Keys: identity.NewEphemeralKeyring(), Clock: c,
		Entitlements: entitlements.NewAdmin(d.Platform), Tenants: tenancy.NewAdmin(d.Platform),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{d: d, svc: svc, clock: c}
}

// bootstrap creates the first operator.
func (e *env) bootstrap(t *testing.T, email string) platform.NewOperator {
	t.Helper()
	op, err := e.svc.CreateOperator(context.Background(), platform.Actor{}, email, "bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func (e *env) code(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *env) signIn(t *testing.T, op platform.NewOperator) platform.Session {
	t.Helper()
	ch, err := e.svc.Login(context.Background(), op.Operator.Email, op.Password)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.svc.VerifySecondFactor(context.Background(), ch.Token, e.code(t, op.TOTPSecret), "", platform.Meta{IP: "203.0.113.5", UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) actions(t *testing.T) []string {
	t.Helper()
	rows, err := e.d.Owner.Query(context.Background(), `SELECT action FROM platform_audit_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) tenant(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	tn, _, err := tenancy.NewService(e.d.App).CreateTenant(context.Background(), tenancy.NewTenant{
		Name: slug, Slug: slug, Outlet: tenancy.NewOutlet{Name: "Main", Code: "MAIN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tn.ID
}

func TestCreateOperator(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "  Ops@Orion.test ")
	if op.Operator.Email != "ops@orion.test" || op.Operator.TOTPConfirmed || len(op.RecoveryCodes) != 10 ||
		!strings.HasPrefix(op.TOTPURI, "otpauth://totp/") || len(op.Password) < 20 {
		t.Errorf("new operator = %+v", op)
	}

	// Nothing secret is stored in the clear.
	var row string
	if err := e.d.Owner.QueryRow(ctx, `SELECT (SELECT row_to_json(o)::text FROM operator o) || (SELECT coalesce(string_agg(row_to_json(r)::text, ''), '') FROM operator_recovery_code r)`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, secret := range append([]string{op.Password, op.TOTPSecret}, op.RecoveryCodes...) {
		if strings.Contains(row, secret) || strings.Contains(row, strings.ReplaceAll(secret, "-", "")) {
			t.Errorf("a secret is stored in the clear: %q", secret)
		}
	}

	// The first operator has no actor; later ones need one; the bootstrap works once.
	if _, err := e.svc.CreateOperator(ctx, platform.Actor{}, "second@orion.test", "again"); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("a second operator without an actor: %v, want ErrValidation", err)
	}
	if _, err := e.svc.CreateOperator(ctx, platform.Actor{OperatorID: &op.Operator.ID}, "OPS@orion.test", "dup"); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate email: %v, want ErrConflict", err)
	}
	second, err := e.svc.CreateOperator(ctx, platform.Actor{OperatorID: &op.Operator.ID}, "second@orion.test", "new hire")
	if err != nil || second.Operator.ID == op.Operator.ID {
		t.Fatalf("second operator: %v", err)
	}
	if _, err := e.svc.CreateOperator(ctx, platform.Actor{OperatorID: &op.Operator.ID}, "x@orion.test", " "); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("no reason: %v, want ErrValidation", err)
	}
	if _, err := e.svc.CreateOperator(ctx, platform.Actor{OperatorID: &op.Operator.ID}, "nope", "x"); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("bad email: %v, want ErrValidation", err)
	}
	if got := strings.Join(e.actions(t), ","); got != "operator.created,operator.created" {
		t.Errorf("audit = %s", got)
	}
}

func TestTwoFactorSignIn(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")

	ch, err := e.svc.Login(ctx, "ops@orion.test", op.Password)
	if err != nil || ch.Enrolled {
		t.Fatalf("login: %+v, %v", ch, err)
	}
	// The password alone is not a session, and the challenge is not a session token.
	if _, err := e.svc.Authenticate(ctx, ch.Token); !errors.Is(err, platform.ErrInvalidToken) {
		t.Errorf("a challenge used as a session: %v", err)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, ch.Token, "000000", "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("wrong code: %v, want ErrInvalidCredentials", err)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, ch.Token, "", "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("no code: %v, want ErrInvalidCredentials", err)
	}

	code := e.code(t, op.TOTPSecret)
	sess, err := e.svc.VerifySecondFactor(ctx, ch.Token, code, "", platform.Meta{IP: "203.0.113.5", UserAgent: "curl/8"})
	if err != nil {
		t.Fatal(err)
	}
	who, err := e.svc.Authenticate(ctx, sess.AccessToken)
	if err != nil || who.ID != op.Operator.ID || !who.TOTPConfirmed {
		t.Fatalf("authenticate: %+v, %v", who, err)
	}
	if !sess.ExpiresAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Errorf("session expires %v", sess.ExpiresAt)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, sess.AccessToken, code, "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidToken) {
		t.Errorf("a session used as a challenge: %v", err)
	}

	// A code works once, even inside its time window.
	ch2, _ := e.svc.Login(ctx, "ops@orion.test", op.Password)
	if !ch2.Enrolled {
		t.Error("enrolment was not confirmed by the first code")
	}
	if _, err := e.svc.VerifySecondFactor(ctx, ch2.Token, code, "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("replayed code: %v, want ErrInvalidCredentials", err)
	}
	e.clock.Advance(30 * time.Second)
	if _, err := e.svc.VerifySecondFactor(ctx, ch2.Token, e.code(t, op.TOTPSecret), "", platform.Meta{}); err != nil {
		t.Errorf("the next step's code: %v", err)
	}

	// The audit log carries the address and agent, and records the enrolment once.
	var ip, ua string
	if err := e.d.Owner.QueryRow(ctx, `SELECT host(ip), user_agent FROM platform_audit_log WHERE action = 'operator.totp_confirmed'`).Scan(&ip, &ua); err != nil || ip != "203.0.113.5" || ua != "curl/8" {
		t.Errorf("confirmation audit: ip %q agent %q (%v)", ip, ua, err)
	}
	if got := strings.Join(e.actions(t), ","); got != "operator.created,operator.totp_confirmed,operator.signed_in,operator.signed_in" {
		t.Errorf("audit = %s", got)
	}
}

func TestSignInFailuresLookAlike(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	for name, in := range map[string][2]string{
		"wrong password": {"ops@orion.test", "not the password"},
		"unknown":        {"nobody@orion.test", op.Password},
		"empty":          {"", ""},
	} {
		if _, err := e.svc.Login(ctx, in[0], in[1]); !errors.Is(err, platform.ErrInvalidCredentials) {
			t.Errorf("%s: %v, want ErrInvalidCredentials", name, err)
		}
	}

	// A disabled operator cannot start, finish or keep a session.
	sess := e.signIn(t, op)
	ch, _ := e.svc.Login(ctx, "ops@orion.test", op.Password)
	e.d.Exec(t, `UPDATE operator SET disabled_at = now()`)
	if _, err := e.svc.Authenticate(ctx, sess.AccessToken); !errors.Is(err, platform.ErrInvalidToken) {
		t.Errorf("session of a disabled operator: %v", err)
	}
	e.clock.Advance(30 * time.Second)
	if _, err := e.svc.VerifySecondFactor(ctx, ch.Token, e.code(t, op.TOTPSecret), "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("finishing a sign-in after being disabled: %v", err)
	}
	if _, err := e.svc.Login(ctx, "ops@orion.test", op.Password); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("login of a disabled operator: %v", err)
	}
}

func TestTokensExpire(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	ch, _ := e.svc.Login(ctx, "ops@orion.test", op.Password)
	sess := e.signIn(t, op)

	e.clock.Advance(6 * time.Minute)
	if _, err := e.svc.VerifySecondFactor(ctx, ch.Token, e.code(t, op.TOTPSecret), "", platform.Meta{}); !errors.Is(err, platform.ErrInvalidToken) {
		t.Errorf("expired challenge: %v", err)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.svc.Authenticate(ctx, sess.AccessToken); !errors.Is(err, platform.ErrInvalidToken) {
		t.Errorf("expired session: %v", err)
	}
}

func TestRecoveryCodes(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	login := func() platform.Challenge {
		ch, err := e.svc.Login(ctx, "ops@orion.test", op.Password)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	// Case and dashes do not matter; a code works once.
	messy := strings.ToLower(op.RecoveryCodes[0])
	if _, err := e.svc.VerifySecondFactor(ctx, login().Token, "", messy, platform.Meta{}); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, login().Token, "", op.RecoveryCodes[0], platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("a recovery code used twice: %v", err)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, login().Token, "", "AAAA-AAAA-AAAA-AAAA", platform.Meta{}); !errors.Is(err, platform.ErrInvalidCredentials) {
		t.Errorf("a made-up recovery code: %v", err)
	}
	if _, err := e.svc.VerifySecondFactor(ctx, login().Token, "", op.RecoveryCodes[1], platform.Meta{}); err != nil {
		t.Errorf("a second, different code: %v", err)
	}
	if got := strings.Join(e.actions(t), ","); !strings.Contains(got, "operator.recovery_code_used") {
		t.Errorf("recovery use not audited: %s", got)
	}
}

func TestConcurrentUseOfOneCode(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	code := e.code(t, op.TOTPSecret)

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, other := 0, []error{}
	for range n {
		ch, _ := e.svc.Login(ctx, "ops@orion.test", op.Password)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.VerifySecondFactor(ctx, ch.Token, code, "", platform.Meta{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, platform.ErrInvalidCredentials):
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || len(other) > 0 {
		t.Errorf("%d of %d concurrent uses of one code succeeded (other errors %v), want exactly 1", wins, n, other)
	}
}

func TestOperatorActionsAreAudited(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	a := platform.Actor{OperatorID: &op.Operator.ID, Meta: platform.Meta{IP: "198.51.100.1"}}
	tenant := e.tenant(t, "kopi")

	exp := e.clock.Now().Add(24 * time.Hour)
	if err := e.svc.SetOverride(ctx, a, "kopi", entitlements.LimitDevices, 9, &exp, "pilot extension"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetOverride(ctx, a, "kopi", entitlements.LimitDevices, 12, nil, "bigger pilot"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ClearOverride(ctx, a, "kopi", entitlements.LimitDevices, "pilot over"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ClearOverride(ctx, a, "kopi", entitlements.LimitDevices, "again"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("clearing nothing: %v, want ErrNotFound", err)
	}
	if err := e.svc.SetTenantSuspended(ctx, a, "kopi", true, "non-payment"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetTenantSuspended(ctx, a, "kopi", true, "twice"); !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("suspending twice: %v, want ErrConflict", err)
	}
	if err := e.svc.SetTenantSuspended(ctx, a, "kopi", false, "paid"); err != nil {
		t.Fatal(err)
	}

	// Failures write nothing: the audit entry and the change are one transaction.
	for name, fn := range map[string]func() error{
		"unknown tenant": func() error { return e.svc.SetTenantSuspended(ctx, a, "nope", true, "x") },
		"no reason":      func() error { return e.svc.SetOverride(ctx, a, "kopi", entitlements.LimitDevices, 1, nil, " ") },
		"bad value":      func() error { return e.svc.SetOverride(ctx, a, "kopi", entitlements.ModuleInventory, 5, nil, "x") },
		"unknown key":    func() error { return e.svc.SetOverride(ctx, a, "kopi", "limit.nope", 1, nil, "x") },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	want := "operator.created,entitlement.override_set,entitlement.override_set,entitlement.override_cleared,tenant.suspended,tenant.reinstated"
	if got := strings.Join(e.actions(t), ","); got != want {
		t.Errorf("audit = %s\nwant    %s", got, want)
	}
	var before, after, reason string
	var tid uuid.UUID
	if err := e.d.Owner.QueryRow(ctx, `SELECT before::text, after::text, reason, tenant_id FROM platform_audit_log
		WHERE action = 'entitlement.override_set' ORDER BY id DESC LIMIT 1`).Scan(&before, &after, &reason, &tid); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(before, `"value": 9`) || !strings.Contains(after, `"value": 12`) || reason != "bigger pilot" || tid != tenant {
		t.Errorf("second override audit: before %s after %s reason %q", before, after, reason)
	}
	var suspended *time.Time
	_ = e.d.Owner.QueryRow(ctx, `SELECT suspended_at FROM tenant WHERE id = $1`, tenant).Scan(&suspended)
	if suspended != nil {
		t.Error("the tenant is still suspended after reinstating")
	}
}

func TestAuditLogIsAppendOnlyAndPages(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	op := e.bootstrap(t, "ops@orion.test")
	a := platform.Actor{OperatorID: &op.Operator.ID}
	e.tenant(t, "kopi")
	for i := range 6 {
		if err := e.svc.SetOverride(ctx, a, "kopi", entitlements.LimitStaff, int64(10+i), nil, "r"); err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	page := kernel.Page{Limit: 3}
	for range 5 {
		res, err := e.svc.ListAuditLog(ctx, page, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range res.Items {
			seen = append(seen, en.ID.String())
		}
		if res.Next == uuid.Nil {
			break
		}
		page.After = res.Next
	}
	if len(seen) != 7 { // creation plus six overrides
		t.Fatalf("saw %d entries, want 7", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Fatalf("not newest first: %v", seen)
		}
	}

	for _, sql := range []string{`UPDATE platform_audit_log SET reason = 'edited'`, `DELETE FROM platform_audit_log`} {
		_, err := e.d.Owner.Exec(ctx, sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s as owner: %v, want 42501 from the trigger", sql, err)
		}
		if _, err := e.d.Platform.Exec(ctx, sql); err == nil {
			t.Errorf("%s as platform succeeded", sql)
		}
	}
}
