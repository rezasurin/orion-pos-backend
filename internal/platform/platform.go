// Package platform is Orion's own back office: operator accounts with required two-factor sign-in,
// the platform audit log, and the operator actions that cross tenants (BACKEND_PLAN.md sections
// 4.3, 4.7 and 6.2; ADR 0008). It connects as orion_platform, and its tables are invisible to
// orion_app.
package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform/db"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

const (
	// AudienceOperator is the audience of operator session tokens; AudienceChallenge of the short
	// token that proves the password step and is exchanged for a session by a TOTP code.
	AudienceOperator  identity.Audience = "operator"
	AudienceChallenge identity.Audience = "operator_challenge"

	challengeTTL = 5 * time.Minute
	sessionTTL   = time.Hour
	totpPeriod   = 30
	recoveryN    = 10
)

var (
	// ErrInvalidCredentials covers an unknown operator, a wrong password, a wrong code and a
	// disabled account alike.
	ErrInvalidCredentials = errors.New("platform: invalid credentials")
	// ErrInvalidToken means a challenge or session token is bad or expired.
	ErrInvalidToken = errors.New("platform: invalid or expired token")
)

// Meta describes where a request came from, for the audit log.
type Meta struct{ IP, UserAgent string }

// Actor is who performs an operator action. OperatorID is nil only for bootstrapping the first
// operator.
type Actor struct {
	OperatorID *uuid.UUID
	Meta       Meta
}

// Deps are what Service needs.
type Deps struct {
	Pool         *pgxpool.Pool // a member of orion_platform
	Box          *kernel.Box   // encrypts TOTP seeds
	Keys         *identity.Keyring
	Hasher       *identity.Hasher
	Clock        kernel.Clock
	Entitlements *entitlements.Admin
	Tenants      *tenancy.Admin
	// PasswordCost overrides the argon2id cost; tests lower it.
	PasswordCost identity.ArgonParams
}

// Service is the platform module's API.
type Service struct {
	Deps
	dummyHash string
}

// NewService returns a Service.
func NewService(d Deps) (*Service, error) {
	if d.Clock == nil {
		d.Clock = kernel.SystemClock{}
	}
	if d.Hasher == nil {
		d.Hasher = identity.NewHasher(2)
	}
	if d.PasswordCost == (identity.ArgonParams{}) {
		d.PasswordCost = identity.PasswordParams
	}
	dummy, err := d.Hasher.Hash(context.Background(), "orion-dummy-operator", d.PasswordCost)
	if err != nil {
		return nil, err
	}
	return &Service{Deps: d, dummyHash: dummy}, nil
}

// Operator is an Orion staff account.
type Operator struct {
	ID            uuid.UUID
	Email         string
	TOTPConfirmed bool
	Disabled      bool
}

func toOperator(o db.Operator) Operator {
	return Operator{ID: o.ID, Email: o.Email, TOTPConfirmed: o.TotpConfirmedAt != nil, Disabled: o.DisabledAt != nil}
}

// Entry is one platform audit log line to write.
type Entry struct {
	Action     string
	TargetType string
	TargetID   *uuid.UUID
	TenantID   *uuid.UUID
	Before     any
	After      any
	Reason     string
}

func (s *Service) audit(ctx context.Context, tx pgx.Tx, a Actor, e Entry) error {
	if strings.TrimSpace(e.Reason) == "" {
		return fmt.Errorf("%w: a reason is required", kernel.ErrValidation)
	}
	enc := func(v any) ([]byte, error) {
		if v == nil {
			return nil, nil
		}
		return json.Marshal(v)
	}
	before, err := enc(e.Before)
	if err != nil {
		return err
	}
	after, err := enc(e.After)
	if err != nil {
		return err
	}
	var ip *netip.Addr
	if addr, err := netip.ParseAddr(a.Meta.IP); err == nil {
		ip = &addr
	}
	var ua *string
	if a.Meta.UserAgent != "" {
		u := a.Meta.UserAgent
		ua = &u
	}
	return db.New(tx).InsertAudit(ctx, db.InsertAuditParams{
		ID: kernel.NewID(), OperatorID: a.OperatorID, Action: e.Action, TargetType: e.TargetType,
		TargetID: e.TargetID, TenantID: e.TenantID, Before: before, After: after, Reason: e.Reason,
		Ip: ip, UserAgent: ua,
	})
}

// NewOperator is the one-time result of creating an operator. Nothing here can be shown again.
type NewOperator struct {
	Operator      Operator
	Password      string
	TOTPSecret    string // base32
	TOTPURI       string // otpauth:// URI for an authenticator app
	RecoveryCodes []string
}

