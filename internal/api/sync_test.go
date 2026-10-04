package api_test

import (
	"encoding/json"
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

type pullBody struct {
	Cursor          string              `json:"cursor"`
	Snapshot        bool                `json:"snapshot"`
	HasMore         bool                `json:"has_more"`
	Outlet          map[string]any      `json:"outlet"`
	Categories      []categoryBody      `json:"categories"`
	Items           []itemBody          `json:"items"`
	ModifierGroups  []groupBody         `json:"modifier_groups"`
	OutletVariants  []outletVariantBody `json:"outlet_variants"`
	Staff           []map[string]any    `json:"staff"`
	RemovedStaffIDs []string            `json:"removed_staff_ids"`
	Entitlements    struct {
		Items     []map[string]any `json:"items"`
		ExpiresAt string           `json:"expires_at"`
	} `json:"entitlements"`
}

func TestPullOverHTTP(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, f, owner, "Kasir 1")
	var item itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Latte", "variants": []map[string]any{{"name": "Hot", "base_price": 28000}}}, &item)

	r := e.do(t, "GET", "/v1/sync/pull", p.token, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", r.Code, r.Body.String())
	}
	var snap pullBody
	r.decode(t, &snap)
	if !snap.Snapshot || snap.HasMore || snap.Cursor == "" || len(snap.Items) != 1 || snap.Items[0].Name != "Latte" || snap.Outlet == nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Staff) != 2 || len(snap.Entitlements.Items) == 0 || snap.Entitlements.ExpiresAt == "" {
		t.Errorf("roster %d, entitlements %d, expires %q", len(snap.Staff), len(snap.Entitlements.Items), snap.Entitlements.ExpiresAt)
	}
	// The response has every list, even when empty, so a client never has to guess null from absent.
	for _, key := range []string{"categories", "items", "modifier_groups", "outlet_variants", "staff", "removed_staff_ids", "deleted"} {
		var raw map[string]json.RawMessage
		r.decode(t, &raw)
		if string(raw[key]) == "null" || raw[key] == nil {
			t.Errorf("%s is %s, want a list", key, raw[key])
		}
	}

	// A change arrives as a delta.
	newName := "Latte (renamed)"
	e.do(t, "PATCH", "/v1/items/"+item.ID, owner, map[string]any{"name": newName})
	var delta pullBody
	e.do(t, "GET", "/v1/sync/pull?cursor="+snap.Cursor, p.token, nil).decode(t, &delta)
	if delta.Snapshot || len(delta.Items) != 1 || delta.Items[0].Name != newName || delta.Outlet != nil || delta.Cursor == snap.Cursor {
		t.Errorf("delta = %+v", delta)
	}

	// Rules.
	e.do(t, "GET", "/v1/sync/pull", "", nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/sync/pull", owner, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/sync/pull?limit=0", p.token, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/sync/pull?limit=1001", p.token, nil).problem(t, http.StatusBadRequest, "validation_failed")
	var limited pullBody
	r = e.do(t, "GET", "/v1/sync/pull?cursor=garbage&limit=1", p.token, nil)
	r.decode(t, &limited)
	if r.Code != http.StatusOK || !limited.Snapshot {
		t.Errorf("an unreadable cursor: %d snapshot=%v", r.Code, limited.Snapshot)
	}
	if rr := e.do(t, "DELETE", "/v1/devices/"+p.deviceID, owner, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rr.Code)
	}
	e.do(t, "GET", "/v1/sync/pull", p.token, nil).problem(t, http.StatusForbidden, "device_revoked")
}

type reportCashBody struct {
	OpeningCash int64  `json:"opening_cash"`
	Received    int64  `json:"received"`
	Refunded    int64  `json:"refunded"`
	PayIn       int64  `json:"pay_in"`
	PayOut      int64  `json:"pay_out"`
	Expected    int64  `json:"expected"`
	Counted     *int64 `json:"counted"`
	Difference  *int64 `json:"difference"`
}

type totalsBody struct {
	Count         int64 `json:"count"`
	Subtotal      int64 `json:"subtotal"`
	Discounts     int64 `json:"discounts"`
	Net           int64 `json:"net"`
	ServiceCharge int64 `json:"service_charge"`
	Tax           int64 `json:"tax"`
	Total         int64 `json:"total"`
	Rounding      int64 `json:"rounding"`
}

