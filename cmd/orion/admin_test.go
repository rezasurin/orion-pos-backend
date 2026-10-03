package main

import (
	"context"
	"testing"

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

	if err := seedDemo(ctx, tenants, ids); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"owner@demo.orion.test", "manager@demo.orion.test"} {
		if _, err := ids.Login(ctx, identity.LoginInput{Email: email, Password: demoPassword}); err != nil {
			t.Errorf("login %s: %v", email, err)
		}
	}
	if err := seedDemo(ctx, tenants, ids); err == nil {
		t.Error("seeding twice should fail, not create a second demo business")
	}
}
