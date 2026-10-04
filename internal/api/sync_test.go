package api_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

type syncResultBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type pushBody struct {
	Results    []syncResultBody `json:"results"`
	ServerTime string           `json:"server_time"`
}

// pusher is a paired device and a cashier who acts on it.
type pusher struct {
	e        *env
	token    string
	deviceID string
	staffID  string
}

func (e *env) pusher(t *testing.T, f tenantFixture, ownerToken, name string) pusher {
	t.Helper()
	roles := e.roles(t, ownerToken)
	var st staffBody
	e.do(t, "POST", "/v1/staff", ownerToken, map[string]any{
		"display_name": "Cashier " + name,
		"outlet_roles": []map[string]string{{"outlet_id": f.outlet.ID.String(), "role_id": roles["Cashier"].ID}},
	}).decode(t, &st)
	p := e.pairDevice(t, ownerToken, f.outlet.ID.String(), name)
	return pusher{e: e, token: e.deviceToken(t, p.DeviceSecret).AccessToken, deviceID: p.Device.ID, staffID: st.ID}
}

func (p pusher) event(typ string, payload map[string]any) map[string]any {
	id := uuid.NewString()
	return map[string]any{
		"id": id, "idempotency_key": id, "type": typ, "staff_id": p.staffID,
		"device_time": time.Now().UTC().Format(time.RFC3339), "schema_version": 1, "payload": payload,
	}
}

func (p pusher) push(t *testing.T, events ...map[string]any) response {
	t.Helper()
	return p.e.do(t, "POST", "/v1/sync/push", p.token, map[string]any{"device_id": p.deviceID, "events": events})
}

func TestPushEndToEnd(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")

	sale := p.event("test.created", map[string]any{"label": "kopi susu"})
	void := p.event("test.voided", map[string]any{"target": sale["id"]})
	bad := p.event("sale.nonexistent", map[string]any{})

	r := p.push(t, void, sale, bad)
	if r.Code != http.StatusOK {
		t.Fatalf("push: %d %s", r.Code, r.Body.String())
	}
	var res pushBody
	r.decode(t, &res)
	if len(res.Results) != 3 || res.ServerTime == "" {
		t.Fatalf("response = %+v", res)
	}
	// The void came first and waits; the sale released it; the unknown type is rejected.
	if got := res.Results[0]; got.ID != void["id"] || got.Status != "accepted" || got.Code != "pending_dependency" {
		t.Errorf("void: %+v", got)
	}
	if got := res.Results[1]; got.ID != sale["id"] || got.Status != "accepted" || got.Code != "" {
		t.Errorf("sale: %+v", got)
	}
	if got := res.Results[2]; got.Status != "rejected" || got.Code != "unknown_type" || got.Detail == "" {
		t.Errorf("bad: %+v", got)
	}

	var voided bool
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT voided FROM stub_record WHERE id = $1`, sale["id"]).Scan(&voided); err != nil || !voided {
		t.Errorf("sale voided = %v (%v)", voided, err)
	}

	// Pushing again changes nothing.
	r = p.push(t, void, sale, bad)
	res = pushBody{}
	r.decode(t, &res)
	if got := fmt.Sprint([]string{res.Results[0].Status, res.Results[1].Status, res.Results[2].Status}); got != "[duplicate duplicate rejected]" {
		t.Errorf("second push: %+v", res.Results)
	}

	// The device row shows it syncing, for the support view.
	var syncedAt *time.Time
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT last_sync_at FROM device WHERE id = $1`, p.deviceID).Scan(&syncedAt); err != nil || syncedAt == nil {
		t.Errorf("last_sync_at = %v (%v)", syncedAt, err)
	}
}

