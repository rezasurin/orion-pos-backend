package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
)

// limit sets an override so the tenant is capped regardless of its plan.
func (e *env) limit(t *testing.T, key entitlements.Key, n int64) {
	t.Helper()
	err := entitlements.NewAdmin(e.d.Platform).SetOverride(context.Background(),
		entitlements.Override{TenantID: e.tenant, Key: key, Value: n, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeviceLimitCountsOnlyActiveDevices(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	e.limit(t, entitlements.LimitDevices, 2)

	d1 := e.pair(t, owner, a, "one")
	e.pair(t, owner, a, "two")
	_, err := e.svc.PairDevice(ctx, owner, identity.NewDevice{OutletID: a, Name: "three"})
	var le *entitlements.LimitError
	if !errors.As(err, &le) || le.Key != entitlements.LimitDevices || le.Limit != 2 {
		t.Fatalf("third device: %v, want a limit error for 2", err)
	}
	// A refused pairing leaves nothing behind, not even a spent device code.
	if err := e.svc.RevokeDevice(ctx, owner, d1.Device.ID); err != nil {
		t.Fatal(err)
	}
	if d := e.pair(t, owner, a, "replacement"); d.Device.Code != 3 {
		t.Errorf("code after a refusal and a revoke = %d, want 3", d.Device.Code)
	}
}

func TestStaffLimit(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.member(t, "owner@kopi.test", true) // one active staff record
	owner := e.caller(t, "owner@kopi.test")
	e.limit(t, entitlements.LimitStaff, 3)

	var first identity.Staff
	for i, name := range []string{"A", "B"} {
		st, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: name})
		if err != nil {
			t.Fatalf("staff %s: %v", name, err)
		}
		if i == 0 {
			first = st
		}
	}
	if _, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "C"}); !errors.Is(err, entitlements.ErrLimitReached) {
		t.Errorf("4th active staff: %v, want ErrLimitReached", err)
	}
	if _, err := e.svc.CreateMember(ctx, identity.NewMember{
		TenantID: e.tenant, Email: "x@kopi.test", Password: password, DisplayName: "X", EmailVerified: true,
	}); !errors.Is(err, entitlements.ErrLimitReached) {
		t.Errorf("a member also takes a staff slot: %v", err)
	}
	// Deactivating frees a slot.
	off := false
	if _, err := e.svc.UpdateStaff(ctx, owner, first.ID, identity.UpdateStaff{Active: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "C"}); err != nil {
		t.Errorf("after deactivating: %v", err)
	}
}

// Ten tablets race for three slots. The tenant lock serializes them, so exactly three win.
func TestLimitHoldsUnderConcurrency(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	e.limit(t, entitlements.LimitDevices, 3)

	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		won, refused int
		other        []error
	)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.PairDevice(ctx, owner, identity.NewDevice{OutletID: a, Name: "t"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, entitlements.ErrLimitReached):
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()
	if len(other) > 0 || won != 3 || refused != 7 {
		t.Errorf("won %d, refused %d, other errors %v; want 3, 7, none", won, refused, other)
	}
}
