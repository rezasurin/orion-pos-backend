package identity_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const password = "correct horse battery"

// testClock can be moved forward by a test.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	d      *testdb.DB
	svc    *identity.Service
	jobs   *identity.Jobs
	mail   *notify.MemorySender
	clock  *testClock
	tenant uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	mail := &notify.MemorySender{}

	// An insert-only river client: the API inserts jobs, the worker (here: Jobs) runs them.
	client, err := river.NewClient(riverpgxv5.New(d.App), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := identity.NewService(identity.Deps{
		Entitlements: entitlements.NewResolver(d.App, clock),
		Pool:         d.App,
		Clock:        clock,
		TenantKeys:   identity.NewEphemeralKeyring(),
		DeviceKeys:   identity.NewEphemeralKeyring(),
		Hasher:       identity.NewHasher(4),
		Jobs:         client,
		// Cheap enough for tests; the production cost is exercised in password_test.go.
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		d: d, svc: svc, mail: mail, clock: clock,
		jobs: identity.NewJobs(identity.JobsDeps{
			Platform: d.Platform, Sender: mail, Clock: clock, PublicURL: "https://app.example.test",
		}),
	}
	e.tenant = e.newTenant(t, "kopi")
	return e
}

func (e *env) newTenant(t *testing.T, slug string) uuid.UUID {
	t.Helper()
	id := kernel.NewID()
	e.d.Exec(t, `INSERT INTO tenant (id, name, slug, plan_id)
		SELECT $1, $2, $2, id FROM plan WHERE code = 'early_access'`, id, slug)
	err := kernel.TenantTx(context.Background(), e.d.App, id, func(tx pgx.Tx) error {
		return identity.SeedRoles(context.Background(), tx, id)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) member(t *testing.T, email string, verified bool) identity.User {
	t.Helper()
	return e.memberIn(t, e.tenant, email, verified)
}

func (e *env) memberIn(t *testing.T, tenant uuid.UUID, email string, verified bool) identity.User {
	t.Helper()
	u, err := e.svc.CreateMember(context.Background(), identity.NewMember{
		TenantID: tenant, Email: email, Password: password, DisplayName: "Test", IsOwner: true, EmailVerified: verified,
	})
	if err != nil {
		t.Fatalf("CreateMember(%s): %v", email, err)
	}
	return u
}

func (e *env) login(t *testing.T, email string) identity.Session {
	t.Helper()
	s, err := e.svc.Login(context.Background(), identity.LoginInput{Email: email, Password: password})
	if err != nil {
		t.Fatalf("Login(%s): %v", email, err)
	}
	return s
}

func (e *env) jobCount(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := e.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM river_job WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginRefreshLogout(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "owner@kopi.test", true)

	sess, err := e.svc.Login(ctx, identity.LoginInput{Email: "  Owner@Kopi.TEST ", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	if sess.TenantID != e.tenant || sess.UserID != u.ID {
		t.Errorf("session for tenant %v user %v, want %v %v", sess.TenantID, sess.UserID, e.tenant, u.ID)
	}

	p, err := e.svc.Authenticate(identity.AudienceTenant, sess.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != identity.PrincipalUser || p.TenantID != e.tenant || p.UserID != u.ID {
		t.Errorf("principal = %+v", p)
	}
	if _, err := e.svc.Authenticate(identity.AudienceDevice, sess.AccessToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("a user token on a device route: err = %v, want ErrInvalidToken", err)
	}

	// Rotation: the first refresh works, replaying it revokes the whole session.
	next, err := e.svc.Refresh(ctx, sess.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if next.RefreshToken == sess.RefreshToken || next.AccessToken == "" {
		t.Error("refresh did not rotate the token")
	}
	if _, err := e.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, identity.ErrTokenReuse) {
		t.Fatalf("replayed refresh token: err = %v, want ErrTokenReuse", err)
	}
	if _, err := e.svc.Refresh(ctx, next.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("descendant of a replayed token: err = %v, want ErrInvalidToken (family revoked)", err)
	}

	// Logout revokes the session and can be repeated.
	s2 := e.login(t, "owner@kopi.test")
	if err := e.svc.Logout(ctx, s2.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Refresh(ctx, s2.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("refresh after logout: err = %v, want ErrInvalidToken", err)
	}
	for _, tok := range []string{s2.RefreshToken, "garbage", ""} {
		if err := e.svc.Logout(ctx, tok); err != nil {
			t.Errorf("Logout(%q) = %v, want nil", tok, err)
		}
	}
}

func TestRefreshTokenExpires(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	sess := e.login(t, "owner@kopi.test")

	e.clock.Advance(31 * 24 * time.Hour)
	if _, err := e.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
	if _, err := e.svc.Authenticate(identity.AudienceTenant, sess.AccessToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("expired access token: err = %v, want ErrInvalidToken", err)
	}
}

func TestLoginFailures(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	e.member(t, "new@kopi.test", false)

	for name, in := range map[string]identity.LoginInput{
		"wrong password": {Email: "owner@kopi.test", Password: "wrong password!"},
		"unknown email":  {Email: "nobody@kopi.test", Password: password},
		"empty":          {},
	} {
		if _, err := e.svc.Login(ctx, in); !errors.Is(err, identity.ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "new@kopi.test", Password: password}); !errors.Is(err, identity.ErrEmailNotVerified) {
		t.Errorf("unverified: err = %v, want ErrEmailNotVerified", err)
	}
	// The verification state is only revealed after the password checks out.
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "new@kopi.test", Password: "wrong password!"}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("unverified with wrong password: err = %v, want ErrInvalidCredentials", err)
	}
}

func TestLoginGate(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	sess := e.login(t, "owner@kopi.test")

	errSuspended := errors.New("suspended")
	svc, err := identity.NewService(identity.Deps{
		Pool: e.d.App, Clock: e.clock, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		Gate: func(context.Context, uuid.UUID) error { return errSuspended },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password}); !errors.Is(err, errSuspended) {
		t.Errorf("login: err = %v, want the gate's error", err)
	}
	if _, err := svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, errSuspended) {
		t.Errorf("refresh: err = %v, want the gate's error", err)
	}
}

func TestLoginAcrossTenants(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "owner@kopi.test", true)
	second := e.newTenant(t, "tea")
	e.d.Exec(t, `INSERT INTO tenant_member (tenant_id, user_id) VALUES ($1, $2)`, second, u.ID)

	_, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password})
	var need *identity.TenantRequiredError
	if !errors.As(err, &need) || len(need.TenantIDs) != 2 {
		t.Fatalf("err = %v, want TenantRequiredError listing 2 tenants", err)
	}

	s, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password, TenantID: second})
	if err != nil || s.TenantID != second {
		t.Fatalf("login to the second tenant: %v, tenant %v", err, s.TenantID)
	}
	other := e.newTenant(t, "stranger")
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password, TenantID: other}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("a tenant the user is not in: err = %v, want ErrInvalidCredentials", err)
	}
}