func TestPushRequestRules(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")
	other := e.pusher(t, f, owner, "Kasir 2")
	ev := p.event("test.created", map[string]any{"label": "x"})

	// Not authenticated, or authenticated as something other than a device.
	e.do(t, "POST", "/v1/sync/push", "", map[string]any{"device_id": p.deviceID, "events": []any{ev}}).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "POST", "/v1/sync/push", owner, map[string]any{"device_id": p.deviceID, "events": []any{ev}}).problem(t, http.StatusUnauthorized, "invalid_token")

	// The body must name the device the token belongs to.
	e.do(t, "POST", "/v1/sync/push", p.token, map[string]any{"device_id": other.deviceID, "events": []any{ev}}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/sync/push", p.token, map[string]any{"device_id": p.deviceID, "events": []any{}}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/sync/push", p.token, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/sync/push", p.token, `{"device_id": "nope", "events": []}`).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/sync/push", p.token, map[string]any{"device_id": p.deviceID, "events": []any{map[string]any{"id": "not-a-uuid"}}}).problem(t, http.StatusBadRequest, "validation_failed")

	many := make([]map[string]any, 501)
	for i := range many {
		many[i] = p.event("test.created", map[string]any{"label": "x"})
	}
	e.do(t, "POST", "/v1/sync/push", p.token, map[string]any{"device_id": p.deviceID, "events": many}).problem(t, http.StatusBadRequest, "validation_failed")
	var n int
	_ = e.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM sync_inbox`).Scan(&n)
	if n != 0 {
		t.Errorf("%d events were stored from refused requests", n)
	}
	if r := p.push(t, ev); r.Code != http.StatusOK {
		t.Errorf("a valid push: %d %s", r.Code, r.Body.String())
	}
}

func TestRevokedDevicePushIsForbidden(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")
	if r := e.do(t, "DELETE", "/v1/devices/"+p.deviceID, owner, nil); r.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.Code, r.Body.String())
	}
	p.push(t, p.event("test.created", map[string]any{"label": "x"})).problem(t, http.StatusForbidden, "device_revoked")
	var n int
	_ = e.d.Owner.QueryRow(t.Context(), `SELECT count(*) FROM stub_record`).Scan(&n)
	if n != 0 {
		t.Errorf("a revoked device wrote %d records", n)
	}
}

// A real day over HTTP: a shift, a sale priced by the device, and the sale voided.
func TestPushingRealEvents(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")

	var item itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Espresso", "variants": []map[string]any{{"base_price": 18000}}}, &item)

	shift := p.event("shift.opened", map[string]any{"opening_cash": 100000})
	sale := p.event("sale.completed", map[string]any{
		"shift_id": shift["id"], "receipt_number": "JKT1-01-000001", "catalog_seq": 1000,
		"pricing": map[string]any{
			"version": 1, "price_includes_tax": false, "tax_rate_bp": 0, "service_charge_rate_bp": 0,
			"service_charge_taxable": true, "cash_rounding_unit": 0, "cash_rounding_mode": "nearest",
		},
		"lines": []map[string]any{{
			"variant_id": item.Variants[0].ID, "name": "Espresso", "unit_price": 18000, "quantity": 1,
			"discount": 0, "allocated_bill_discount": 0, "total": 18000,
		}},
		"totals":   map[string]any{"subtotal": 18000, "discount_total": 0, "service_charge": 0, "tax": 0, "rounding_amount": 0, "total": 18000},
		"payments": []map[string]any{{"method": "cash", "amount": 18000, "tendered": 20000, "change": 2000}},
	})
	void := p.event("sale.voided", map[string]any{"sale_id": sale["id"], "reason": "wrong order"})

	r := p.push(t, void, sale, shift) // backwards: everything waits for the shift
	var res pushBody
	r.decode(t, &res)
	for i, got := range res.Results {
		if got.Status != "accepted" {
			t.Fatalf("event %d: %+v", i, got)
		}
	}
	var status, flagCode string
	var total int64
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT status, total FROM sale WHERE id = $1`, sale["id"]).Scan(&status, &total); err != nil {
		t.Fatal(err)
	}
	if status != "voided" || total != 18000 {
		t.Errorf("sale: status %s total %d", status, total)
	}
	// The cashier (not a manager) voided it, so there is a flag for review.
	if err := e.d.Owner.QueryRow(t.Context(), `SELECT code FROM flag WHERE target_type = 'void'`).Scan(&flagCode); err != nil || flagCode != "permission_missing" {
		t.Errorf("void flag = %q (%v)", flagCode, err)
	}
}
