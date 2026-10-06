// Package tenancy owns tenants, outlets and outlet settings. Other modules use this package's
// Service and types, never the generated queries in ./db.
package tenancy

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy/db"
)

// Timezones an outlet can be in: WIB, WITA and WIT.
var Timezones = []string{"Asia/Jakarta", "Asia/Makassar", "Asia/Jayapura"}

var (
	slugPattern       = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	outletCodePattern = regexp.MustCompile(`^[A-Z0-9]{2,6}$`)
)

// ErrSuspended means an operator suspended the business. Its users and devices cannot sign in.
var ErrSuspended = errors.New("tenancy: business is suspended")

// Tenant is a business using Orion.
type Tenant struct {
	ID                 uuid.UUID
	Name               string
	Slug               string
	SubscriptionStatus string
	CreatedAt          time.Time
}

// Outlet is one location of a tenant, with its settings.
type Outlet struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	Code      string
	Address   string
	Settings  OutletSettings
	CreatedAt time.Time
}

// OutletSettings are the per-outlet tax, rounding, time and receipt settings (ADR 0006).
type OutletSettings struct {
	Timezone             string
	BusinessDayCutoff    time.Duration // when the business day begins, as a time of day; 0 is midnight
	PriceIncludesTax     bool
	TaxRate              kernel.BasisPoints
	ServiceChargeRate    kernel.BasisPoints
	ServiceChargeTaxable bool
	CashRoundingUnit     kernel.Rupiah
	CashRoundingMode     string
	ReceiptHeader        string
	ReceiptFooter        string
}

// Plans a tenant can be created on.
const (
	PlanEarlyAccess = "early_access" // unrestricted (ADR 0007): operator-made tenants and design partners
	PlanFree        = "free"         // self-serve signups: 1 outlet, 2 devices, 5 staff
)

// NewTenant is what CreateTenant needs: the business and its first outlet.
type NewTenant struct {
	Name   string
	Slug   string // generated from the name when empty (signup)
	Plan   string // PlanEarlyAccess when empty
	Outlet NewOutlet
}

// NewOutlet describes an outlet to create.
type NewOutlet struct {
	Name     string
	Code     string
	Address  string
	Timezone string // defaults to Asia/Jakarta
}

// Service is the tenancy module's API.
type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service using pool, whose role must be a member of orion_app.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// CreateTenant creates a tenant on the early_access plan with its system roles and its first
// outlet, in one transaction.
func (s *Service) CreateTenant(ctx context.Context, in NewTenant) (Tenant, Outlet, error) {
	return s.CreateTenantWith(ctx, in, nil)
}

// CreateTenantWith is CreateTenant that also runs then inside the same transaction, after the
// outlet exists. Signup uses it to add the owner, so a business never exists without one.
func (s *Service) CreateTenantWith(ctx context.Context, in NewTenant, then func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error) (Tenant, Outlet, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Slug == "" {
		in.Slug = slugFor(in.Name)
	}
	if in.Plan == "" {
		in.Plan = PlanEarlyAccess
	}
	if err := validateTenant(in); err != nil {
		return Tenant{}, Outlet{}, err
	}

	var (
		tenant db.Tenant
		outlet Outlet
	)
	tenantID := kernel.NewID()
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		// The new tenant row is locked by this insert and invisible to anyone else, so it is
		// already the first lock this transaction holds (see kernel.LockTenant).
		tenant, err = q.InsertTenant(ctx, db.InsertTenantParams{ID: tenantID, Name: in.Name, Slug: in.Slug, PlanCode: in.Plan})
		if err != nil {
			return mapErr(err)
		}
		if err = identity.SeedRoles(ctx, tx, tenantID); err != nil {
			return err
		}
		if outlet, err = insertOutlet(ctx, q, tenantID, in.Outlet); err != nil || then == nil {
			return err
		}
		return then(ctx, tx, tenantID)
	})
	if err != nil {
		return Tenant{}, Outlet{}, err
	}
	return toTenant(tenant), outlet, nil
}

// GetTenant returns a tenant.
func (s *Service) GetTenant(ctx context.Context, tenantID uuid.UUID) (Tenant, error) {
	var t db.Tenant
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		t, err = db.New(tx).GetTenant(ctx, tenantID)
		return mapErr(err)
	})
	return toTenant(t), err
}

// CheckActive returns ErrSuspended if an operator suspended the tenant, and ErrNotFound if it does
// not exist. It is the gate sign-in and request authentication pass through.
func (s *Service) CheckActive(ctx context.Context, tenantID uuid.UUID) error {
	var t db.Tenant
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		t, err = db.New(tx).GetTenant(ctx, tenantID)
		return mapErr(err)
	})
	if err != nil {
		return err
	}
	if t.SuspendedAt != nil {
		return ErrSuspended
	}
	return nil
}

// ListOutlets returns the tenant's active outlets with their settings, ordered by code.
func (s *Service) ListOutlets(ctx context.Context, tenantID uuid.UUID) ([]Outlet, error) {
	var out []Outlet
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListOutletsWithSettings(ctx, tenantID)
		if err != nil {
			return err
		}
		out = make([]Outlet, len(rows))
		for i, r := range rows {
			out[i] = toOutlet(r.Outlet, r.OutletSetting)
		}
		return nil
	})
	return out, err
}