func TestRemovedMemberLosesAccess(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "owner@kopi.test", true)
	sess := e.login(t, "owner@kopi.test")
	p, _ := e.svc.Authenticate(identity.AudienceTenant, sess.AccessToken)

	acc, err := e.svc.LoadAccess(ctx, p)
	if err != nil || !acc.IsOwner {
		t.Fatalf("LoadAccess = %+v, %v", acc, err)
	}

	e.d.Exec(t, `DELETE FROM tenant_member WHERE user_id = $1`, u.ID)
	if _, err := e.svc.LoadAccess(ctx, p); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("LoadAccess after removal: err = %v, want ErrInvalidToken", err)
	}
	if _, err := e.svc.Refresh(ctx, sess.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("Refresh after removal: err = %v, want ErrInvalidToken", err)
	}
}

var linkToken = regexp.MustCompile(`token=(\S+)`)

func TestEmailVerification(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "new@kopi.test", false)

	// The job is queued with ids only; no secret waits in the table.
	if n := e.jobCount(t, "verify_email"); n != 1 {
		t.Fatalf("verify_email jobs = %d, want 1", n)
	}
	var args string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args::text FROM river_job WHERE kind = 'verify_email'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(args, "vt1") || strings.Contains(args, "@") {
		t.Errorf("job args %s carry a secret or an email address", args)
	}

	if err := e.jobs.SendVerificationEmail(ctx, identity.VerifyEmailArgs{TenantID: e.tenant, UserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if len(sent) != 1 || sent[0].To != "new@kopi.test" || !strings.Contains(sent[0].Text, "https://app.example.test/verify-email?token=") {
		t.Fatalf("sent = %+v", sent)
	}
	if !strings.Contains(sent[0].Subject, "Verifikasi") {
		t.Errorf("subject %q is not in the user's locale (id-ID)", sent[0].Subject)
	}
	token := linkToken.FindStringSubmatch(sent[0].Text)[1]

	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "new@kopi.test", Password: password}); !errors.Is(err, identity.ErrEmailNotVerified) {
		t.Fatalf("before verifying: err = %v", err)
	}
	if err := e.svc.VerifyEmail(ctx, "vt1."+e.tenant.String()+".AAAA"); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("bad token: err = %v, want ErrInvalidToken", err)
	}
	if err := e.svc.VerifyEmail(ctx, token); err != nil {
		t.Fatal(err)
	}
	e.login(t, "new@kopi.test")

	if err := e.svc.VerifyEmail(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("a link used twice: err = %v, want ErrInvalidToken", err)
	}

	// Already verified: the job does nothing.
	if err := e.jobs.SendVerificationEmail(ctx, identity.VerifyEmailArgs{TenantID: e.tenant, UserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.mail.Sent()); n != 1 {
		t.Errorf("emails after verification = %d, want 1", n)
	}
}

