package identity_test

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

const newPassword = "a different long password"

var resetLinkToken = regexp.MustCompile(`/reset-password\?token=(\S+)`)

// requestReset asks for a reset, runs the queued job like the worker would and returns the token
// from the email.
func (e *env) requestReset(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()
	before := len(e.mail.Sent())
	if err := e.svc.RequestPasswordReset(ctx, email); err != nil {
		t.Fatal(err)
	}
	var tenantID, userID string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args->>'tenant_id', args->>'user_id' FROM river_job
		WHERE kind = 'password_reset_email' ORDER BY id DESC LIMIT 1`).Scan(&tenantID, &userID); err != nil {
		t.Fatalf("no reset email was queued: %v", err)
	}
	if err := e.jobs.SendPasswordResetEmail(ctx, identity.PasswordResetArgs{TenantID: uuid.MustParse(tenantID), UserID: uuid.MustParse(userID)}); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if len(sent) != before+1 {
		t.Fatalf("sent %d emails, want 1 more", len(sent)-before)
	}
	last := sent[len(sent)-1]
	m := resetLinkToken.FindStringSubmatch(last.Text)
	if last.To != strings.ToLower(email) || m == nil {
		t.Fatalf("email = %+v", last)
	}
	return m[1]
}

func TestPasswordResetEndToEnd(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)

	// The job holds ids only; the secret exists only in the email.
	if err := e.svc.RequestPasswordReset(ctx, "Owner@Kopi.test"); err != nil {
		t.Fatal(err)
	}
	var args string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args::text FROM river_job WHERE kind = 'password_reset_email'`).Scan(&args); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(args, "pr1") || strings.Contains(args, "@") {
		t.Errorf("job args %s carry a secret or an email address", args)
	}

	token := e.requestReset(t, "owner@kopi.test")
	if sent := e.mail.Sent(); !strings.Contains(sent[len(sent)-1].Subject, "Atur ulang") {
		t.Errorf("subject %q is not in the user's locale (id-ID)", sent[len(sent)-1].Subject)
	}

	// Until the token is used the old password still works.
	old := e.login(t, "owner@kopi.test")

	if err := e.svc.ResetPassword(ctx, token, newPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password}); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Errorf("old password after reset: err = %v, want ErrInvalidCredentials", err)
	}
	fresh, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: newPassword})
	if err != nil {
		t.Fatalf("new password: %v", err)
	}

	// The session that existed before the reset is over; the new one works.
	if _, err := e.svc.Refresh(ctx, old.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("refresh with a pre-reset token: err = %v, want ErrInvalidToken", err)
	}
	if _, err := e.svc.Refresh(ctx, fresh.RefreshToken); err != nil {
		t.Errorf("refresh with a post-reset token: %v", err)
	}

	// The user is told, and it is audited.
	if n := e.jobCount(t, "password_changed_notice"); n != 1 {
		t.Fatalf("password changed notices = %d, want 1", n)
	}
	var user string
	if err := e.d.Owner.QueryRow(ctx, `SELECT args->>'user_id' FROM river_job WHERE kind = 'password_changed_notice'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := e.jobs.SendPasswordChangedEmail(ctx, identity.PasswordChangedArgs{UserID: uuid.MustParse(user)}); err != nil {
		t.Fatal(err)
	}
	sent := e.mail.Sent()
	if last := sent[len(sent)-1]; last.To != "owner@kopi.test" || !strings.Contains(last.Text, "/forgot-password") || strings.Contains(last.Text, "token") {
		t.Errorf("notice = %+v", last)
	}
	var audits int
	if err := e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM tenant_audit_log WHERE action = 'user.password_reset'`).Scan(&audits); err != nil || audits != 1 {
		t.Errorf("audit entries = %d (%v), want 1", audits, err)
	}
}

func TestAResetLinkWorksOnce(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")
	if err := e.svc.ResetPassword(ctx, token, newPassword); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResetPassword(ctx, token, "yet another password"); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("second use: err = %v, want ErrInvalidToken", err)
	}
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: newPassword}); err != nil {
		t.Errorf("the second use changed the password: %v", err)
	}
}

func TestUsingOneResetLinkRetiresTheOthers(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	first := e.requestReset(t, "owner@kopi.test")
	e.clock.Advance(6 * time.Minute) // past the five minute de-duplication
	second := e.requestReset(t, "owner@kopi.test")
	if first == second {
		t.Fatal("two reset emails carried the same token")
	}
	if err := e.svc.ResetPassword(ctx, second, newPassword); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResetPassword(ctx, first, "something else entirely"); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("the older link after a reset: err = %v, want ErrInvalidToken", err)
	}
}

func TestAResetLinkExpires(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")
	e.clock.Advance(time.Hour + time.Minute)
	if err := e.svc.ResetPassword(ctx, token, newPassword); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("an expired link: err = %v, want ErrInvalidToken", err)
	}
	e.login(t, "owner@kopi.test") // nothing changed
}