// CreateOperator creates an operator with a generated password, a TOTP seed and ten recovery
// codes. Enrolment is confirmed by their first successful code. The first operator is created
// with a nil actor; every later one needs an existing operator as the actor.
func (s *Service) CreateOperator(ctx context.Context, a Actor, email, reason string) (NewOperator, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") || len(email) > 254 {
		return NewOperator{}, fmt.Errorf("%w: email is not valid", kernel.ErrValidation)
	}
	password, err := randomString(24)
	if err != nil {
		return NewOperator{}, err
	}
	hash, err := s.Hasher.Hash(ctx, password, s.PasswordCost)
	if err != nil {
		return NewOperator{}, err
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "Orion POS", AccountName: email, Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		return NewOperator{}, err
	}

	id := kernel.NewID()
	sealed, err := s.Box.Seal([]byte(key.Secret()), id[:])
	if err != nil {
		return NewOperator{}, err
	}
	codes := make([]string, recoveryN)
	hashes := make([][]byte, recoveryN)
	for i := range codes {
		if codes[i], err = randomCode(); err != nil {
			return NewOperator{}, err
		}
		hashes[i] = hashCode(codes[i])
	}

	var op db.Operator
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.CountOperators(ctx)
		if err != nil {
			return err
		}
		if (n == 0) != (a.OperatorID == nil) {
			return fmt.Errorf("%w: the first operator is created without an actor, later ones with one", kernel.ErrValidation)
		}
		if err := q.InsertOperator(ctx, db.InsertOperatorParams{ID: id, Email: email, PasswordHash: hash, TotpSecretEnc: sealed}); err != nil {
			return mapErr(err)
		}
		for _, h := range hashes {
			if err := q.InsertRecoveryCode(ctx, db.InsertRecoveryCodeParams{ID: kernel.NewID(), OperatorID: id, CodeHash: h}); err != nil {
				return err
			}
		}
		if op, err = q.GetOperator(ctx, id); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "operator.created", TargetType: "operator", TargetID: &id,
			After: map[string]any{"email": email}, Reason: reason,
		})
	})
	if err != nil {
		return NewOperator{}, err
	}
	return NewOperator{Operator: toOperator(op), Password: password, TOTPSecret: key.Secret(), TOTPURI: key.URL(), RecoveryCodes: codes}, nil
}

// OperatorByEmail finds an operator.
func (s *Service) OperatorByEmail(ctx context.Context, email string) (Operator, error) {
	o, err := db.New(s.Pool).GetOperatorByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operator{}, kernel.ErrNotFound
	}
	return toOperator(o), err
}

// Challenge is the result of the password step.
type Challenge struct {
	Token     string
	ExpiresAt time.Time
	// Enrolled is false until the operator's first successful code.
	Enrolled bool
}

type claims struct {
	jwt.RegisteredClaims
}

func (s *Service) token(aud identity.Audience, operatorID uuid.UUID, ttl time.Duration) (string, time.Time, error) {
	now := s.Clock.Now()
	exp := now.Add(ttl)
	t, err := s.Keys.SignClaims(claims{jwt.RegisteredClaims{
		Issuer: identity.Issuer, Subject: operatorID.String(), Audience: jwt.ClaimStrings{string(aud)},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp), ID: kernel.NewID().String(),
	}})
	return t, exp, err
}

// Login checks an operator's password and returns a short-lived challenge to be completed with a
// TOTP or recovery code. A session is never issued without the second factor. Unknown operators,
// wrong passwords and disabled accounts are indistinguishable.
func (s *Service) Login(ctx context.Context, email, password string) (Challenge, error) {
	o, err := db.New(s.Pool).GetOperatorByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Challenge{}, err
	}
	hash := s.dummyHash
	if found {
		hash = o.PasswordHash
	}
	ok, err := s.Hasher.Verify(ctx, password, hash)
	if err != nil {
		return Challenge{}, err
	}
	if !found || !ok || o.DisabledAt != nil {
		return Challenge{}, ErrInvalidCredentials
	}
	tok, exp, err := s.token(AudienceChallenge, o.ID, challengeTTL)
	if err != nil {
		return Challenge{}, err
	}
	return Challenge{Token: tok, ExpiresAt: exp, Enrolled: o.TotpConfirmedAt != nil}, nil
}

// Session is an operator's signed-in session.
type Session struct {
	OperatorID  uuid.UUID
	AccessToken string
	ExpiresAt   time.Time
}

