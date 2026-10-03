package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

type deviceBody struct {
	ID          string  `json:"id"`
	OutletID    string  `json:"outlet_id"`
	DeviceCode  int     `json:"device_code"`
	Name        string  `json:"name"`
	RevokedAt   *string `json:"revoked_at"`
	LastSeenAt  *string `json:"last_seen_at"`
	AppVersion  *string `json:"app_version"`
	ClockSkewMs *int    `json:"clock_skew_ms"`
}

type pairedBody struct {
	Device       deviceBody `json:"device"`
	DeviceSecret string     `json:"device_secret"`
}

type deviceSessionBody struct {
	TokenType   string `json:"token_type"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
	OutletID    string `json:"outlet_id"`
	TenantID    string `json:"tenant_id"`
}

func (e *env) pairDevice(t *testing.T, token, outlet, name string) pairedBody {
	t.Helper()
	r := e.do(t, "POST", "/v1/devices/pair", token, map[string]string{"outlet_id": outlet, "name": name})
	if r.Code != http.StatusCreated {
		t.Fatalf("pair: %d %s", r.Code, r.Body.String())
	}
	var p pairedBody
	r.decode(t, &p)
	return p
}

func (e *env) deviceToken(t *testing.T, secret string) deviceSessionBody {
	t.Helper()
	r := e.do(t, "POST", "/v1/devices/token", "", map[string]any{"device_secret": secret, "app_version": "1.0.0", "client_time": time.Now().Format(time.RFC3339)})
	if r.Code != http.StatusOK {
		t.Fatalf("device token: %d %s", r.Code, r.Body.String())
	}
	var s deviceSessionBody
	r.decode(t, &s)
	return s
}

// The Phase 0 exit on the device side: a tablet pairs, gets a token, and downloads the roster.
func TestPairingToRoster(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	roles := e.roles(t, owner)
	outlet := f.outlet.ID.String()

	var sari staffBody
	e.do(t, "POST", "/v1/staff", owner, map[string]any{
		"display_name": "Sari", "pin": "4821",
		"outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": roles["Cashier"].ID}},
	}).decode(t, &sari)

	paired := e.pairDevice(t, owner, outlet, "Kasir 1")
	if paired.Device.DeviceCode != 1 || paired.Device.Name != "Kasir 1" || paired.Device.OutletID != outlet || !strings.HasPrefix(paired.DeviceSecret, "dk1.") {
		t.Errorf("paired = %+v", paired)
	}

	sess := e.deviceToken(t, paired.DeviceSecret)
	if sess.TokenType != "Bearer" || sess.DeviceID != paired.Device.ID || sess.OutletID != outlet || sess.TenantID != f.tenant.ID.String() {
		t.Errorf("device session = %+v", sess)
	}

	r := e.do(t, "GET", "/v1/pos/roster", sess.AccessToken, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("roster: %d %s", r.Code, r.Body.String())
	}
	var roster struct {
		Device deviceBody `json:"device"`
		Outlet struct {
			ID       string `json:"id"`
			Code     string `json:"code"`
			Settings struct {
				Timezone string `json:"timezone"`
			} `json:"settings"`
		} `json:"outlet"`
		Staff []struct {
			ID          string   `json:"id"`
			DisplayName string   `json:"display_name"`
			PinHash     *string  `json:"pin_hash"`
			Permissions []string `json:"permissions"`
		} `json:"staff"`
		GeneratedAt string `json:"generated_at"`
	}
	r.decode(t, &roster)
	if roster.Device.ID != paired.Device.ID || roster.Device.DeviceCode != 1 || roster.Outlet.Code != "JKT1" || roster.Outlet.Settings.Timezone != "Asia/Jakarta" || roster.GeneratedAt == "" {
		t.Errorf("roster header = %+v", roster)
	}
	var found bool
	for _, s := range roster.Staff {
		if s.ID != sari.ID {
			continue
		}
		found = true
		if s.PinHash == nil || !strings.HasPrefix(*s.PinHash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Join(s.Permissions, ",") != "sale.create,shift.close,shift.open" {
			t.Errorf("Sari on the roster = %+v", s)
		}
	}
	if !found || len(roster.Staff) != 2 { // Sari and the owner
		t.Errorf("roster staff = %d (Sari found: %v), want Sari and the owner", len(roster.Staff), found)
	}
}

func TestDeviceAndUserTokensStayInTheirLanes(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	user := e.login(t, "owner@kopi.test").AccessToken
	device := e.deviceToken(t, e.pairDevice(t, user, f.outlet.ID.String(), "Kasir 1").DeviceSecret).AccessToken

	// A device cannot call back-office routes, and a user cannot call device routes.
	for _, path := range []string{"/v1/me", "/v1/outlets", "/v1/staff", "/v1/roles", "/v1/devices"} {
		e.do(t, "GET", path, device, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	}
	e.do(t, "GET", "/v1/pos/roster", user, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/pos/roster", "", nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "POST", "/v1/devices/pair", device, map[string]string{"outlet_id": f.outlet.ID.String(), "name": "x"}).
		problem(t, http.StatusUnauthorized, "invalid_token")
	// A refresh token or a device secret is not an access token.
	e.do(t, "GET", "/v1/pos/roster", e.pairDevice(t, user, f.outlet.ID.String(), "Kasir 2").DeviceSecret, nil).
		problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestRevokedDeviceIsCutOff(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	paired := e.pairDevice(t, owner, f.outlet.ID.String(), "Kasir 1")
	token := e.deviceToken(t, paired.DeviceSecret).AccessToken
	if r := e.do(t, "GET", "/v1/pos/roster", token, nil); r.Code != http.StatusOK {
		t.Fatalf("roster before revoking: %d", r.Code)
	}

	if r := e.do(t, "DELETE", "/v1/devices/"+paired.Device.ID, owner, nil); r.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.Code, r.Body.String())
	}
	// The token it already holds stops working at once, and it cannot get a new one.
	e.do(t, "GET", "/v1/pos/roster", token, nil).problem(t, http.StatusForbidden, "device_revoked")
	e.do(t, "POST", "/v1/devices/token", "", map[string]string{"device_secret": paired.DeviceSecret}).
		problem(t, http.StatusForbidden, "device_revoked")
	// Idempotent, and unknown devices are 404.
	if r := e.do(t, "DELETE", "/v1/devices/"+paired.Device.ID, owner, nil); r.Code != http.StatusNoContent {
		t.Errorf("second revoke: %d", r.Code)
	}
	e.do(t, "DELETE", "/v1/devices/"+uuid.NewString(), owner, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "DELETE", "/v1/devices/nope", owner, nil).problem(t, http.StatusBadRequest, "validation_failed")

	var list struct {
		Items []deviceBody `json:"items"`
	}
	e.do(t, "GET", "/v1/devices", owner, nil).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].RevokedAt == nil {
		t.Errorf("devices = %+v, want the revoked device listed with revoked_at", list.Items)
	}
}

func TestDeviceTokenErrors(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	paired := e.pairDevice(t, owner, f.outlet.ID.String(), "Kasir 1")

	e.do(t, "POST", "/v1/devices/token", "", map[string]string{"device_secret": ""}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/devices/token", "", "{}").problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/devices/token", "", map[string]string{"device_secret": "dk1.nope"}).problem(t, http.StatusUnauthorized, "invalid_token")
	forged := paired.DeviceSecret[:len(paired.DeviceSecret)-3] + "AAA"
	e.do(t, "POST", "/v1/devices/token", "", map[string]string{"device_secret": forged}).problem(t, http.StatusUnauthorized, "invalid_token")

	e.d.Exec(t, `UPDATE tenant SET suspended_at = now() WHERE id = $1`, f.tenant.ID)
	e.do(t, "POST", "/v1/devices/token", "", map[string]string{"device_secret": paired.DeviceSecret}).problem(t, http.StatusForbidden, "tenant_suspended")
}

func TestPairingPermissionsAndValidation(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	mgr := e.member(t, f, "mgr@kopi.test", "Manager").AccessToken
	cash := e.member(t, f, "cash@kopi.test", "Cashier").AccessToken
	outlet := f.outlet.ID.String()

	if p := e.pairDevice(t, mgr, outlet, "Kasir 2"); p.Device.DeviceCode != 1 {
		t.Errorf("a manager pairing at their outlet got code %d", p.Device.DeviceCode)
	}
	e.do(t, "POST", "/v1/devices/pair", cash, map[string]string{"outlet_id": outlet, "name": "x"}).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/devices", cash, nil).problem(t, http.StatusForbidden, "forbidden")

	for name, body := range map[string]any{
		"no name":        map[string]string{"outlet_id": outlet},
		"bad outlet id":  map[string]string{"outlet_id": "nope", "name": "x"},
		"unknown outlet": map[string]string{"outlet_id": uuid.NewString(), "name": "x"},
		"empty body":     "{}",
	} {
		r := e.do(t, "POST", "/v1/devices/pair", owner, body)
		if r.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, r.Code, r.Body.String())
		}
	}
	e.do(t, "POST", "/v1/devices/pair", "", map[string]string{"outlet_id": outlet, "name": "x"}).problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestDevicesAreTenantScoped(t *testing.T) {
	e := newEnv(t)
	a := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	b := e.business(t, "tea", "BDG1", "owner@tea.test")
	ta, tb := e.login(t, "owner@kopi.test").AccessToken, e.login(t, "owner@tea.test").AccessToken
	dev := e.pairDevice(t, ta, a.outlet.ID.String(), "Kasir 1")

	e.do(t, "DELETE", "/v1/devices/"+dev.Device.ID, tb, nil).problem(t, http.StatusNotFound, "not_found")
	// Pairing into another business's outlet is refused, whoever asks.
	r := e.do(t, "POST", "/v1/devices/pair", tb, map[string]string{"outlet_id": a.outlet.ID.String(), "name": "x"})
	if r.Code != http.StatusBadRequest {
		t.Errorf("pairing at another tenant's outlet: %d %s", r.Code, r.Body.String())
	}
	var list struct {
		Items []deviceBody `json:"items"`
	}
	e.do(t, "GET", "/v1/devices", tb, nil).decode(t, &list)
	if len(list.Items) != 0 {
		t.Errorf("tenant B lists %d of tenant A's devices", len(list.Items))
	}
	// B's device cannot be used with A's data: its roster is B's.
	devB := e.deviceToken(t, e.pairDevice(t, tb, b.outlet.ID.String(), "Kasir B").DeviceSecret)
	var roster struct {
		Outlet struct{ Code string } `json:"outlet"`
	}
	e.do(t, "GET", "/v1/pos/roster", devB.AccessToken, nil).decode(t, &roster)
	if roster.Outlet.Code != "BDG1" {
		t.Errorf("roster outlet = %q, want BDG1", roster.Outlet.Code)
	}
}

func TestPairingIsRateLimited(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	var limited bool
	for range 8 {
		r := e.do(t, "POST", "/v1/devices/pair", owner, map[string]string{"outlet_id": f.outlet.ID.String(), "name": "x"})
		if r.Code == http.StatusTooManyRequests {
			limited = true
			r.problem(t, http.StatusTooManyRequests, "rate_limited")
			break
		}
	}
	if !limited {
		t.Error("8 pairings in a row were all accepted")
	}
}

func TestLimitsAndEntitlementsOverHTTP(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken

	var snap struct {
		Items []struct {
			Key    string `json:"key"`
			Kind   string `json:"kind"`
			Value  int64  `json:"value"`
			Source string `json:"source"`
		} `json:"items"`
		GeneratedAt string `json:"generated_at"`
	}
	r := e.do(t, "GET", "/v1/entitlements", tok, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("entitlements: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &snap)
	got := map[string]int64{}
	for _, it := range snap.Items {
		got[it.Key] = it.Value
	}
	if len(snap.Items) != 5 || got["limit.devices"] != -1 || got["module.inventory"] != 1 || snap.GeneratedAt == "" {
		t.Errorf("early access snapshot = %+v", snap)
	}
	e.do(t, "GET", "/v1/entitlements", "", nil).problem(t, http.StatusUnauthorized, "invalid_token")

	// Cap the tenant at one device: the second pairing is a 403 limit_reached.
	e.d.Exec(t, `INSERT INTO tenant_entitlement_override (tenant_id, key, value, reason) VALUES ($1, 'limit.devices', 1, 'test')`, f.tenant.ID)
	// Limits are read inside the creating transaction, not from the snapshot cache, so the override
	// applies at once.
	e.pairDevice(t, tok, f.outlet.ID.String(), "Kasir 1")
	p := e.do(t, "POST", "/v1/devices/pair", tok, map[string]string{"outlet_id": f.outlet.ID.String(), "name": "Kasir 2"}).
		problem(t, http.StatusForbidden, "limit_reached")
	if d, _ := p["detail"].(string); d != "your plan allows 1 (limit.devices)" {
		t.Errorf("detail = %q", d)
	}
}
