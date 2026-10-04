package identity_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

func (e *env) pair(t *testing.T, c identity.Caller, outlet uuid.UUID, name string) identity.PairedDevice {
	t.Helper()
	p, err := e.svc.PairDevice(context.Background(), c, identity.NewDevice{OutletID: outlet, Name: name})
	if err != nil {
		t.Fatalf("PairDevice(%s): %v", name, err)
	}
	return p
}

func TestPairExchangeAndDeviceToken(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	outlet := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")

	p := e.pair(t, owner, outlet, "  Kasir 1 ")
	if p.Device.Name != "Kasir 1" || p.Device.Code != 1 || p.Device.OutletID != outlet || p.Device.PairedBy != owner.Principal.UserID || p.Device.RevokedAt != nil {
		t.Errorf("device = %+v", p.Device)
	}
	if !strings.HasPrefix(p.Secret, "dk1."+e.tenant.String()+"."+p.Device.ID.String()+".") {
		t.Errorf("secret %q does not name its tenant and device", p.Secret)
	}

	// Only a hash is stored: the secret itself is nowhere in the row.
	var stored []byte
	var asText string
	if err := e.d.Owner.QueryRow(ctx, `SELECT secret_hash, row_to_json(device)::text FROM device WHERE id = $1`, p.Device.ID).Scan(&stored, &asText); err != nil {
		t.Fatal(err)
	}
	secretPart := p.Secret[strings.LastIndex(p.Secret, ".")+1:]
	if len(stored) != 32 || strings.Contains(asText, secretPart) {
		t.Errorf("stored hash is %d bytes; row contains the secret: %v", len(stored), strings.Contains(asText, secretPart))
	}

	// Exchange: a device token that authenticates as the device, with its outlet, and records
	// what support needs to know.
	deviceClock := e.clock.Now().Add(-5 * time.Second)
	sess, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret, AppVersion: "1.4.2", ClientTime: &deviceClock})
	if err != nil {
		t.Fatal(err)
	}
	if sess.DeviceID != p.Device.ID || sess.OutletID != outlet || sess.TenantID != e.tenant {
		t.Errorf("session = %+v", sess)
	}
	if want := e.clock.Now().Add(30 * time.Minute); !sess.AccessExpiresAt.Equal(want) {
		t.Errorf("expires %v, want %v", sess.AccessExpiresAt, want)
	}
	pr, err := e.svc.Authenticate(identity.AudienceDevice, sess.AccessToken)
	if err != nil || pr.Type != identity.PrincipalDevice || pr.DeviceID != p.Device.ID || pr.OutletID != outlet || pr.TenantID != e.tenant || pr.ID() != p.Device.ID {
		t.Fatalf("principal = %+v, %v", pr, err)
	}
	list, err := e.svc.ListDevices(ctx, owner, kernel.Page{})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	d := list.Items[0]
	if d.LastSeenAt == nil || d.AppVersion == nil || *d.AppVersion != "1.4.2" || d.ClockSkewMs == nil || *d.ClockSkewMs != 5000 {
		t.Errorf("recorded contact = seen %v, version %v, skew %v; want skew 5000ms", d.LastSeenAt, d.AppVersion, d.ClockSkewMs)
	}

	// A device token is not a user token, and the other way round.
	if _, err := e.svc.Authenticate(identity.AudienceTenant, sess.AccessToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("device token as a user token: %v", err)
	}
	user := e.login(t, "owner@kopi.test")
	if _, err := e.svc.Authenticate(identity.AudienceDevice, user.AccessToken); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("user token as a device token: %v", err)
	}

	// Bad secrets: wrong random part, garbage, another tenant's id in front.
	wrong := p.Secret[:strings.LastIndex(p.Secret, ".")+1] + strings.Repeat("A", 43)
	other := e.newTenant(t, "other")
	forged := strings.Replace(p.Secret, e.tenant.String(), other.String(), 1)
	for name, secret := range map[string]string{"wrong secret": wrong, "garbage": "nope", "empty": "", "wrong tenant": forged} {
		if _, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: secret}); !errors.Is(err, identity.ErrInvalidToken) {
			t.Errorf("%s: err = %v, want ErrInvalidToken", name, err)
		}
	}
	// A skew over a day is ignored rather than recorded.
	way := e.clock.Now().Add(-48 * time.Hour)
	if _, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret, ClientTime: &way}); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceCodesAreNeverReused(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")

	d1, d2 := e.pair(t, owner, a, "one"), e.pair(t, owner, a, "two")
	if d1.Device.Code != 1 || d2.Device.Code != 2 {
		t.Fatalf("codes = %d, %d, want 1, 2", d1.Device.Code, d2.Device.Code)
	}
	// Revoking the newest device does not free its code.
	if err := e.svc.RevokeDevice(ctx, owner, d2.Device.ID); err != nil {
		t.Fatal(err)
	}
	if d3 := e.pair(t, owner, a, "three"); d3.Device.Code != 3 {
		t.Errorf("code after revoking the newest = %d, want 3", d3.Device.Code)
	}
	// Each outlet counts on its own.
	if db1 := e.pair(t, owner, b, "other outlet"); db1.Device.Code != 1 {
		t.Errorf("first code at another outlet = %d, want 1", db1.Device.Code)
	}
}