func TestEmailVerificationLinkExpires(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "new@kopi.test", false)
	if err := e.jobs.SendVerificationEmail(ctx, identity.VerifyEmailArgs{TenantID: e.tenant, UserID: u.ID}); err != nil {
		t.Fatal(err)
	}
	token := linkToken.FindStringSubmatch(e.mail.Sent()[0].Text)[1]

	e.clock.Advance(25 * time.Hour)
	if err := e.svc.VerifyEmail(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestResendVerification(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "new@kopi.test", false)
	e.member(t, "done@kopi.test", true)
	before := e.jobCount(t, "verify_email") // the one queued by CreateMember

	// Unknown and already verified addresses succeed and queue nothing.
	for _, email := range []string{"nobody@kopi.test", "done@kopi.test"} {
		if err := e.svc.ResendVerification(ctx, email); err != nil {
			t.Fatalf("ResendVerification(%s): %v", email, err)
		}
	}
	if n := e.jobCount(t, "verify_email"); n != before {
		t.Errorf("jobs = %d, want %d", n, before)
	}
	// A repeat inside five minutes is dropped by the job's uniqueness.
	for range 3 {
		if err := e.svc.ResendVerification(ctx, "new@kopi.test"); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.jobCount(t, "verify_email"); n != before {
		t.Errorf("jobs after repeats = %d, want %d (deduplicated)", n, before)
	}
}

func TestCreateMember(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", false)
	jobs := e.jobCount(t, "verify_email")

	// A duplicate email is a conflict and leaves no job behind: the job is part of the
	// transaction that rolled back.
	_, err := e.svc.CreateMember(ctx, identity.NewMember{
		TenantID: e.tenant, Email: "OWNER@kopi.test", Password: password, DisplayName: "Again",
	})
	if !errors.Is(err, kernel.ErrConflict) {
		t.Errorf("duplicate email: err = %v, want ErrConflict", err)
	}
	if n := e.jobCount(t, "verify_email"); n != jobs {
		t.Errorf("jobs = %d after a failed CreateMember, want %d", n, jobs)
	}

	for name, in := range map[string]identity.NewMember{
		"bad email":      {TenantID: e.tenant, Email: "nope", Password: password, DisplayName: "x"},
		"short password": {TenantID: e.tenant, Email: "a@kopi.test", Password: "short", DisplayName: "x"},
		"no name":        {TenantID: e.tenant, Email: "a@kopi.test", Password: password},
		"bad locale":     {TenantID: e.tenant, Email: "a@kopi.test", Password: password, DisplayName: "x", Locale: "fr"},
		"no tenant":      {Email: "a@kopi.test", Password: password, DisplayName: "x"},
		"display form":   {TenantID: e.tenant, Email: "A <a@kopi.test>", Password: password, DisplayName: "x"},
	} {
		if _, err := e.svc.CreateMember(ctx, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	if _, err := e.svc.CreateMember(ctx, identity.NewMember{
		TenantID: uuid.New(), Email: "a@kopi.test", Password: password, DisplayName: "x", EmailVerified: true,
	}); err == nil {
		t.Error("a member of a tenant that does not exist was created")
	}
}

func TestGetMe(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "owner@kopi.test", true)
	p, _ := e.svc.Authenticate(identity.AudienceTenant, e.login(t, "owner@kopi.test").AccessToken)

	me, err := e.svc.GetMe(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if me.User.ID != u.ID || me.User.Email != "owner@kopi.test" || !me.User.EmailVerified || !me.IsOwner || me.User.Locale != "id-ID" {
		t.Errorf("me = %+v", me)
	}
}

// Many requests presenting the same refresh token at once: one wins, the rest see a replay, and
// Postgres never reports a deadlock.
func TestConcurrentRefreshOfOneToken(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	sess := e.login(t, "owner@kopi.test")

	const n = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		wins    []identity.Session
		replays int
		others  []error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := e.svc.Refresh(ctx, sess.RefreshToken)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins = append(wins, s)
			case errors.Is(err, identity.ErrTokenReuse):
				replays++
			default:
				others = append(others, err)
			}
		}()
	}
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if len(wins) != 1 || replays != n-1 {
		t.Fatalf("wins = %d, replays = %d, want 1 and %d", len(wins), replays, n-1)
	}
	// The replays revoked the family, including the winner's new token.
	if _, err := e.svc.Refresh(ctx, wins[0].RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("winner's token after the replays: err = %v, want ErrInvalidToken", err)
	}
}

