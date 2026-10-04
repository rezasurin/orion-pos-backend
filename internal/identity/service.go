// Package identity owns users, sessions, staff, roles and devices: who is calling and what they
// may do. Other modules use this package's Service and types, never the generated queries in ./db.
package identity

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Locales the server can write emails and messages in.
var Locales = []string{"id-ID", "en"}

const (
	minPasswordLen = 10
	maxPasswordLen = 128
)

// Config holds token lifetimes.
type Config struct {
	AccessTTL       time.Duration // user access tokens
	RefreshTTL      time.Duration // user refresh tokens
	VerificationTTL time.Duration // email verification links
	DeviceAccessTTL time.Duration // device access tokens
}

// DefaultConfig is BACKEND_PLAN.md section 4.3: 15 minute access tokens, 30 day refresh tokens.
func DefaultConfig() Config {
	return Config{
		AccessTTL:       15 * time.Minute,
		RefreshTTL:      30 * 24 * time.Hour,
		VerificationTTL: 24 * time.Hour,
		DeviceAccessTTL: 30 * time.Minute,
	}
}

// TenantGate says whether a tenant may sign in or be used, for example not suspended. It is
// supplied by the caller so identity does not depend on the tenancy module.
type TenantGate func(ctx context.Context, tenantID uuid.UUID) error

// JobInserter is the part of a river client the service needs: inserting a job inside the
// caller's transaction, so the job exists if and only if the business write committed.
type JobInserter interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Deps are what NewService needs.
type Deps struct {
	Pool       *pgxpool.Pool // a member of orion_app
	Clock      kernel.Clock
	TenantKeys *Keyring
	DeviceKeys *Keyring
	Hasher     *Hasher
	Jobs       JobInserter
	Gate       TenantGate // optional
	// Entitlements enforces plan limits (staff, devices) when creating things. Nil means no limits,
	// which is only right for tests and tooling that deliberately bypass them.
	Entitlements *entitlements.Resolver
	Config       Config
	// PasswordCost overrides the argon2id cost for new passwords. Leave it zero in production;
	// tests lower it to stay fast.
	PasswordCost ArgonParams
}

// Service is the identity module's API.
type Service struct {
	Deps
	// dummyHash is checked when an email is unknown, so a wrong email costs as much as a wrong
	// password.
	dummyHash string
}

// NewService returns a Service. It computes one password hash at start-up, so it can fail.
func NewService(d Deps) (*Service, error) {
	if d.Clock == nil {
		d.Clock = kernel.SystemClock{}
	}
	if d.Hasher == nil {
		d.Hasher = NewHasher(4)
	}
	if d.Config == (Config{}) {
		d.Config = DefaultConfig()
	}
	if d.PasswordCost == (ArgonParams{}) {
		d.PasswordCost = PasswordParams
	}
	dummy, err := d.Hasher.Hash(context.Background(), "orion-dummy-password", d.PasswordCost)
	if err != nil {
		return nil, err
	}
	return &Service{Deps: d, dummyHash: dummy}, nil
}

// User is an account, as tenant-side code may see it.
type User struct {
	ID            uuid.UUID
	Email         string
	EmailVerified bool
	Locale        string
	CreatedAt     time.Time
}

// Session is what a successful login or refresh returns.
type Session struct {
	TenantID         uuid.UUID
	UserID           uuid.UUID
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// LoginInput is an email and password, plus the business to sign in to when the account belongs
// to more than one.
type LoginInput struct {
	Email    string
	Password string
	TenantID uuid.UUID // optional
}

type loginUser struct {
	ID              uuid.UUID
	PasswordHash    string
	EmailVerifiedAt *time.Time
	Locale          string
	TenantIDs       []uuid.UUID
}

// findUserForLogin calls auth_find_user, the one lookup allowed before a tenant is known. It is
// not in the sqlc queries because sqlc cannot read the columns of a RETURNS TABLE function.
func (s *Service) findUserForLogin(ctx context.Context, email string) (loginUser, bool, error) {
	var u loginUser
	err := s.Pool.QueryRow(ctx, `
		SELECT f.id, f.password_hash, f.email_verified_at, f.locale, f.tenant_ids
		FROM auth_find_user($1::text) AS f`, email,
	).Scan(&u.ID, &u.PasswordHash, &u.EmailVerifiedAt, &u.Locale, &u.TenantIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return loginUser{}, false, nil
	}
	return u, err == nil, err
}

// Login checks an email and password and starts a session. Unknown email and wrong password are
// indistinguishable to the caller, in the error and in the time taken.
func (s *Service) Login(ctx context.Context, in LoginInput) (Session, error) {
	email := normalizeEmail(in.Email)
	u, found, err := s.findUserForLogin(ctx, email)
	if err != nil {
		return Session{}, err
	}
	hash := s.dummyHash
	if found {
		hash = u.PasswordHash
	}
	ok, err := s.Hasher.Verify(ctx, in.Password, hash)
	if err != nil {
		return Session{}, err
	}
	if !found || !ok {
		return Session{}, ErrInvalidCredentials
	}
	if u.EmailVerifiedAt == nil {
		return Session{}, ErrEmailNotVerified
	}

	tenantID, err := chooseTenant(u.TenantIDs, in.TenantID)
	if err != nil {
		return Session{}, err
	}
	if err := s.gate(ctx, tenantID); err != nil {
		return Session{}, err
	}

	var sess Session
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.GetMember(ctx, db.GetMemberParams{TenantID: tenantID, UserID: u.ID}); err != nil {
			return mapNoRows(err, ErrInvalidCredentials)
		}
		var err error
		sess, err = s.issue(ctx, q, tenantID, u.ID, kernel.NewID())
		return err
	})
	return sess, err
}