func TestReportsOverHTTP(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	manager := e.member(t, f, "mgr@kopi.test", "Manager")
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	p := e.pusher(t, f, owner, "Kasir 1")

	var item itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Espresso", "variants": []map[string]any{{"base_price": 18000}}}, &item)
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	shift := p.event("shift.opened", map[string]any{"opening_cash": 100000})
	shift["device_time"] = at
	sale := p.event("sale.completed", map[string]any{
		"shift_id": shift["id"], "receipt_number": "JKT1-01-000001", "catalog_seq": 1000,
		"pricing": map[string]any{
			"version": 1, "price_includes_tax": false, "tax_rate_bp": 0, "service_charge_rate_bp": 0,
			"service_charge_taxable": true, "cash_rounding_unit": 0, "cash_rounding_mode": "nearest",
		},
		"lines": []map[string]any{{
			"variant_id": item.Variants[0].ID, "name": "Espresso", "unit_price": 18000, "quantity": 2,
			"discount": 0, "allocated_bill_discount": 0, "total": 36000,
		}},
		"totals":   map[string]any{"subtotal": 36000, "discount_total": 0, "service_charge": 0, "tax": 0, "rounding_amount": 0, "total": 36000},
		"payments": []map[string]any{{"method": "cash", "amount": 36000, "tendered": 40000, "change": 4000}},
	})
	sale["device_time"] = at
	for _, got := range func() []syncResultBody {
		var res pushBody
		p.push(t, shift, sale).decode(t, &res)
		return res.Results
	}() {
		if got.Status != "accepted" || got.Code != "" {
			t.Fatalf("push: %+v", got)
		}
	}

	var sr struct {
		Shift struct {
			ID       string  `json:"id"`
			ClosedAt *string `json:"closed_at"`
		} `json:"shift"`
		Cash           reportCashBody `json:"cash"`
		Sales          totalsBody     `json:"sales"`
		PaymentMethods []struct {
			Method string `json:"method"`
			Amount int64  `json:"amount"`
		} `json:"payment_methods"`
	}
	path := "/v1/reports/shifts/" + shift["id"].(string)
	r := e.do(t, "GET", path, owner, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("shift report: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &sr)
	// 100000 opening + 36000 cash taken for the bill (the 4000 change went back to the customer).
	if sr.Cash.Expected != 136000 || sr.Cash.Received != 36000 || sr.Cash.Counted != nil || sr.Sales.Count != 1 || sr.Sales.Total != 36000 ||
		len(sr.PaymentMethods) != 1 || sr.PaymentMethods[0].Method != "cash" || sr.PaymentMethods[0].Amount != 36000 || sr.Shift.ClosedAt != nil {
		t.Errorf("shift report = %+v", sr)
	}
	if r := e.do(t, "GET", path, manager.AccessToken, nil); r.Code != http.StatusOK {
		t.Errorf("a manager reads reports: %d", r.Code)
	}
	e.do(t, "GET", path, cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", path, p.token, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/reports/shifts/"+uuid.NewString(), owner, nil).problem(t, http.StatusNotFound, "not_found")

	// The day it belongs to, in the outlet's time zone.
	local := time.Now().UTC().Add(-time.Hour).In(time.FixedZone("WIB", 7*3600)).Format(time.DateOnly)
	dayPath := "/v1/reports/days/" + local + "?outlet_id=" + f.outlet.ID.String()
	var dr struct {
		Date       string     `json:"date"`
		Sales      totalsBody `json:"sales"`
		OpenShifts int        `json:"open_shifts"`
		Shifts     []struct {
			Cash reportCashBody `json:"cash"`
		} `json:"shifts"`
		Flags map[string]int64 `json:"flags"`
	}
	r = e.do(t, "GET", dayPath, owner, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("day report: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &dr)
	if dr.Date != local || dr.Sales.Count != 1 || dr.Sales.Total != 36000 || dr.OpenShifts != 1 || len(dr.Shifts) != 1 || dr.Shifts[0].Cash.Expected != 136000 {
		t.Errorf("day report = %+v", dr)
	}
	// The cashier who rang it up has no permission to see reports: nothing flagged for the sale
	// itself, since a cashier may sell and open shifts.
	if len(dr.Flags) != 0 {
		t.Errorf("flags = %v", dr.Flags)
	}

	e.do(t, "GET", dayPath, cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/reports/days/"+local, owner, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/reports/days/not-a-date?outlet_id="+f.outlet.ID.String(), owner, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/reports/days/"+local+"?outlet_id="+uuid.NewString(), owner, nil).problem(t, http.StatusNotFound, "not_found")
	empty := e.do(t, "GET", "/v1/reports/days/2020-01-01?outlet_id="+f.outlet.ID.String(), owner, nil)
	if empty.Code != http.StatusOK {
		t.Errorf("an empty day: %d %s", empty.Code, empty.Body.String())
	}
}

func TestSalesListAndDetailOverHTTP(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	manager := e.member(t, f, "mgr@kopi.test", "Manager")
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	p := e.pusher(t, f, owner, "Kasir 1")

	var item itemBody
	e.create(t, "/v1/items", owner, map[string]any{"name": "Espresso", "variants": []map[string]any{{"base_price": 18000}}}, &item)
	at := time.Now().UTC().Add(-time.Hour)
	shift := p.event("shift.opened", map[string]any{"opening_cash": 0})
	shift["device_time"] = at.Add(-time.Hour).Format(time.RFC3339)
	events := []map[string]any{shift}
	var saleIDs []string
	for i := 0; i < 5; i++ {
		sale := p.event("sale.completed", map[string]any{
			"shift_id": shift["id"], "receipt_number": fmt.Sprintf("JKT1-01-%06d", i+1), "catalog_seq": 1000,
			"pricing": map[string]any{
				"version": 1, "price_includes_tax": false, "tax_rate_bp": 0, "service_charge_rate_bp": 0,
				"service_charge_taxable": true, "cash_rounding_unit": 0, "cash_rounding_mode": "nearest",
			},
			"lines": []map[string]any{{
				"variant_id": item.Variants[0].ID, "name": "Espresso", "unit_price": 18000, "quantity": i + 1,
				"discount": 0, "allocated_bill_discount": 0, "total": 18000 * (i + 1),
			}},
			"totals":   map[string]any{"subtotal": 18000 * (i + 1), "discount_total": 0, "service_charge": 0, "tax": 0, "rounding_amount": 0, "total": 18000 * (i + 1)},
			"payments": []map[string]any{{"method": "qris_manual", "amount": 18000 * (i + 1), "reference": "QR"}},
		})
		sale["device_time"] = at.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		events = append(events, sale)
		saleIDs = append(saleIDs, sale["id"].(string))
	}
	r := p.push(t, events...)
	var pushed pushBody
	r.decode(t, &pushed)
	for _, got := range pushed.Results {
		if got.Status != "accepted" || got.Code != "" {
			t.Fatalf("push: %+v", got)
		}
	}

	type summary struct {
		ID            string `json:"id"`
		ReceiptNumber string `json:"receipt_number"`
		Total         int64  `json:"total"`
		Status        string `json:"status"`
		Payments      []struct {
			Method string `json:"method"`
			Amount int64  `json:"amount"`
		} `json:"payments"`
		FlagCodes []string `json:"flag_codes"`
	}
	var page struct {
		Items      []summary `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}

	// Newest first by the device's clock, in pages.
	r = e.do(t, "GET", "/v1/sales?limit=2", owner, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("list: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &page)
	if len(page.Items) != 2 || page.NextCursor == nil || page.Items[0].ID != saleIDs[4] || page.Items[1].ID != saleIDs[3] {
		t.Fatalf("first page: %+v", page)
	}
	if page.Items[0].Total != 90000 || len(page.Items[0].Payments) != 1 || page.Items[0].Payments[0].Method != "qris_manual" || page.Items[0].FlagCodes == nil {
		t.Errorf("summary: %+v", page.Items[0])
	}
	var second struct {
		Items      []summary `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}
	e.do(t, "GET", "/v1/sales?limit=2&cursor="+*page.NextCursor, owner, nil).decode(t, &second)
	if len(second.Items) != 2 || second.Items[0].ID != saleIDs[2] || second.NextCursor == nil {
		t.Errorf("second page: %+v", second)
	}

	// Filters and what a person may see.
	var filtered struct {
		Items []summary `json:"items"`
	}
	e.do(t, "GET", "/v1/sales?receipt_number=JKT1-01-000003", owner, nil).decode(t, &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].ID != saleIDs[2] {
		t.Errorf("by receipt: %+v", filtered)
	}
	filtered.Items = nil
	e.do(t, "GET", "/v1/sales?status=voided", owner, nil).decode(t, &filtered)
	if len(filtered.Items) != 0 {
		t.Errorf("voided: %+v", filtered)
	}
	if r := e.do(t, "GET", "/v1/sales?outlet_id="+f.outlet.ID.String(), manager.AccessToken, nil); r.Code != http.StatusOK {
		t.Errorf("a manager lists sales: %d", r.Code)
	}
	e.do(t, "GET", "/v1/sales", cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/sales", p.token, nil).problem(t, http.StatusUnauthorized, "invalid_token")
	e.do(t, "GET", "/v1/sales?outlet_id="+uuid.NewString(), owner, nil).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "GET", "/v1/sales?cursor=junk", owner, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/sales?status=bogus", owner, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/sales?limit=0", owner, nil).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "GET", "/v1/sales?from=yesterday", owner, nil).problem(t, http.StatusBadRequest, "validation_failed")

	// One sale in full.
	var detail struct {
		summary
		Lines []struct {
			Name      string `json:"name"`
			Quantity  int    `json:"quantity"`
			Modifiers []any  `json:"modifiers"`
		} `json:"lines"`
		Discounts []any          `json:"discounts"`
		Void      map[string]any `json:"void"`
		Flags     []any          `json:"flags"`
		Pricing   map[string]any `json:"pricing"`
	}
	r = e.do(t, "GET", "/v1/sales/"+saleIDs[1], owner, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &detail)
	if detail.ID != saleIDs[1] || detail.ReceiptNumber != "JKT1-01-000002" || len(detail.Lines) != 1 || detail.Lines[0].Quantity != 2 ||
		detail.Lines[0].Modifiers == nil || detail.Void != nil || detail.Flags == nil || detail.Discounts == nil || detail.Pricing["version"] != float64(1) {
		t.Errorf("detail = %+v", detail)
	}
	e.do(t, "GET", "/v1/sales/"+saleIDs[1], cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/sales/"+uuid.NewString(), owner, nil).problem(t, http.StatusNotFound, "not_found")
}