func TestResetRejectsBadTokensAndWeakPasswordsWithoutSpendingTheLink(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")

	for name, bad := range map[string]string{
		"empty":          "",
		"garbage":        "nonsense",
		"wrong prefix":   strings.Replace(token, "pr1.", "vt1.", 1),
		"another secret": "pr1." + e.tenant.String() + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"another tenant": "pr1." + uuid.NewString() + "." + token[strings.LastIndex(token, ".")+1:],
	} {
		if err := e.svc.ResetPassword(ctx, bad, newPassword); !errors.Is(err, identity.ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
	if err := e.svc.ResetPassword(ctx, token, "short"); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("a weak password: err = %v, want a validation error", err)
	}
	// A weak password did not use up the link.
	if err := e.svc.ResetPassword(ctx, token, newPassword); err != nil {
		t.Errorf("the link was spent by a rejected password: %v", err)
	}
}

func TestAskingForAResetRevealsNothingAndIsDeduplicated(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	for _, email := range []string{"nobody@kopi.test", "NOBODY@kopi.test", "not even an address"} {
		if err := e.svc.RequestPasswordReset(ctx, email); err != nil {
			t.Errorf("unknown %q: err = %v, want nil: the answer must not depend on the account", email, err)
		}
	}
	if n := e.jobCount(t, "password_reset_email"); n != 0 {
		t.Errorf("%d reset emails queued for unknown addresses", n)
	}
	for range 4 {
		if err := e.svc.RequestPasswordReset(ctx, "owner@kopi.test"); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.jobCount(t, "password_reset_email"); n != 1 {
		t.Errorf("reset emails queued = %d after four requests, want 1 (no inbox flooding)", n)
	}
}

// Proving control of the inbox proves the address, so a reset also verifies it.
func TestResetVerifiesTheAddress(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "new@kopi.test", false)
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "new@kopi.test", Password: password}); !errors.Is(err, identity.ErrEmailNotVerified) {
		t.Fatalf("before: err = %v", err)
	}
	if err := e.svc.ResetPassword(ctx, e.requestReset(t, "new@kopi.test"), newPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "new@kopi.test", Password: newPassword}); err != nil {
		t.Errorf("after: %v", err)
	}
}

// One password, every business: a reset signs the user out of all of them.
func TestResetEndsSessionsInEveryBusiness(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	u := e.member(t, "owner@kopi.test", true)
	second := e.newTenant(t, "tea")
	e.d.Exec(t, `INSERT INTO tenant_member (tenant_id, user_id) VALUES ($1, $2)`, second, u.ID)
	inFirst, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password, TenantID: e.tenant})
	if err != nil {
		t.Fatal(err)
	}
	inSecond, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: password, TenantID: second})
	if err != nil {
		t.Fatal(err)
	}

	if err := e.svc.ResetPassword(ctx, e.requestReset(t, "owner@kopi.test"), newPassword); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]identity.Session{"first business": inFirst, "second business": inSecond} {
		if _, err := e.svc.Refresh(ctx, s.RefreshToken); !errors.Is(err, identity.ErrInvalidToken) {
			t.Errorf("%s: a session from before the reset survived: err = %v", name, err)
		}
	}
	if _, err := e.svc.Login(ctx, identity.LoginInput{Email: "owner@kopi.test", Password: newPassword, TenantID: second}); err != nil {
		t.Errorf("signing in again to the second business: %v", err)
	}
}

func TestResetFromTheLinkOfABusinessTheUserLeftFails(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")
	e.d.Exec(t, `DELETE FROM tenant_member WHERE tenant_id = $1`, e.tenant)
	if err := e.svc.ResetPassword(ctx, token, newPassword); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestPurgeRemovesOldResetLinks(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	e.requestReset(t, "owner@kopi.test")
	e.clock.Advance(10 * 24 * time.Hour)
	if err := e.jobs.PurgeExpiredTokens(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM password_reset`).Scan(&n); err != nil || n != 0 {
		t.Errorf("password_reset rows after the purge = %d (%v), want 0", n, err)
	}
}

// The app role can read the stored hashes, so the database function must take the secret and hash
// it itself: with the hash in hand, or from another business, nothing changes.
func TestTheDatabaseOnlyAcceptsTheSecretFromTheEmail(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")

	var hash []byte
	if err := e.d.Owner.QueryRow(ctx, `SELECT token_hash FROM password_reset`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	call := func(tenant uuid.UUID, secret []byte) *uuid.UUID {
		var got *uuid.UUID
		err := kernel.TenantTx(ctx, e.d.App, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT auth_reset_password($1, $2, 'taken-over', now())`, e.tenant, secret).Scan(&got)
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if call(e.tenant, hash) != nil {
		t.Fatal("the stored hash worked as the secret: reading the table would be enough to take over an account")
	}
	// The real secret, but from the context of another business, is refused too.
	secret, err := base64.RawURLEncoding.DecodeString(token[strings.LastIndex(token, ".")+1:])
	if err != nil {
		t.Fatal(err)
	}
	if call(e.newTenant(t, "tea"), secret) != nil {
		t.Fatal("the real secret worked from another business's context")
	}
	var hashAfter string
	if err := e.d.Owner.QueryRow(ctx, `SELECT password_hash FROM user_account`).Scan(&hashAfter); err != nil || hashAfter == "taken-over" {
		t.Fatalf("the password was changed without the secret (%v)", err)
	}
	// The real token still works afterwards: those attempts spent nothing.
	if err := e.svc.ResetPassword(ctx, token, newPassword); err != nil {
		t.Errorf("the real link: %v", err)
	}
}

func TestOnlyOneOfManyConcurrentUsesOfALinkWins(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true)
	token := e.requestReset(t, "owner@kopi.test")

	const n = 6
	results := make(chan error, n)
	for i := range n {
		go func() { results <- e.svc.ResetPassword(ctx, token, newPassword+strings.Repeat("x", i)) }()
	}
	wins := 0
	for range n {
		err := <-results
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, identity.ErrInvalidToken):
			t.Errorf("a losing use failed with %v, want ErrInvalidToken", err)
		}
	}
	if wins != 1 {
		t.Errorf("%d uses of one link succeeded, want exactly 1", wins)
	}
	if n := e.jobCount(t, "password_changed_notice"); n != 1 {
		t.Errorf("password changed notices = %d, want 1", n)
	}
}
