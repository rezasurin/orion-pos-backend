package main

import (
	"context"
	"testing"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

func TestSeedDemoIsUsableAndRunsOnce(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	ids, err := identity.NewService(identity.Deps{
		Pool: d.App, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	tenants := tenancy.NewService(d.App)
	cat := catalog.NewService(d.App)

	if err := seedDemo(ctx, tenants, ids, cat); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"owner@demo.orion.test", "manager@demo.orion.test"} {
		if _, err := ids.Login(ctx, identity.LoginInput{Email: email, Password: demoPassword}); err != nil {
			t.Errorf("login %s: %v", email, err)
		}
	}
	var items int
	if err := d.Owner.QueryRow(ctx, `SELECT count(*) FROM item i JOIN tenant t ON t.id = i.tenant_id WHERE t.slug = 'demo-kopi'`).Scan(&items); err != nil || items != 6 {
		t.Errorf("demo menu has %d items (%v), want 6", items, err)
	}
	if err := seedDemo(ctx, tenants, ids, cat); err == nil {
		t.Error("seeding twice should fail, not create a second demo business")
	}
}