// VerifySecondFactor completes a sign-in with either a TOTP code or a recovery code. A TOTP code
// works once (its time step must be newer than the last accepted), and a recovery code works once.
// The first successful code confirms enrolment.
func (s *Service) VerifySecondFactor(ctx context.Context, challenge, code, recoveryCode string, meta Meta) (Session, error) {
	var c claims
	if err := s.Keys.VerifyClaims(challenge, AudienceChallenge, s.Clock.Now, &c); err != nil {
		return Session{}, ErrInvalidToken
	}
	opID, err := uuid.Parse(c.Subject)
	if err != nil {
		return Session{}, ErrInvalidToken
	}

	now := s.Clock.Now()
	var failure error
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		failure = nil
		q := db.New(tx)
		o, err := q.GetOperatorForUpdate(ctx, opID)
		if err != nil {
			return mapNoRows(err, ErrInvalidCredentials)
		}
		if o.DisabledAt != nil {
			failure = ErrInvalidCredentials
			return nil
		}
		actor := Actor{OperatorID: &opID, Meta: meta}

		switch {
		case recoveryCode != "":
			n, err := q.UseRecoveryCode(ctx, db.UseRecoveryCodeParams{OperatorID: opID, CodeHash: hashCode(recoveryCode), Now: &now})
			if err != nil {
				return err
			}
			if n == 0 {
				failure = ErrInvalidCredentials
				return nil
			}
			return s.audit(ctx, tx, actor, Entry{Action: "operator.recovery_code_used", TargetType: "operator", TargetID: &opID, Reason: "sign in"})
		default:
			secret, err := s.Box.Open(o.TotpSecretEnc, opID[:])
			if err != nil {
				return err
			}
			step, ok := matchStep(string(secret), code, now, o.TotpLastStep)
			if !ok {
				failure = ErrInvalidCredentials
				return nil
			}
			if err := q.AdvanceTOTP(ctx, db.AdvanceTOTPParams{ID: opID, Step: step, Now: now}); err != nil {
				return err
			}
			if o.TotpConfirmedAt == nil {
				if err := s.audit(ctx, tx, actor, Entry{Action: "operator.totp_confirmed", TargetType: "operator", TargetID: &opID, Reason: "first sign in"}); err != nil {
					return err
				}
			}
			return s.audit(ctx, tx, actor, Entry{Action: "operator.signed_in", TargetType: "operator", TargetID: &opID, Reason: "sign in"})
		}
	})
	if err != nil {
		return Session{}, err
	}
	if failure != nil {
		return Session{}, failure
	}
	tok, exp, err := s.token(AudienceOperator, opID, sessionTTL)
	return Session{OperatorID: opID, AccessToken: tok, ExpiresAt: exp}, err
}

// matchStep finds the time step a code belongs to, allowing one step of clock drift either way,
// and accepts it only if it is newer than the last step used.
func matchStep(secret, code string, now time.Time, lastStep int64) (int64, bool) {
	cur := now.Unix() / totpPeriod
	for _, step := range []int64{cur, cur - 1, cur + 1} {
		if step <= lastStep {
			continue
		}
		want, err := totp.GenerateCodeCustom(secret, time.Unix(step*totpPeriod, 0), totp.ValidateOpts{Period: totpPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(code))) == 1 {
			return step, true
		}
	}
	return 0, false
}

// Authenticate verifies an operator session token and that the account is still enabled.
func (s *Service) Authenticate(ctx context.Context, token string) (Operator, error) {
	var c claims
	if err := s.Keys.VerifyClaims(token, AudienceOperator, s.Clock.Now, &c); err != nil {
		return Operator{}, ErrInvalidToken
	}
	id, err := uuid.Parse(c.Subject)
	if err != nil {
		return Operator{}, ErrInvalidToken
	}
	o, err := db.New(s.Pool).GetOperator(ctx, id)
	if err != nil || o.DisabledAt != nil || o.TotpConfirmedAt == nil {
		return Operator{}, ErrInvalidToken
	}
	return toOperator(o), nil
}

// AuditEntry is a stored platform audit log line.
type AuditEntry struct {
	ID         uuid.UUID
	OperatorID *uuid.UUID
	Action     string
	TargetType string
	TargetID   *uuid.UUID
	TenantID   *uuid.UUID
	Before     json.RawMessage
	After      json.RawMessage
	Reason     string
	IP         string
	UserAgent  *string
	CreatedAt  time.Time
}

