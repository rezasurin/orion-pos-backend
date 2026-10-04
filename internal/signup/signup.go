// Package signup creates a business from the public sign-up form: the tenant, its system roles,
// its first outlet and its owner, all in one transaction, so a business never exists without an
// owner and a failed sign-up leaves nothing behind.
//
// The owner cannot sign in until the email address is verified (identity queues the link in the
// same transaction). Signing up with an address that already has an account creates nothing and
// is indistinguishable, to the caller, from a new sign-up: the existing account's owner gets an
// email instead, so the form cannot be used to find out who has an account.
package signup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// Deps are what the service needs.
type Deps struct {
	Pool     *pgxpool.Pool // a member of orion_app
	Identity *identity.Service
	Tenancy  *tenancy.Service
}

// Service creates businesses from sign-ups.
type Service struct{ Deps }

// NewService returns a Service.
func NewService(d Deps) *Service { return &Service{d} }

// Input is the sign-up form.
type Input struct {
	BusinessName string
	OwnerName    string
	Email        string
	Password     string
	// OutletName defaults to the business name; OutletCode to letters from it followed by 1;
	// Timezone to Asia/Jakarta; Locale to id-ID.
	OutletName string
	OutletCode string
	Timezone   string
	Locale     string
}

const (
	maxNameLen = 120
	// slugAttempts bounds the retries when a generated slug is taken. The suffix has 36^4 values.
	slugAttempts = 5
	slugSuffix   = 4
	maxSlugBase  = 40
)

// SignUp creates the business, or does nothing when the email already has an account. In both
// cases it returns nil, so the caller cannot learn which happened; invalid input is an error.
func (s *Service) SignUp(ctx context.Context, in Input) error {
	in.BusinessName = strings.TrimSpace(in.BusinessName)
	in.OwnerName = strings.TrimSpace(in.OwnerName)
	if err := validate(in); err != nil {
		return err
	}
	outletName := strings.TrimSpace(in.OutletName)
	if outletName == "" {
		outletName = in.BusinessName
	}
	code := strings.ToUpper(strings.TrimSpace(in.OutletCode))
	if code == "" {
		code = deriveOutletCode(outletName)
	}

	tenantID := kernel.NewID()
	// Check and hash before any transaction: it costs the same whether or not the address is
	// known, so the response time does not tell.
	owner, err := s.Identity.PrepareMember(ctx, identity.NewMember{
		TenantID: tenantID, Email: in.Email, Password: in.Password, DisplayName: in.OwnerName,
		Locale: in.Locale, IsOwner: true,
	})
	if err != nil {
		return err
	}
	if exists, err := s.Identity.NoticeExistingAccount(ctx, owner.Email); err != nil || exists {
		return err
	}

	base := slugBase(in.BusinessName)
	for attempt := 0; ; attempt++ {
		slug := base + "-" + newSuffix(slugSuffix)
		err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
			t, o, err := s.Tenancy.CreateTenantIn(ctx, tx, tenantID, tenancy.NewTenant{
				Name: in.BusinessName, Slug: slug,
				Outlet: tenancy.NewOutlet{Name: outletName, Code: code, Timezone: in.Timezone},
			})
			if err != nil {
				return err
			}
			if _, err := s.Identity.CreateMemberIn(ctx, tx, owner); err != nil {
				return err
			}
			return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
				Action: "tenant.signed_up", TargetType: "tenant", TargetID: t.ID,
				Detail: map[string]any{"outlet_id": o.ID, "slug": t.Slug},
			})
		})
		switch {
		case err == nil:
			return nil
		case errors.Is(err, identity.ErrEmailTaken):
			// Another sign-up with the same address won the race after our check.
			_, err := s.Identity.NoticeExistingAccount(ctx, owner.Email)
			return err
		case errors.Is(err, tenancy.ErrSlugTaken) && attempt+1 < slugAttempts:
			continue
		default:
			return err
		}
	}
}

func validate(in Input) error {
	var problems []string
	if in.BusinessName == "" || len([]rune(in.BusinessName)) > maxNameLen {
		problems = append(problems, fmt.Sprintf("business name is required, at most %d characters", maxNameLen))
	}
	if in.OwnerName == "" || len([]rune(in.OwnerName)) > 60 {
		problems = append(problems, "your name is required, at most 60 characters")
	}
	if len([]rune(in.OutletName)) > maxNameLen {
		problems = append(problems, fmt.Sprintf("outlet name is at most %d characters", maxNameLen))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", kernel.ErrValidation, strings.Join(problems, "; "))
	}
	return nil
}

// slugBase turns a business name into the readable part of its slug: lowercase ASCII letters and
// digits joined by single hyphens, at most maxSlugBase characters. A name with none (for example
// written in another script) becomes "bisnis".
func slugBase(name string) string {
	var b strings.Builder
	pendingHyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if pendingHyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingHyphen = false
			b.WriteRune(r)
		default:
			pendingHyphen = true
		}
		if b.Len() >= maxSlugBase {
			break
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "bisnis"
	}
	return out
}

// deriveOutletCode makes a receipt prefix from an outlet name: up to four letters or digits,
// uppercased, then 1 ("Kopi Senja" gives KOPI1). A name with none gives OUT1.
func deriveOutletCode(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			if b.Len() == 4 {
				break
			}
		}
	}
	if b.Len() == 0 {
		return "OUT1"
	}
	return b.String() + "1"
}

// newSuffix is randomSuffix; a test replaces it to force a slug collision.
var newSuffix = randomSuffix

const slugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomSuffix(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // the system's random source failing is not recoverable
	}
	out := make([]byte, n)
	for i, c := range raw {
		out[i] = slugAlphabet[int(c)%len(slugAlphabet)]
	}
	return string(out)
}