func TestPurgeExpiredTokens(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	e.login(t, "owner@kopi.test")

	count := func() (n int) {
		_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM refresh_token`).Scan(&n)
		return n
	}
	if err := e.jobs.PurgeExpiredTokens(ctx); err != nil || count() != 1 {
		t.Fatalf("a live token was purged (err %v, count %d)", err, count())
	}
	e.clock.Advance(30*24*time.Hour + 8*24*time.Hour) // expired, and past the one-week grace
	if err := e.jobs.PurgeExpiredTokens(ctx); err != nil || count() != 0 {
		t.Errorf("expired token kept (err %v, count %d)", err, count())
	}
}

// Row-level security and column grants on the identity tables, seen from the app role.
func TestIdentityIsolation(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "a@kopi.test", true)
	other := e.newTenant(t, "tea")
	e.memberIn(t, other, "b@tea.test", true)
	sessA := e.login(t, "a@kopi.test")

	inTenant := func(tenant uuid.UUID, sql string, args ...any) error {
		return kernel.TenantTx(ctx, e.d.App, tenant, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			rows.Close()
			return rows.Err()
		})
	}
	count := func(tenant uuid.UUID, table string) (n int) {
		_ = kernel.TenantTx(ctx, e.d.App, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
		})
		return n
	}

	// Each tenant sees only its own user, membership and tokens.
	for _, table := range []string{"user_account", "tenant_member", "refresh_token"} {
		if n := count(e.tenant, table); n != 1 {
			t.Errorf("%s as tenant A: %d rows, want 1", table, n)
		}
		if n := count(other, table); n != 1 && table != "refresh_token" {
			t.Errorf("%s as tenant B: %d rows, want 1", table, n)
		}
	}
	if n := count(other, "refresh_token"); n != 0 {
		t.Errorf("tenant B sees %d of tenant A's refresh tokens", n)
	}

	// No tenant context: no users at all.
	var n int
	if err := e.d.App.QueryRow(ctx, `SELECT count(*) FROM user_account`).Scan(&n); err != nil || n != 0 {
		t.Errorf("user_account without a tenant: %d rows, err %v", n, err)
	}

	// The app role cannot read password hashes or token hashes' owners' secrets, or change who an
	// account is.
	for _, sql := range []string{
		`SELECT password_hash FROM user_account`,
		`SELECT * FROM user_account`,
		`UPDATE user_account SET email = 'x@y.test'`,
		`UPDATE user_account SET password_hash = 'x'`,
		`UPDATE refresh_token SET expires_at = now() + interval '10 years'`,
		`DELETE FROM refresh_token`,
		`DELETE FROM user_account`,
		`DELETE FROM tenant_member`,
		`UPDATE email_verification SET expires_at = now()`,
	} {
		if err := inTenant(e.tenant, sql); !isPgCode(err, "42501") {
			t.Errorf("%s: err = %v, want 42501 insufficient_privilege", sql, err)
		}
	}

	// Writing a membership into another tenant is refused by row-level security.
	err := kernel.TenantTx(ctx, e.d.App, e.tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_member (tenant_id, user_id) VALUES ($1, $2)`, other, uuid.New())
		return err
	})
	if !isPgCode(err, "42501") {
		t.Errorf("cross-tenant membership insert: err = %v, want 42501", err)
	}

	// Tenant B cannot revoke tenant A's session by guessing ids.
	if err := e.svc.Logout(ctx, strings.Replace(sessA.RefreshToken, e.tenant.String(), other.String(), 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Refresh(ctx, sessA.RefreshToken); err != nil {
		t.Errorf("A's refresh token stopped working after a forged logout: %v", err)
	}
}

func isPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// The whole path: CreateMember queues a job in its transaction, a real river worker running as
// orion_platform picks it up, and the verification email arrives.
func TestVerificationEmailThroughRiverWorker(t *testing.T) {
	e, ctx := newEnv(t), context.Background()

	workers := river.NewWorkers()
	e.jobs.AddWorkers(workers)
	worker, err := river.NewClient(riverpgxv5.New(e.d.Platform), &river.Config{
		Queues:            map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 2}},
		Workers:           workers,
		FetchCooldown:     100 * time.Millisecond,
		FetchPollInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Stop(context.Background()) })

	e.member(t, "new@kopi.test", false)

	deadline := time.Now().Add(10 * time.Second)
	for len(e.mail.Sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no email after 10s; the worker did not run the job")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sent := e.mail.Sent(); len(sent) != 1 || sent[0].To != "new@kopi.test" {
		t.Errorf("sent = %+v", sent)
	}
	// The job is marked completed just after the email goes out.
	var state string
	for !time.Now().After(deadline) && state != "completed" {
		if err := e.d.Owner.QueryRow(ctx, `SELECT state FROM river_job WHERE kind = 'verify_email'`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if state != "completed" {
		t.Errorf("job state = %q, want completed", state)
	}
}