// ListAuditLog returns a page of the audit log, newest first. page.After is the id of the last
// entry of the previous page, so the next page holds older entries. With tenantID set, only the
// entries about that business.
func (s *Service) ListAuditLog(ctx context.Context, page kernel.Page, tenantID *uuid.UUID) (kernel.Paged[AuditEntry], error) {
	before := page.After
	if before == uuid.Nil {
		before = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	}
	rows, err := db.New(s.Pool).ListAudit(ctx, db.ListAuditParams{Before: before, TenantID: tenantID, PageSize: page.Fetch()})
	if err != nil {
		return kernel.Paged[AuditEntry]{}, err
	}
	paged := kernel.Trim(page, rows, func(r db.ListAuditRow) uuid.UUID { return r.ID })
	out := kernel.Paged[AuditEntry]{Next: paged.Next, Items: make([]AuditEntry, len(paged.Items))}
	for i, r := range paged.Items {
		out.Items[i] = AuditEntry{
			ID: r.ID, OperatorID: r.OperatorID, Action: r.Action, TargetType: r.TargetType, TargetID: r.TargetID,
			TenantID: r.TenantID, Before: r.Before, After: r.After, Reason: r.Reason, IP: r.Ip, UserAgent: r.UserAgent, CreatedAt: r.CreatedAt,
		}
	}
	return out, nil
}

// SetOverride sets a tenant entitlement override and audits it, before and after, in one
// transaction.
func (s *Service) SetOverride(ctx context.Context, a Actor, tenantSlug string, key entitlements.Key, value int64, expires *time.Time, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		before, err := s.Entitlements.GetOverrideTx(ctx, tx, t.ID, key)
		if err != nil {
			return err
		}
		if err := s.Entitlements.SetOverrideTx(ctx, tx, entitlements.Override{
			TenantID: t.ID, Key: key, Value: value, Reason: reason, ExpiresAt: expires, OperatorID: a.OperatorID,
		}); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "entitlement.override_set", TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID,
			Before: overrideView(key, before), After: map[string]any{"key": key, "value": value, "expires_at": expires}, Reason: reason,
		})
	})
}

// ClearOverride removes an override and audits it.
func (s *Service) ClearOverride(ctx context.Context, a Actor, tenantSlug string, key entitlements.Key, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		before, err := s.Entitlements.GetOverrideTx(ctx, tx, t.ID, key)
		if err != nil {
			return err
		}
		if before == nil {
			return fmt.Errorf("%w: no override for %s", kernel.ErrNotFound, key)
		}
		if _, err := s.Entitlements.ClearOverrideTx(ctx, tx, t.ID, key); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: "entitlement.override_cleared", TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID,
			Before: overrideView(key, before), Reason: reason,
		})
	})
}

func overrideView(key entitlements.Key, o *entitlements.Override) any {
	if o == nil {
		return nil
	}
	return map[string]any{"key": key, "value": o.Value, "expires_at": o.ExpiresAt}
}

// SetTenantSuspended suspends or reinstates a tenant and audits it.
func (s *Service) SetTenantSuspended(ctx context.Context, a Actor, tenantSlug string, suspended bool, reason string) error {
	action := "tenant.reinstated"
	if suspended {
		action = "tenant.suspended"
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		t, err := s.Tenants.Find(ctx, tx, tenantSlug)
		if err != nil {
			return err
		}
		if (t.SuspendedAt != nil) == suspended {
			return fmt.Errorf("%w: tenant %s is already %s", kernel.ErrConflict, tenantSlug, map[bool]string{true: "suspended", false: "active"}[suspended])
		}
		if err := s.Tenants.SetSuspended(ctx, tx, t.ID, suspended, s.Clock.Now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, a, Entry{
			Action: action, TargetType: "tenant", TargetID: &t.ID, TenantID: &t.ID,
			Before: map[string]any{"suspended": !suspended}, After: map[string]any{"suspended": suspended}, Reason: reason,
		})
	})
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:n], nil
}

// randomCode is an 80-bit recovery code, shown as four groups of four base32 characters.
func randomCode() (string, error) {
	s, err := randomString(16)
	if err != nil {
		return "", err
	}
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16], nil
}

func hashCode(code string) []byte {
	norm := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
	sum := sha256.Sum256([]byte(norm))
	return sum[:]
}

func mapNoRows(err, as error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return as
	}
	return err
}

func mapErr(err error) error {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return fmt.Errorf("%w: an operator with that email exists", kernel.ErrConflict)
	}
	return err
}
