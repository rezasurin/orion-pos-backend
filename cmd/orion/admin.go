package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/config"
	"github.com/rezasurin/orion-pos-backend/internal/database"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

const adminUsage = `usage: orion admin <command>

commands:
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
		return seedDemo(ctx, tenants, ids)
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

func seedDemo(ctx context.Context, tenants *tenancy.Service, ids *identity.Service) error {
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

	fmt.Printf("demo business %s (outlet %s %s)\n", t.ID, o.Code, o.ID)
	fmt.Printf("sign in: owner@demo.orion.test or manager@demo.orion.test, password %s\n", demoPassword)
	fmt.Println("cashiers: Sari PIN 4821, Budi PIN 9071")
	fmt.Printf("device %s code %d, secret: %s\n", dev.Device.ID, dev.Device.Code, dev.Secret)
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