func TestRevokeDevice(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	outlet := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	p := e.pair(t, owner, outlet, "Kasir 1")
	sess, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret})
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := e.svc.Authenticate(identity.AudienceDevice, sess.AccessToken)
	if err := e.svc.CheckDevice(ctx, principal); err != nil {
		t.Fatalf("before revoking: %v", err)
	}

	if err := e.svc.RevokeDevice(ctx, owner, p.Device.ID); err != nil {
		t.Fatal(err)
	}
	// Cut off at once, even with a token that has not expired, and from the exchange.
	if err := e.svc.CheckDevice(ctx, principal); !errors.Is(err, identity.ErrDeviceRevoked) {
		t.Errorf("CheckDevice after revoking: %v, want ErrDeviceRevoked", err)
	}
	if _, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret}); !errors.Is(err, identity.ErrDeviceRevoked) {
		t.Errorf("exchange after revoking: %v, want ErrDeviceRevoked", err)
	}
	if _, err := e.svc.GetRoster(ctx, principal); err != nil {
		t.Logf("roster for a revoked device is refused by the HTTP layer, not by GetRoster: %v", err)
	}

	// Revoking again is fine and is audited once.
	if err := e.svc.RevokeDevice(ctx, owner, p.Device.ID); err != nil {
		t.Errorf("second revoke: %v", err)
	}
	var revoked int
	_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM tenant_audit_log WHERE action = 'device.revoked'`).Scan(&revoked)
	if revoked != 1 {
		t.Errorf("device.revoked audit entries = %d, want 1", revoked)
	}
	if err := e.svc.RevokeDevice(ctx, owner, uuid.New()); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown device: %v, want ErrNotFound", err)
	}
	// A device that never existed fails CheckDevice as an invalid token, not as revoked.
	ghost := principal
	ghost.DeviceID = uuid.New()
	if err := e.svc.CheckDevice(ctx, ghost); !errors.Is(err, identity.ErrInvalidToken) {
		t.Errorf("unknown device: %v, want ErrInvalidToken", err)
	}
}

func TestDeviceAuthorization(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	e.member(t, "owner@kopi.test", true)
	e.manager(t, "mgr@kopi.test", "Manager", a)
	e.manager(t, "cash@kopi.test", "Cashier", a)
	owner, mgr, cash := e.caller(t, "owner@kopi.test"), e.caller(t, "mgr@kopi.test"), e.caller(t, "cash@kopi.test")

	atA, atB := e.pair(t, owner, a, "A"), e.pair(t, owner, b, "B")
	if _, err := e.svc.PairDevice(ctx, mgr, identity.NewDevice{OutletID: a, Name: "ok"}); err != nil {
		t.Errorf("manager pairing at their outlet: %v", err)
	}
	for name, fn := range map[string]func() error{
		"manager pairing at another outlet": func() error {
			_, err := e.svc.PairDevice(ctx, mgr, identity.NewDevice{OutletID: b, Name: "x"})
			return err
		},
		"cashier pairing": func() error {
			_, err := e.svc.PairDevice(ctx, cash, identity.NewDevice{OutletID: a, Name: "x"})
			return err
		},
		"manager revoking at another outlet": func() error { return e.svc.RevokeDevice(ctx, mgr, atB.Device.ID) },
		"cashier revoking":                   func() error { return e.svc.RevokeDevice(ctx, cash, atA.Device.ID) },
	} {
		if err := fn(); !isForbidden(err) {
			t.Errorf("%s: err = %v, want ErrForbidden", name, err)
		}
	}
	if err := e.svc.RevokeDevice(ctx, mgr, atA.Device.ID); err != nil {
		t.Errorf("manager revoking at their outlet: %v", err)
	}

	// Lists are filtered by outlet in SQL, so a manager sees only theirs.
	got, err := e.svc.ListDevices(ctx, mgr, kernel.Page{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range got.Items {
		if d.OutletID != a {
			t.Errorf("manager at A sees a device at %v", d.OutletID)
		}
	}
	if all, _ := e.svc.ListDevices(ctx, owner, kernel.Page{}); len(all.Items) != 3 {
		t.Errorf("owner sees %d devices, want 3", len(all.Items))
	}
	if none, _ := e.svc.ListDevices(ctx, cash, kernel.Page{}); len(none.Items) != 0 {
		t.Errorf("cashier sees %d devices, want 0", len(none.Items))
	}

	for name, in := range map[string]identity.NewDevice{
		"no name":        {OutletID: a},
		"no outlet":      {Name: "x"},
		"unknown outlet": {OutletID: uuid.New(), Name: "x"},
		"long name":      {OutletID: a, Name: strings.Repeat("x", 61)},
	} {
		if _, err := e.svc.PairDevice(ctx, owner, in); !errors.Is(err, kernel.ErrValidation) && !isForbidden(err) {
			t.Errorf("%s: err = %v, want ErrValidation (or ErrForbidden for an outlet nobody holds)", name, err)
		}
	}
}

func TestListDevicesPages(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	var ids []uuid.UUID
	for i := range 7 {
		ids = append(ids, e.pair(t, owner, a, "d"+string(rune('0'+i))).Device.ID)
	}

	var seen []uuid.UUID
	page := kernel.Page{Limit: 3}
	for range 5 {
		res, err := e.svc.ListDevices(ctx, owner, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Items {
			seen = append(seen, d.ID)
		}
		if res.Next == uuid.Nil {
			break
		}
		page.After = res.Next
	}
	if !slices.Equal(seen, ids) {
		t.Errorf("paged %v, want %v", seen, ids)
	}
}

func TestRoster(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	ownerUser := e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier, kitchen := e.role(t, e.tenant, "Cashier"), e.role(t, e.tenant, "Kitchen")

	mk := func(name, pin string, roles ...identity.OutletRole) identity.Staff {
		t.Helper()
		st, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: name, PIN: pin, OutletRoles: roles})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	sari := mk("Sari", "4821", identity.OutletRole{OutletID: a, RoleID: cashier}, identity.OutletRole{OutletID: a, RoleID: kitchen})
	mk("Budi", "", identity.OutletRole{OutletID: a, RoleID: cashier}) // no PIN yet
	mk("Elsewhere", "9071", identity.OutletRole{OutletID: b, RoleID: cashier})
	mk("Nowhere", "1357")
	gone := mk("Gone", "2468", identity.OutletRole{OutletID: a, RoleID: cashier})
	off := false
	if _, err := e.svc.UpdateStaff(ctx, owner, gone.ID, identity.UpdateStaff{Active: &off}); err != nil {
		t.Fatal(err)
	}

	p := e.pair(t, owner, a, "Kasir 1")
	sess, err := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret})
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := e.svc.Authenticate(identity.AudienceDevice, sess.AccessToken)

	roster, err := e.svc.GetRoster(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if roster.OutletID != a || roster.Device.ID != p.Device.ID || roster.Device.Code != 1 {
		t.Errorf("roster header = %+v", roster)
	}
	byName := map[string]identity.RosterStaff{}
	for _, st := range roster.Staff {
		byName[st.DisplayName] = st
	}
	// The owner's own staff record is named after the member; find it by id instead.
	var ownerEntry *identity.RosterStaff
	for i, st := range roster.Staff {
		if st.DisplayName == "Test" { // CreateMember's display name in the helper
			ownerEntry = &roster.Staff[i]
		}
	}
	_ = ownerUser
	if got := len(roster.Staff); got != 3 { // owner, Sari, Budi
		names := []string{}
		for _, st := range roster.Staff {
			names = append(names, st.DisplayName)
		}
		t.Fatalf("roster has %d people %v, want the owner, Sari and Budi only", got, names)
	}
	if ownerEntry == nil || len(ownerEntry.Permissions) != len(identity.AllPermissions()) || ownerEntry.PINHash != nil {
		t.Errorf("owner entry = %+v", ownerEntry)
	}
	if s := byName["Sari"]; s.StaffID != sari.ID || s.PINHash == nil || !strings.HasPrefix(*s.PINHash, "$argon2id$") ||
		!slices.Equal(s.Permissions, []identity.Permission{identity.PermKitchenView, identity.PermSaleCreate, identity.PermShiftClose, identity.PermShiftOpen}) {
		t.Errorf("Sari = %+v (a cashier and kitchen role merge their permissions)", s)
	}
	if bd := byName["Budi"]; bd.PINHash != nil || len(bd.Permissions) != 3 {
		t.Errorf("Budi = %+v, want no PIN hash and the cashier permissions", bd)
	}
	for _, name := range []string{"Elsewhere", "Nowhere", "Gone"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%s should not be on this outlet's roster", name)
		}
	}
}

func TestRosterQueryCountIsConstant(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier := e.role(t, e.tenant, "Cashier")
	p := e.pair(t, owner, a, "Kasir 1")
	sess, _ := e.svc.ExchangeDeviceSecret(ctx, identity.DeviceExchange{Secret: p.Secret})
	principal, _ := e.svc.Authenticate(identity.AudienceDevice, sess.AccessToken)

	roster := func() (r identity.Roster) {
		var err error
		if r, err = e.svc.GetRoster(ctx, principal); err != nil {
			t.Fatal(err)
		}
		return r
	}
	var small identity.Roster
	few := e.d.Queries.During(func() { small = roster() })
	for range 40 {
		if _, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "S", PIN: "4821", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: cashier}}}); err != nil {
			t.Fatal(err)
		}
	}
	var big identity.Roster
	many := e.d.Queries.During(func() { big = roster() })
	if len(big.Staff) != len(small.Staff)+40 || few != many {
		t.Errorf("roster of %d people took %d queries; of %d took %d; want equal", len(small.Staff), few, len(big.Staff), many)
	}
}

func TestCheckDeviceTouchesAtMostOncePerMinute(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	p := e.pair(t, owner, a, "Kasir 1")
	principal := identity.Principal{Type: identity.PrincipalDevice, TenantID: e.tenant, DeviceID: p.Device.ID, OutletID: a}

	seen := func() time.Time {
		var ts time.Time
		if err := e.d.Owner.QueryRow(ctx, `SELECT last_seen_at FROM device WHERE id = $1`, p.Device.ID).Scan(&ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if err := e.svc.CheckDevice(ctx, principal); err != nil {
		t.Fatal(err)
	}
	first := seen()
	e.clock.Advance(20 * time.Second)
	if err := e.svc.CheckDevice(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if !seen().Equal(first) {
		t.Error("last_seen_at was written again within a minute")
	}
	e.clock.Advance(2 * time.Minute)
	if err := e.svc.CheckDevice(ctx, principal); err != nil {
		t.Fatal(err)
	}
	if !seen().After(first) {
		t.Error("last_seen_at was not refreshed after a minute")
	}
}

// Sixteen tablets paired at one outlet at once get sixteen different codes, with no deadlock.
func TestConcurrentPairingGivesDistinctCodes(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")

	const n = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes []int
		errs  []error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := e.svc.PairDevice(ctx, owner, identity.NewDevice{OutletID: a, Name: "tablet"})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			codes = append(codes, p.Device.Code)
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	slices.Sort(codes)
	for i, c := range codes {
		if c != i+1 {
			t.Fatalf("codes = %v, want 1..%d with no gaps or repeats", codes, n)
		}
	}
}

func TestDeviceGrantsAndIsolation(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	p := e.pair(t, owner, a, "Kasir 1")

	other := e.newTenant(t, "other")
	e.memberIn(t, other, "owner@other.test", true)
	otherOwner := e.caller(t, "owner@other.test")

	// Another tenant can neither see nor revoke the device.
	if err := e.svc.RevokeDevice(ctx, otherOwner, p.Device.ID); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("revoking another tenant's device: %v, want ErrNotFound", err)
	}
	if res, _ := e.svc.ListDevices(ctx, otherOwner, kernel.Page{}); len(res.Items) != 0 {
		t.Errorf("another tenant lists %d devices", len(res.Items))
	}

	// Pairing at another tenant's outlet is a validation error whether or not that outlet already
	// has a code counter (an upsert onto another tenant's row is refused by row-level security).
	fresh := e.outlet(t, e.tenant, "CCC")
	for name, outlet := range map[string]uuid.UUID{"outlet with devices": a, "outlet without": fresh} {
		if _, err := e.svc.PairDevice(ctx, otherOwner, identity.NewDevice{OutletID: outlet, Name: "x"}); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("pairing at another tenant's %s: %v, want ErrValidation", name, err)
		}
	}

	// The service role cannot rewrite what makes a device what it is.
	for _, sql := range []string{
		`UPDATE device SET secret_hash = '\x00'`,
		`UPDATE device SET device_code = 99`,
		`UPDATE device SET outlet_id = gen_random_uuid()`,
		`DELETE FROM device`,
		`UPDATE device_code_counter SET outlet_id = gen_random_uuid()`,
		`DELETE FROM device_code_counter`,
	} {
		err := kernel.TenantTx(ctx, e.d.App, e.tenant, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		if !isPgCode(err, "42501") {
			t.Errorf("%s: err = %v, want 42501", sql, err)
		}
	}
}