func chooseTenant(memberOf []uuid.UUID, want uuid.UUID) (uuid.UUID, error) {
	switch {
	case len(memberOf) == 0:
		return uuid.Nil, ErrNoTenant
	case want != uuid.Nil:
		for _, id := range memberOf {
			if id == want {
				return id, nil
			}
		}
		return uuid.Nil, ErrInvalidCredentials
	case len(memberOf) == 1:
		return memberOf[0], nil
	default:
		return uuid.Nil, &TenantRequiredError{TenantIDs: memberOf}
	}
}

// issue creates a refresh token in the given family and an access token for a user.
func (s *Service) issue(ctx context.Context, q *db.Queries, tenantID, userID, familyID uuid.UUID) (Session, error) {
	now := s.Clock.Now()
	refresh, hash := mintOpaque(prefixRefresh, tenantID)
	refreshExp := now.Add(s.Config.RefreshTTL)
	if err := q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		ID: kernel.NewID(), TenantID: tenantID, UserID: userID, FamilyID: familyID,
		TokenHash: hash, ExpiresAt: refreshExp,
	}); err != nil {
		return Session{}, err
	}
	accessExp := now.Add(s.Config.AccessTTL)
	access, err := s.TenantKeys.sign(accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   userID.String(),
			Audience:  jwt.ClaimStrings{string(AudienceTenant)},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExp),
			ID:        kernel.NewID().String(),
		},
		TenantID: tenantID.String(),
	})
	if err != nil {
		return Session{}, err
	}
	return Session{
		TenantID: tenantID, UserID: userID,
		AccessToken: access, AccessExpiresAt: accessExp,
		RefreshToken: refresh, RefreshExpiresAt: refreshExp,
	}, nil
}

// Refresh exchanges a refresh token for a new session. Each refresh token works once. Presenting
// one a second time means it was copied, so every token from the same login is revoked and
// ErrTokenReuse is returned.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (Session, error) {
	ids, hash, err := parseOpaque(prefixRefresh, 1, refreshToken)
	if err != nil {
		return Session{}, err
	}
	tenantID := ids[0]
	if err := s.gate(ctx, tenantID); err != nil {
		return Session{}, err
	}

	// Revoking a family must commit, so those paths return nil from the transaction and report
	// the problem afterwards; returning an error would roll the revocation back.
	var (
		sess    Session
		failure error
	)
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		sess, failure = Session{}, nil // the function may run again after a deadlock retry
		q := db.New(tx)
		now := s.Clock.Now()

		rt, err := q.GetRefreshTokenForUpdate(ctx, db.GetRefreshTokenForUpdateParams{TenantID: tenantID, TokenHash: hash})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		switch {
		case rt.UsedAt != nil:
			// Rotated already: someone is replaying it.
			failure = ErrTokenReuse
			return q.RevokeRefreshFamily(ctx, db.RevokeRefreshFamilyParams{TenantID: tenantID, FamilyID: rt.FamilyID, Now: &now})
		case rt.RevokedAt != nil || !rt.ExpiresAt.After(now):
			return ErrInvalidToken
		}
		// A user removed from the business loses their sessions at the next refresh at the latest.
		if _, err := q.GetMember(ctx, db.GetMemberParams{TenantID: tenantID, UserID: rt.UserID}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				failure = ErrInvalidToken
				return q.RevokeRefreshFamily(ctx, db.RevokeRefreshFamilyParams{TenantID: tenantID, FamilyID: rt.FamilyID, Now: &now})
			}
			return err
		}
		if err := q.MarkRefreshTokenUsed(ctx, db.MarkRefreshTokenUsedParams{ID: rt.ID, Now: &now}); err != nil {
			return err
		}
		sess, err = s.issue(ctx, q, tenantID, rt.UserID, rt.FamilyID)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	if failure != nil {
		return Session{}, failure
	}
	return sess, nil
}