// GetOutlet returns one outlet with its settings.
func (s *Service) GetOutlet(ctx context.Context, tenantID, outletID uuid.UUID) (Outlet, error) {
	var out Outlet
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		r, err := db.New(tx).GetOutletWithSettings(ctx, db.GetOutletWithSettingsParams{TenantID: tenantID, ID: outletID})
		if err != nil {
			return mapErr(err)
		}
		out = toOutlet(r.Outlet, r.OutletSetting)
		return nil
	})
	return out, err
}

func insertOutlet(ctx context.Context, q *db.Queries, tenantID uuid.UUID, in NewOutlet) (Outlet, error) {
	o, err := q.InsertOutlet(ctx, db.InsertOutletParams{
		ID:       kernel.NewID(),
		TenantID: tenantID,
		Name:     strings.TrimSpace(in.Name),
		Code:     in.Code,
		Address:  strings.TrimSpace(in.Address),
	})
	if err != nil {
		return Outlet{}, mapErr(err)
	}
	tz := in.Timezone
	if tz == "" {
		tz = Timezones[0]
	}
	st, err := q.InsertOutletSettings(ctx, db.InsertOutletSettingsParams{
		OutletID: o.ID,
		TenantID: tenantID,
		Timezone: tz,
	})
	if err != nil {
		return Outlet{}, mapErr(err)
	}
	// Devices pull outlet and settings changes (BACKEND_PLAN.md section 5.2).
	for _, entity := range []string{"outlet", "outlet_settings"} {
		if _, err := q.RecordChange(ctx, db.RecordChangeParams{
			EntityType: entity, EntityID: o.ID, Op: "upsert", OutletID: &o.ID,
		}); err != nil {
			return Outlet{}, err
		}
	}
	return toOutlet(o, st), nil
}

// slugFor makes a slug from a business name plus a random suffix, so two cafes with the same name
// do not collide: "Kopi Kenangan!" becomes "kopi-kenangan-x7k2qd".
func slugFor(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			hyphen = false
		} else {
			hyphen = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	suffix := strings.ToLower(rand.Text()[:6])
	if b.Len() == 0 {
		return "usaha-" + suffix
	}
	return b.String() + "-" + suffix
}

func validateTenant(in NewTenant) error {
	var problems []string
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 100 {
		problems = append(problems, "name is required, at most 100 characters")
	}
	if !slugPattern.MatchString(in.Slug) {
		problems = append(problems, "slug must be lowercase letters, digits and single hyphens")
	}
	if strings.TrimSpace(in.Outlet.Name) == "" {
		problems = append(problems, "outlet name is required")
	}
	if in.Plan != PlanEarlyAccess && in.Plan != PlanFree {
		problems = append(problems, "plan must be "+PlanEarlyAccess+" or "+PlanFree)
	}
	if !outletCodePattern.MatchString(in.Outlet.Code) {
		problems = append(problems, "outlet code must be 2 to 6 uppercase letters or digits")
	}
	if in.Outlet.Timezone != "" && !isTimezone(in.Outlet.Timezone) {
		problems = append(problems, "outlet timezone must be one of "+strings.Join(Timezones, ", "))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", kernel.ErrValidation, strings.Join(problems, "; "))
	}
	return nil
}

func isTimezone(tz string) bool {
	for _, t := range Timezones {
		if t == tz {
			return true
		}
	}
	return false
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "tenant_slug_key":
			return fmt.Errorf("%w: slug is taken", kernel.ErrConflict)
		case "outlet_tenant_id_code_key":
			return fmt.Errorf("%w: outlet code is taken", kernel.ErrConflict)
		}
		return fmt.Errorf("%w: %s", kernel.ErrConflict, pgErr.ConstraintName)
	}
	return err
}

func toTenant(t db.Tenant) Tenant {
	return Tenant{
		ID:                 t.ID,
		Name:               t.Name,
		Slug:               t.Slug,
		SubscriptionStatus: string(t.SubscriptionStatus),
		CreatedAt:          t.CreatedAt,
	}
}

func toOutlet(o db.Outlet, s db.OutletSetting) Outlet {
	return Outlet{
		ID:        o.ID,
		TenantID:  o.TenantID,
		Name:      o.Name,
		Code:      o.Code,
		Address:   o.Address,
		CreatedAt: o.CreatedAt,
		Settings: OutletSettings{
			Timezone:             s.Timezone,
			BusinessDayCutoff:    time.Duration(s.BusinessDayCutoff.Microseconds) * time.Microsecond,
			PriceIncludesTax:     s.PriceIncludesTax,
			TaxRate:              kernel.BasisPoints(s.TaxRateBp),
			ServiceChargeRate:    kernel.BasisPoints(s.ServiceChargeRateBp),
			ServiceChargeTaxable: s.ServiceChargeTaxable,
			CashRoundingUnit:     kernel.Rupiah(s.CashRoundingUnit),
			CashRoundingMode:     string(s.CashRoundingMode),
			ReceiptHeader:        s.ReceiptHeader,
			ReceiptFooter:        s.ReceiptFooter,
		},
	}
}

// OutletInTx returns an outlet with its settings inside the caller's transaction, for projectors
// that need the outlet's code and time settings while applying an event.
func (s *Service) OutletInTx(ctx context.Context, tx pgx.Tx, tenantID, outletID uuid.UUID) (Outlet, error) {
	r, err := db.New(tx).GetOutletWithSettings(ctx, db.GetOutletWithSettingsParams{TenantID: tenantID, ID: outletID})
	if err != nil {
		return Outlet{}, mapErr(err)
	}
	return toOutlet(r.Outlet, r.OutletSetting), nil
}
