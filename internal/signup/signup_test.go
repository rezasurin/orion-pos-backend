package signup

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/notify"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

const password = "correct horse battery"

type env struct {
	d    *testdb.DB
	svc  *Service
	ids  *identity.Service
	jobs *identity.Jobs
	mail *notify.MemorySender
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	client, err := river.NewClient(riverpgxv5.New(d.App), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	tenants := tenancy.NewService(d.App)
	ids, err := identity.NewService(identity.Deps{
		Entitlements: entitlements.NewResolver(d.App, nil), Pool: d.App, Jobs: client, Gate: tenants.CheckActive,
		TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	mail := &notify.MemorySender{}
	return &env{
		d: d, ids: ids, mail: mail,
		svc: NewService(Deps{Pool: d.App, Identity: ids, Tenancy: tenants}),
		jobs: identity.NewJobs(identity.JobsDeps{
			Platform: d.Platform, Sender: mail, PublicURL: "https://app.example.test",
		}),
	}
}

func form() Input {
	return Input{BusinessName: "Kopi Senja", OwnerName: "Sari", Email: "Sari@Kopi.test", Password: password}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.d.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) tenants(t *testing.T) int { return e.count(t, `SELECT count(*) FROM tenant`) }

func (e *env) jobCount(t *testing.T, kind string) int {
	return e.count(t, `SELECT count(*) FROM river_job WHERE kind = $1`, kind)
}

func TestSignUpCreatesTheWholeBusinessAndTheOwnerCanSignInAfterVerifying(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	if err := e.svc.SignUp(ctx, form()); err != nil {
		t.Fatal(err)
	}

	// Everything exists, once: the business, its roles, its outlet, its owner.
	var name, slug, plan string
	if err := e.d.Owner.QueryRow(ctx, `SELECT t.name, t.slug, p.code FROM tenant t JOIN plan p ON p.id = t.plan_id`).Scan(&name, &slug, &plan); err != nil {
		t.Fatal(err)
	}
	if name != "Kopi Senja" || plan != "early_access" || !regexp.MustCompile(`^kopi-senja-[a-z0-9]{4}$`).MatchString(slug) {
		t.Errorf("tenant = %q slug %q plan %q", name, slug, plan)
	}
	if n := e.count(t, `SELECT count(*) FROM role WHERE is_system AND name IN ('Owner', 'Manager', 'Cashier', 'Kitchen')`); n != 4 {
		t.Errorf("system roles = %d, want Owner, Manager, Cashier and Kitchen", n)
	}
	var outletName, code, tz string
	if err := e.d.Owner.QueryRow(ctx, `SELECT o.name, o.code, s.timezone FROM outlet o JOIN outlet_settings s ON s.outlet_id = o.id`).Scan(&outletName, &code, &tz); err != nil {
		t.Fatal(err)
	}
	if outletName != "Kopi Senja" || code != "KOPI1" || tz != "Asia/Jakarta" {
		t.Errorf("outlet = %q %q %q", outletName, code, tz)
	}
	var email string
	var owner, verified bool
	if err := e.d.Owner.QueryRow(ctx, `SELECT u.email, m.is_owner, u.email_verified_at IS NOT NULL
		FROM user_account u JOIN tenant_member m ON m.user_id = u.id`).Scan(&email, &owner, &verified); err != nil {
		t.Fatal(err)
	}
	if email != "sari@kopi.test" || !owner || verified {
		t.Errorf("owner = %q owner=%v verified=%v: want a normalized address, an owner, not yet verified", email, owner, verified)
	}
	if n := e.count(t, `SELECT count(*) FROM staff WHERE display_name = 'Sari'`); n != 1 {
		t.Errorf("owner staff records = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tenant_audit_log WHERE action = 'tenant.signed_up'`); n != 1 {
		t.Errorf("signup audit entries = %d", n)
	}

	// Signing in is refused until the link from the email is used.
	if _, err := e.ids.Login(ctx, identity.LoginInput{Email: "sari@kopi.test", Password: password}); !errors.Is(err, identity.ErrEmailNotVerified) {
		t.Fatalf("before verifying: err = %v, want ErrEmailNotVerified", err)
	}
	var tenantID, userID string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args->>'tenant_id', args->>'user_id' FROM river_job WHERE kind = 'verify_email'`).Scan(&tenantID, &userID); err != nil {
		t.Fatalf("no verification email was queued: %v", err)
	}
	var args identity.VerifyEmailArgs
	args.TenantID, args.UserID = uuid.MustParse(tenantID), uuid.MustParse(userID)
	if err := e.jobs.SendVerificationEmail(ctx, args); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if len(sent) != 1 || sent[0].To != "sari@kopi.test" {
		t.Fatalf("sent = %+v", sent)
	}
	token := regexp.MustCompile(`token=(\S+)`).FindStringSubmatch(sent[0].Text)[1]
	if err := e.ids.VerifyEmail(ctx, token); err != nil {
		t.Fatal(err)
	}
	sess, err := e.ids.Login(ctx, identity.LoginInput{Email: "sari@kopi.test", Password: password})
	if err != nil {
		t.Fatalf("after verifying: %v", err)
	}
	if sess.TenantID.String() != tenantID {
		t.Errorf("signed in to %v, want the new business %v", sess.TenantID, tenantID)
	}
}

func TestSignUpCreatesNothingWhenTheInputIsWrong(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*Input){
		"no business name":   func(in *Input) { in.BusinessName = "  " },
		"no owner name":      func(in *Input) { in.OwnerName = "" },
		"short password":     func(in *Input) { in.Password = "short" },
		"bad email":          func(in *Input) { in.Email = "not-an-email" },
		"bad outlet code":    func(in *Input) { in.OutletCode = "x" },
		"bad time zone":      func(in *Input) { in.Timezone = "Europe/Paris" },
		"bad locale":         func(in *Input) { in.Locale = "fr" },
		"huge business name": func(in *Input) { in.BusinessName = strings.Repeat("k", 500) },
	} {
		in := form()
		mutate(&in)
		if err := e.svc.SignUp(ctx, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: err = %v, want a validation error", name, err)
		}
	}
	if n := e.tenants(t); n != 0 {
		t.Errorf("%d tenants were left behind by failed sign-ups", n)
	}
	if n := e.count(t, `SELECT count(*) FROM user_account`); n != 0 {
		t.Errorf("%d users were left behind", n)
	}
	if n := e.jobCount(t, "verify_email"); n != 0 {
		t.Errorf("%d verification emails were queued for failed sign-ups", n)
	}
}

// The tenant and its owner are one transaction: when the owner cannot be created, the business
// must not exist.
func TestABusinessNeverExistsWithoutItsOwner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.SignUp(ctx, form()); err != nil {
		t.Fatal(err)
	}
	owner, err := e.ids.PrepareMember(ctx, identity.NewMember{
		TenantID: kernel.NewID(), Email: "sari@kopi.test", Password: password, DisplayName: "Sari", IsOwner: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = kernel.TenantTx(ctx, e.d.App, owner.TenantID, func(tx pgx.Tx) error {
		if _, _, err := e.svc.Tenancy.CreateTenantIn(ctx, tx, owner.TenantID, tenancy.NewTenant{
			Name: "Second", Slug: "second-0001", Outlet: tenancy.NewOutlet{Name: "Second", Code: "SEC1"},
		}); err != nil {
			return err
		}
		_, err := e.svc.Identity.CreateMemberIn(ctx, tx, owner) // the address is taken
		return err
	})
	if !errors.Is(err, identity.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	if n := e.tenants(t); n != 1 {
		t.Errorf("tenants = %d, want only the first: the second was half created", n)
	}
	if n := e.count(t, `SELECT count(*) FROM tenant WHERE slug = 'second-0001'`) + e.count(t, `SELECT count(*) FROM outlet WHERE code = 'SEC1'`); n != 0 {
		t.Error("the second business's rows survived the failed transaction")
	}
}

func TestSigningUpAgainWithAKnownAddressCreatesNothingAndSaysNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.svc.SignUp(ctx, form()); err != nil {
		t.Fatal(err)
	}

	// Unverified: the verification link is sent again (the job is deduplicated for five minutes).
	again := form()
	again.BusinessName = "Another Name"
	if err := e.svc.SignUp(ctx, again); err != nil {
		t.Fatalf("a repeat sign-up must look like any other, got %v", err)
	}
	if n := e.tenants(t); n != 1 {
		t.Fatalf("tenants = %d after a second sign-up with the same address, want 1", n)
	}
	if n := e.jobCount(t, "account_exists_notice"); n != 0 {
		t.Errorf("an unverified account got an 'account exists' notice")
	}

	// Verified: the owner is told, once an hour.
	e.d.Exec(t, `UPDATE user_account SET email_verified_at = now()`)
	for range 3 {
		if err := e.svc.SignUp(ctx, again); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.tenants(t); n != 1 {
		t.Fatalf("tenants = %d", n)
	}
	if n := e.jobCount(t, "account_exists_notice"); n != 1 {
		t.Errorf("account exists notices = %d after three attempts, want 1 (no inbox flooding)", n)
	}
	var userID string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args->>'user_id' FROM river_job WHERE kind = 'account_exists_notice'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := e.jobs.SendAccountExistsEmail(ctx, identity.AccountExistsArgs{UserID: uuid.MustParse(userID)}); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if len(sent) != 1 || sent[0].To != "sari@kopi.test" || !strings.Contains(sent[0].Text, "https://app.example.test/login") {
		t.Fatalf("sent = %+v", sent)
	}
	if strings.Contains(sent[0].Text, "token") {
		t.Error("the notice carries a link that does something; it must only point at sign-in")
	}
}

// Many people double-clicking, or a bot retrying, with the same address: one business.
func TestConcurrentSignUpsWithOneAddressCreateOneBusiness(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- e.svc.SignUp(ctx, form())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent sign-up failed instead of looking like any other: %v", err)
		}
	}
	if n := e.tenants(t); n != 1 {
		t.Errorf("tenants = %d, want exactly 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM user_account`); n != 1 {
		t.Errorf("users = %d, want exactly 1", n)
	}
}

func TestATakenSlugIsRetriedWithAnotherSuffix(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.d.Exec(t, `INSERT INTO tenant (id, name, slug, plan_id) SELECT gen_random_uuid(), 'x', 'kopi-senja-aaaa', id FROM plan WHERE code = 'early_access'`)
	suffixes := []string{"aaaa", "aaaa", "bbbb"} // the first two collide
	var mu sync.Mutex
	newSuffix = func(int) string {
		mu.Lock()
		defer mu.Unlock()
		s := suffixes[0]
		if len(suffixes) > 1 {
			suffixes = suffixes[1:]
		}
		return s
	}
	t.Cleanup(func() { newSuffix = randomSuffix })

	if err := e.svc.SignUp(ctx, form()); err != nil {
		t.Fatalf("a slug collision should be retried, got %v", err)
	}
	if n := e.count(t, `SELECT count(*) FROM tenant WHERE slug = 'kopi-senja-bbbb'`); n != 1 {
		t.Error("the business was not created under the retried slug")
	}

	// If every attempt collides, the sign-up fails cleanly rather than looping.
	newSuffix = func(int) string { return "aaaa" }
	other := form()
	other.Email = "other@kopi.test"
	if err := e.svc.SignUp(ctx, other); !errors.Is(err, tenancy.ErrSlugTaken) {
		t.Errorf("err = %v, want ErrSlugTaken after the attempts run out", err)
	}
	if n := e.count(t, `SELECT count(*) FROM user_account WHERE email = 'other@kopi.test'`); n != 0 {
		t.Error("a failed sign-up left a user behind")
	}
}

func TestSlugAndOutletCodeDerivation(t *testing.T) {
	for in, want := range map[string]string{
		"Kopi Senja":            "kopi-senja",
		"  Warung  Bu  Ani!! ":  "warung-bu-ani",
		"Café Été":              "caf-t", // non-ASCII letters are dropped, not transliterated
		"北京烤鸭":                  "bisnis",
		"---":                   "bisnis",
		"A&B 2":                 "a-b-2",
		strings.Repeat("x", 90): strings.Repeat("x", maxSlugBase),
	} {
		if got := slugBase(in); got != want {
			t.Errorf("slugBase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"Kopi Senja": "KOPI1", "Ab": "AB1", "A": "A1", "7 Eleven": "7ELE1", "北京": "OUT1", "": "OUT1",
	} {
		if got := deriveOutletCode(in); got != want {
			t.Errorf("deriveOutletCode(%q) = %q, want %q", in, got, want)
		}
	}
}