// Logout revokes the session the refresh token belongs to. It succeeds for unknown or already
// revoked tokens, so it can always be retried.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	ids, hash, err := parseOpaque(prefixRefresh, 1, refreshToken)
	if err != nil {
		return nil //nolint:nilerr // logging out with garbage is not an error
	}
	tenantID := ids[0]
	return kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rt, err := q.GetRefreshTokenForUpdate(ctx, db.GetRefreshTokenForUpdateParams{TenantID: tenantID, TokenHash: hash})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := s.Clock.Now()
		return q.RevokeRefreshFamily(ctx, db.RevokeRefreshFamilyParams{TenantID: tenantID, FamilyID: rt.FamilyID, Now: &now})
	})
}

// Authenticate verifies a user or device access token and returns who it belongs to. It reads no
// database; use Access to check the user is still a member.
func (s *Service) Authenticate(aud Audience, bearer string) (Principal, error) {
	keys := s.TenantKeys
	if aud == AudienceDevice {
		keys = s.DeviceKeys
	}
	var c accessClaims
	if err := keys.verify(bearer, aud, s.Clock.Now, &c); err != nil {
		return Principal{}, err
	}
	tenantID, err := uuid.Parse(c.TenantID)
	if err != nil || tenantID == uuid.Nil {
		return Principal{}, ErrInvalidToken
	}
	p := Principal{TenantID: tenantID}
	switch aud {
	case AudienceTenant:
		p.Type = PrincipalUser
		if p.UserID, err = uuid.Parse(c.Subject); err != nil {
			return Principal{}, ErrInvalidToken
		}
	case AudienceDevice:
		p.Type = PrincipalDevice
		if p.DeviceID, err = uuid.Parse(c.DeviceID); err != nil {
			return Principal{}, ErrInvalidToken
		}
		if p.OutletID, err = uuid.Parse(c.OutletID); err != nil {
			return Principal{}, ErrInvalidToken
		}
	}
	return p, nil
}

// LoadAccess checks that the user is still a member of the tenant and returns what they may do.
// It runs on every request to a user route, so a removed member loses access at once instead of
// when their access token expires. One query, however many outlets and roles the user has.
func (s *Service) LoadAccess(ctx context.Context, p Principal) (Access, error) {
	var a Access
	err := kernel.TenantTx(ctx, s.Pool, p.TenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).GetMemberAccess(ctx, db.GetMemberAccessParams{TenantID: p.TenantID, UserID: p.UserID})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrInvalidToken // not a member (any more)
		}
		a = Access{IsOwner: rows[0].IsOwner}
		for _, r := range rows {
			if r.OutletID != nil && r.Permission != nil {
				a.grant(*r.OutletID, Permission(*r.Permission))
			}
		}
		return nil
	})
	return a, err
}

// Me is the signed-in user in their tenant.
type Me struct {
	User    User
	IsOwner bool
}

// GetMe returns the user behind a principal.
func (s *Service) GetMe(ctx context.Context, p Principal) (Me, error) {
	var me Me
	err := kernel.TenantTx(ctx, s.Pool, p.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		isOwner, err := q.GetMember(ctx, db.GetMemberParams{TenantID: p.TenantID, UserID: p.UserID})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		u, err := q.GetUser(ctx, p.UserID)
		if err != nil {
			return mapNoRows(err, kernel.ErrNotFound)
		}
		me = Me{
			IsOwner: isOwner,
			User: User{
				ID: u.ID, Email: u.Email, EmailVerified: u.EmailVerifiedAt != nil,
				Locale: u.Locale, CreatedAt: u.CreatedAt,
			},
		}
		return nil
	})
	return me, err
}

