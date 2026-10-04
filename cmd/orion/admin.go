package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/config"
	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

const adminUsage = `usage: orion admin <command>

operator commands (connect as the platform role, ORION_PLATFORM_DATABASE_URL and ORION_SECRETS_KEY;
every change is written to the platform audit log with --reason, attributed to --operator):
  create-operator   --email E --reason R [--operator E]
                    creates an operator and prints their password, TOTP secret and recovery codes
                    once; the first operator is created without --operator
  set-override      --operator E --tenant SLUG --key K --value N --reason R [--expires RFC3339]
  clear-override    --operator E --tenant SLUG --key K --reason R
  suspend-tenant    --operator E --tenant SLUG --reason R
  reinstate-tenant  --operator E --tenant SLUG --reason R

tenant commands (connect as the service role, ORION_DATABASE_URL):
  create-tenant   --name N --slug S --outlet-name N --outlet-code C --owner-email E [--password P]
                  creates a business with its first outlet and a verified owner; a random
                  password is generated and printed once if --password is not given
  seed-demo       creates a demo business with an owner, a manager, two cashiers and a paired
                  device, for front-end work (refused in production)
`

// admin runs the operator commands. They connect as the service role (ORION_DATABASE_URL), so
// row-level security applies to them like to the API.
func admin(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	if len(args) == 0 {
		fmt.Print(adminUsage)
		return errors.New("missing admin command")
	}
	switch args[0] {
	case "create-operator", "set-override", "clear-override", "suspend-tenant", "reinstate-tenant":
		return adminPlatform(ctx, cfg, logger, args)
	}
	if cfg.DatabaseURL == "" {
		return errors.New("ORION_DATABASE_URL is required")
	}
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.CheckAppRole(ctx, pool); err != nil {
		return err
	}
	ids, err := identity.NewService(identity.Deps{
		Pool: pool, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		Entitlements: entitlements.NewResolver(pool, kernel.SystemClock{}),
	})
	if err != nil {
		return err
	}
	tenants := tenancy.NewService(pool)

	switch args[0] {
	case "create-tenant":
		fs := flag.NewFlagSet("create-tenant", flag.ContinueOnError)
		name := fs.String("name", "", "business name")
		slug := fs.String("slug", "", "url-safe business id")
		outletName := fs.String("outlet-name", "", "first outlet name")
		outletCode := fs.String("outlet-code", "", "2-6 uppercase letters or digits")
		email := fs.String("owner-email", "", "owner's email")
		password := fs.String("password", "", "owner's password (generated if empty)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		pw, generated := *password, false
		if pw == "" {
			if pw, err = randomPassword(); err != nil {
				return err
			}
			generated = true
		}
		t, o, err := createBusiness(ctx, tenants, ids, tenancy.NewTenant{
			Name: *name, Slug: *slug, Outlet: tenancy.NewOutlet{Name: *outletName, Code: *outletCode},
		}, *email, pw, "Owner")
		if err != nil {
			return err
		}
		logger.Info("tenant created", "tenant_id", t.ID, "outlet_id", o.ID, "owner", *email)
		if generated {
			fmt.Printf("owner password (shown once): %s\n", pw)
		}
		return nil
	case "seed-demo":
		if cfg.Env == "production" {
			return errors.New("seed-demo is refused when ORION_ENV is production")
		}
		return seedDemo(ctx, tenants, ids, catalog.NewService(pool))
	default:
		fmt.Print(adminUsage)
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func createBusiness(ctx context.Context, tenants *tenancy.Service, ids *identity.Service, in tenancy.NewTenant, email, password, ownerName string) (tenancy.Tenant, tenancy.Outlet, error) {
	t, o, err := tenants.CreateTenant(ctx, in)
	if err != nil {
		return tenancy.Tenant{}, tenancy.Outlet{}, err
	}
	_, err = ids.CreateMember(ctx, identity.NewMember{
		TenantID: t.ID, Email: email, Password: password, DisplayName: ownerName, IsOwner: true, EmailVerified: true,
	})
	return t, o, err
}

const demoPassword = "demo-password-1"

func seedDemo(ctx context.Context, tenants *tenancy.Service, ids *identity.Service, cat *catalog.Service) error {
	t, o, err := createBusiness(ctx, tenants, ids, tenancy.NewTenant{
		Name: "Demo Kopi", Slug: "demo-kopi", Outlet: tenancy.NewOutlet{Name: "Demo Kopi Jakarta", Code: "JKT1"},
	}, "owner@demo.orion.test", demoPassword, "Demo Owner")
	if err != nil {
		if errors.Is(err, kernel.ErrConflict) {
			return errors.New("the demo business already exists (slug demo-kopi)")
		}
		return err
	}

	roles, err := ids.ListRoles(ctx, t.ID)
	if err != nil {
		return err
	}
	roleID := func(name string) (id uuid.UUID) {
		for _, r := range roles {
			if r.Name == name {
				return r.ID
			}
		}
		return uuid.Nil
	}
	at := func(role string) []identity.OutletRole {
		return []identity.OutletRole{{OutletID: o.ID, RoleID: roleID(role)}}
	}
	if _, err := ids.CreateMember(ctx, identity.NewMember{
		TenantID: t.ID, Email: "manager@demo.orion.test", Password: demoPassword, DisplayName: "Demo Manager",
		EmailVerified: true, OutletRoles: at("Manager"),
	}); err != nil {
		return err
	}

	// Act as the owner for the staff and device steps, the way the API would.
	owner, err := ids.Login(ctx, identity.LoginInput{Email: "owner@demo.orion.test", Password: demoPassword})
	if err != nil {
		return err
	}
	p, err := ids.Authenticate(identity.AudienceTenant, owner.AccessToken)
	if err != nil {
		return err
	}
	access, err := ids.LoadAccess(ctx, p)
	if err != nil {
		return err
	}
	c := identity.Caller{Principal: p, Access: access}
	for _, s := range []struct{ name, pin string }{{"Sari", "4821"}, {"Budi", "9071"}} {
		if _, err := ids.CreateStaff(ctx, c, identity.NewStaff{DisplayName: s.name, PIN: s.pin, OutletRoles: at("Cashier")}); err != nil {
			return err
		}
	}
	dev, err := ids.PairDevice(ctx, c, identity.NewDevice{OutletID: o.ID, Name: "Kasir 1"})
	if err != nil {
		return err
	}

	if err := seedMenu(ctx, cat, t.ID); err != nil {
		return err
	}

	fmt.Printf("demo business %s (outlet %s %s)\n", t.ID, o.Code, o.ID)
	fmt.Printf("sign in: owner@demo.orion.test or manager@demo.orion.test, password %s\n", demoPassword)
	fmt.Println("cashiers: Sari PIN 4821, Budi PIN 9071")
	fmt.Printf("device %s code %d, secret: %s\n", dev.Device.ID, dev.Device.Code, dev.Secret)
	return nil
}

// seedMenu gives the demo business a small cafe menu: three categories, two modifier groups, and
// items with and without variants, so front-end and POS work has realistic catalog data.
func seedMenu(ctx context.Context, cat *catalog.Service, tenantID uuid.UUID) error {
	category := func(name string, order int) (uuid.UUID, error) {
		c, err := cat.CreateCategory(ctx, tenantID, catalog.NewCategory{Name: name, SortOrder: order})
		return c.ID, err
	}
	coffee, err := category("Coffee", 1)
	if err != nil {
		return err
	}
	drinks, err := category("Other drinks", 2)
	if err != nil {
		return err
	}
	food, err := category("Food", 3)
	if err != nil {
		return err
	}
	sugar, err := cat.CreateModifierGroup(ctx, tenantID, catalog.NewModifierGroup{
		Name: "Sugar level", MinSelect: 1, MaxSelect: 1, Required: true,
		Modifiers: []catalog.NewModifier{{Name: "Normal"}, {Name: "Less sugar"}, {Name: "No sugar"}},
	})
	if err != nil {
		return err
	}
	addOns, err := cat.CreateModifierGroup(ctx, tenantID, catalog.NewModifierGroup{
		Name: "Add-ons", MinSelect: 0, MaxSelect: 3,
		Modifiers: []catalog.NewModifier{{Name: "Extra shot", PriceDelta: 5000}, {Name: "Oat milk", PriceDelta: 4000}, {Name: "Whipped cream", PriceDelta: 3000}},
	})
	if err != nil {
		return err
	}
	one := func(price kernel.Rupiah) []catalog.NewVariant { return []catalog.NewVariant{{BasePrice: price}} }
	for _, it := range []catalog.NewItem{
		{Name: "Espresso", CategoryID: &coffee, Variants: one(18000), ModifierGroupIDs: []uuid.UUID{addOns.ID}},
		{Name: "Latte", CategoryID: &coffee, ModifierGroupIDs: []uuid.UUID{sugar.ID, addOns.ID},
			Variants: []catalog.NewVariant{{Name: "Hot", BasePrice: 28000}, {Name: "Iced", BasePrice: 30000}}},
		{Name: "Kopi susu", CategoryID: &coffee, Variants: one(22000), ModifierGroupIDs: []uuid.UUID{sugar.ID}},
		{Name: "Es teh manis", CategoryID: &drinks, Variants: one(12000), ModifierGroupIDs: []uuid.UUID{sugar.ID}},
		{Name: "Croissant", CategoryID: &food, Variants: one(25000)},
		{Name: "Nasi goreng", CategoryID: &food, Variants: one(35000)},
	} {
		if _, err := cat.CreateItem(ctx, tenantID, it); err != nil {
			return err
		}
	}
	return nil
}

func randomPassword() (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, 20)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}

// adminPlatform runs the operator commands on the platform role.
func adminPlatform(ctx context.Context, cfg config.Config, logger *slog.Logger, args []string) error {
	if cfg.PlatformDatabaseURL == "" {
		return errors.New("ORION_PLATFORM_DATABASE_URL is required")
	}
	pool, err := database.Connect(ctx, cfg.PlatformDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := database.CheckPlatformRole(ctx, pool); err != nil {
		return err
	}
	svc, err := newPlatform(cfg, logger, pool)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	email := fs.String("email", "", "new operator's email")
	operator := fs.String("operator", "", "your operator email, for the audit log")
	reason := fs.String("reason", "", "why (required, goes in the audit log)")
	tenant := fs.String("tenant", "", "tenant slug")
	key := fs.String("key", "", "entitlement key")
	value := fs.Int64("value", 0, "override value (0/1 for modules, a count or -1 for limits)")
	expires := fs.String("expires", "", "override expiry, RFC 3339")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	var actor platform.Actor
	if *operator != "" {
		op, err := svc.OperatorByEmail(ctx, *operator)
		if err != nil {
			return fmt.Errorf("--operator %s: %w", *operator, err)
		}
		if op.Disabled {
			return fmt.Errorf("--operator %s is disabled", *operator)
		}
		actor.OperatorID = &op.ID
	} else if args[0] != "create-operator" {
		return errors.New("--operator is required")
	}

	switch args[0] {
	case "create-operator":
		n, err := svc.CreateOperator(ctx, actor, *email, *reason)
		if err != nil {
			return err
		}
		fmt.Printf("operator %s created (shown once, store them now)\n", n.Operator.Email)
		fmt.Printf("password:       %s\n", n.Password)
		fmt.Printf("totp secret:    %s\n", n.TOTPSecret)
		fmt.Printf("totp uri:       %s\n", n.TOTPURI)
		fmt.Println("recovery codes (each works once):")
		for _, c := range n.RecoveryCodes {
			fmt.Println("  " + c)
		}
		return nil
	case "set-override":
		var exp *time.Time
		if *expires != "" {
			t, err := time.Parse(time.RFC3339, *expires)
			if err != nil {
				return fmt.Errorf("--expires: %w", err)
			}
			exp = &t
		}
		return svc.SetOverride(ctx, actor, *tenant, entitlements.Key(*key), *value, exp, *reason)
	case "clear-override":
		return svc.ClearOverride(ctx, actor, *tenant, entitlements.Key(*key), *reason)
	case "suspend-tenant":
		return svc.SetTenantSuspended(ctx, actor, *tenant, true, *reason)
	default: // reinstate-tenant
		return svc.SetTenantSuspended(ctx, actor, *tenant, false, *reason)
	}
}