// NewMember describes a person to add to a tenant with an email login.
type NewMember struct {
	TenantID    uuid.UUID
	Email       string
	Password    string
	DisplayName string
	Locale      string // defaults to id-ID
	IsOwner     bool
	// EmailVerified skips the verification email, for accounts an operator or a seed creates.
	EmailVerified bool
	// OutletRoles are the roles a non-owner member holds. Owners need none: they may do anything.
	// The caller is trusted here (a command or signup); API handlers use CreateStaff's checks.
	OutletRoles []OutletRole
}

// CreateMember creates a user, adds them to a tenant and gives them a staff record (so they appear
// in the roster once they have a PIN). Unless the email is marked verified, a verification email
// is queued in the same transaction.
func (s *Service) CreateMember(ctx context.Context, in NewMember) (User, error) {
	m, err := s.PrepareMember(ctx, in)
	if err != nil {
		return User{}, err
	}
	var u User
	err = kernel.TenantTx(ctx, s.Pool, m.TenantID, func(tx pgx.Tx) error {
		var err error
		u, err = s.CreateMemberIn(ctx, tx, m)
		return err
	})
	return u, err
}

// PreparedMember is a NewMember that has been checked and whose password is hashed, ready for
// CreateMemberIn. Hashing is slow, so it is done before a transaction opens, not inside it.
type PreparedMember struct {
	NewMember
	hash string
}

// PrepareMember normalizes and validates in and hashes its password.
func (s *Service) PrepareMember(ctx context.Context, in NewMember) (PreparedMember, error) {
	in.Email = normalizeEmail(in.Email)
	if in.Locale == "" {
		in.Locale = Locales[0]
	}
	if err := validateNewMember(in); err != nil {
		return PreparedMember{}, err
	}
	hash, err := s.Hasher.Hash(ctx, in.Password, s.PasswordCost)
	if err != nil {
		return PreparedMember{}, err
	}
	return PreparedMember{NewMember: in, hash: hash}, nil
}

// CreateMemberIn creates the member inside the caller's transaction, which must have been opened
// for m.TenantID (kernel.TenantTx). Signup uses it to create a business and its owner together.
func (s *Service) CreateMemberIn(ctx context.Context, tx pgx.Tx, m PreparedMember) (User, error) {
	userID := kernel.NewID()
	now := s.Clock.Now()
	var verifiedAt *time.Time
	if m.EmailVerified {
		verifiedAt = &now
	}
	staffID := kernel.NewID()
	q := db.New(tx)
	if err := kernel.LockTenant(ctx, tx); err != nil {
		return User{}, err
	}
	if err := s.checkStaffLimit(ctx, tx, q, m.TenantID); err != nil {
		return User{}, err
	}
	if err := q.InsertUser(ctx, db.InsertUserParams{
		ID: userID, Email: m.Email, PasswordHash: m.hash, EmailVerifiedAt: verifiedAt, Locale: m.Locale,
	}); err != nil {
		return User{}, mapErr(err)
	}
	if err := q.InsertMember(ctx, db.InsertMemberParams{TenantID: m.TenantID, UserID: userID, IsOwner: m.IsOwner}); err != nil {
		return User{}, mapErr(err)
	}
	if err := q.InsertStaff(ctx, db.InsertStaffParams{
		ID: staffID, TenantID: m.TenantID, UserID: &userID, DisplayName: strings.TrimSpace(m.DisplayName),
	}); err != nil {
		return User{}, mapErr(err)
	}
	if err := insertAssignments(ctx, q, m.TenantID, staffID, m.OutletRoles); err != nil {
		return User{}, err
	}
	if _, err := kernel.RecordChange(ctx, tx, "staff", staffID, "upsert", nil); err != nil {
		return User{}, err
	}
	if err := kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
		Action: "member.created", TargetType: "user", TargetID: userID,
		Detail: map[string]any{"is_owner": m.IsOwner, "staff_id": staffID},
	}); err != nil {
		return User{}, err
	}
	if !m.EmailVerified {
		if err := s.enqueueVerification(ctx, tx, m.TenantID, userID); err != nil {
			return User{}, err
		}
	}
	return User{ID: userID, Email: m.Email, EmailVerified: m.EmailVerified, Locale: m.Locale, CreatedAt: now}, nil
}

// NoticeExistingAccount handles someone signing up with an address that already has an account,
// without telling the caller anything: an unverified account gets its verification link again, a
// verified one gets an email saying so and pointing at sign-in. It reports whether the account
// exists, for the signup flow only; it must not reach the HTTP response.
func (s *Service) NoticeExistingAccount(ctx context.Context, email string) (bool, error) {
	u, found, err := s.findUserForLogin(ctx, normalizeEmail(email))
	if err != nil || !found {
		return false, err
	}
	if len(u.TenantIDs) == 0 {
		return true, nil
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if u.EmailVerifiedAt == nil {
			return s.enqueueVerification(ctx, tx, u.TenantIDs[0], u.ID)
		}
		// One notice an hour per account, so signing up over and over cannot flood an inbox.
		_, err := s.Jobs.InsertTx(ctx, tx, AccountExistsArgs{UserID: u.ID}, &river.InsertOpts{
			UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: time.Hour},
		})
		return err
	})
	return true, err
}

// ResendVerification queues another verification email for an unverified account. It says
// nothing about whether the email exists, and a repeat within five minutes is dropped.
func (s *Service) ResendVerification(ctx context.Context, email string) error {
	u, found, err := s.findUserForLogin(ctx, normalizeEmail(email))
	if err != nil || !found || u.EmailVerifiedAt != nil || len(u.TenantIDs) == 0 {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.enqueueVerification(ctx, tx, u.TenantIDs[0], u.ID)
	})
}

func (s *Service) enqueueVerification(ctx context.Context, tx pgx.Tx, tenantID, userID uuid.UUID) error {
	_, err := s.Jobs.InsertTx(ctx, tx, VerifyEmailArgs{TenantID: tenantID, UserID: userID}, &river.InsertOpts{
		UniqueOpts: river.UniqueOpts{ByArgs: true, ByPeriod: 5 * time.Minute},
	})
	return err
}

// VerifyEmail consumes the token from a verification email.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	ids, hash, err := parseOpaque(prefixVerification, 1, token)
	if err != nil {
		return err
	}
	tenantID := ids[0]
	return kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		now := s.Clock.Now()
		v, err := q.GetVerificationForUpdate(ctx, db.GetVerificationForUpdateParams{TenantID: tenantID, TokenHash: hash})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		if v.UsedAt != nil || !v.ExpiresAt.After(now) {
			return ErrInvalidToken
		}
		if err := q.MarkVerificationUsed(ctx, db.MarkVerificationUsedParams{ID: v.ID, Now: &now}); err != nil {
			return err
		}
		return q.MarkEmailVerified(ctx, db.MarkEmailVerifiedParams{ID: v.UserID, Now: &now})
	})
}

func (s *Service) gate(ctx context.Context, tenantID uuid.UUID) error {
	if s.Gate == nil {
		return nil
	}
	return s.Gate(ctx, tenantID)
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validateNewMember(in NewMember) error {
	var problems []string
	if in.TenantID == uuid.Nil {
		problems = append(problems, "tenant is required")
	}
	if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email || len(in.Email) > 254 {
		problems = append(problems, "email is not valid")
	}
	if n := utf8.RuneCountInString(in.Password); n < minPasswordLen || n > maxPasswordLen {
		problems = append(problems, fmt.Sprintf("password must be %d to %d characters", minPasswordLen, maxPasswordLen))
	}
	if strings.TrimSpace(in.DisplayName) == "" {
		problems = append(problems, "display name is required")
	}
	if !contains(Locales, in.Locale) {
		problems = append(problems, "locale must be one of "+strings.Join(Locales, ", "))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", kernel.ErrValidation, strings.Join(problems, "; "))
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// mapNoRows turns "no rows" into the given error and leaves other errors alone.
func mapNoRows(err, as error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return as
	}
	return err
}

// ErrEmailTaken is returned when an email address already has an account. It is a
// kernel.ErrConflict.
var ErrEmailTaken = fmt.Errorf("%w: email is already registered", kernel.ErrConflict)

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "user_account_email_key" {
				return ErrEmailTaken
			}
			return fmt.Errorf("%w: %s", kernel.ErrConflict, pgErr.ConstraintName)
		case "23503":
			return fmt.Errorf("%w: referenced row does not exist", kernel.ErrNotFound)
		}
	}
	return err
}

// checkStaffLimit enforces limit.staff. Call it after kernel.LockTenant, so two requests cannot
// both take the last slot.
func (s *Service) checkStaffLimit(ctx context.Context, tx pgx.Tx, q *db.Queries, tenantID uuid.UUID) error {
	if s.Entitlements == nil {
		return nil
	}
	n, err := q.CountActiveStaff(ctx, tenantID)
	if err != nil {
		return err
	}
	return s.Entitlements.CheckLimit(ctx, tx, tenantID, entitlements.LimitStaff, n)
}
