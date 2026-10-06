package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

type adminTenantBody struct {
	ID                 string  `json:"id"`
	Slug               string  `json:"slug"`
	Plan               string  `json:"plan"`
	SubscriptionStatus string  `json:"subscription_status"`
	SuspendedAt        *string `json:"suspended_at"`
	Outlets            int     `json:"outlets"`
	Devices            int     `json:"devices"`
	LastSyncAt         *string `json:"last_sync_at"`
}

type adminEntitlementBody struct {
	Key      string `json:"key"`
	Value    int64  `json:"value"`
	Source   string `json:"source"`
	Override *struct {
		Value   int64  `json:"value"`
		Reason  string `json:"reason"`
		InForce bool   `json:"in_force"`
	} `json:"override"`
}

func TestTheOperatorConsole(t *testing.T) {
	e := newEnv(t)
	kopi := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	e.business(t, "teh", "BDG1", "owner@teh.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	p := e.pusher(t, kopi, owner, "Kasir 1")
	op := e.operator(t)
	ops := e.operatorSignIn(t, op).AccessToken
	k := kopi.tenant.ID.String()
	reason := map[string]any{"reason": "ticket 42"}
	e.do(t, "GET", "/v1/sync/pull", p.token, nil) // the tablet's first sync

	// The list, and a search.
	var list struct{ Items []adminTenantBody }
	e.do(t, "GET", "/admin/tenants", ops, nil).decode(t, &list)
	if len(list.Items) != 2 {
		t.Fatalf("tenants = %+v", list)
	}
	e.do(t, "GET", "/admin/tenants?q=KOP", ops, nil).decode(t, &list)
	if len(list.Items) != 1 || list.Items[0].Slug != "kopi" || list.Items[0].Plan != "early_access" || list.Items[0].Devices != 1 ||
		list.Items[0].Outlets != 1 || list.Items[0].LastSyncAt == nil {
		t.Errorf("search = %+v", list.Items)
	}
	e.do(t, "GET", "/admin/tenants", owner, nil).problem(t, http.StatusUnauthorized, "invalid_token")

	// One business. Overriding a limit takes effect at once for its owner, and clears back to the plan.
	detail := func() (out struct {
		Tenant       adminTenantBody        `json:"tenant"`
		Entitlements []adminEntitlementBody `json:"entitlements"`
		Devices      []struct {
			ID      string `json:"id"`
			Revoked bool   `json:"revoked"`
		} `json:"devices"`
	}) {
		t.Helper()
		e.do(t, "GET", "/admin/tenants/"+k, ops, nil).decode(t, &out)
		return out
	}
	entitlement := func(key string) adminEntitlementBody {
		t.Helper()
		for _, en := range detail().Entitlements {
			if en.Key == key {
				return en
			}
		}
		t.Fatalf("no %s", key)
		return adminEntitlementBody{}
	}
	if en := entitlement("limit.devices"); en.Value != -1 || en.Source != "plan" || en.Override != nil {
		t.Errorf("limit.devices = %+v", en)
	}
	put := func(path string, body map[string]any) response { return e.do(t, "PUT", path, ops, body) }
	ownerDevices := func() int64 {
		t.Helper()
		var ents struct {
			Items []struct {
				Key   string `json:"key"`
				Value int64  `json:"value"`
			} `json:"items"`
		}
		e.do(t, "GET", "/v1/entitlements", owner, nil).decode(t, &ents)
		for _, it := range ents.Items {
			if it.Key == "limit.devices" {
				return it.Value
			}
		}
		return -99
	}
	if ownerDevices() != -1 { // and now the server has it cached
		t.Fatal("limit.devices before the override")
	}
	if r := put("/admin/tenants/"+k+"/entitlements/limit.devices", map[string]any{"value": 1, "reason": "trial cap"}); r.Code != http.StatusNoContent {
		t.Fatalf("set override: %d %s", r.Code, r.Body.String())
	}
	if en := entitlement("limit.devices"); en.Value != 1 || en.Source != "override" || en.Override == nil || !en.Override.InForce || en.Override.Reason != "trial cap" {
		t.Errorf("after override = %+v", en)
	}
	e.do(t, "POST", "/v1/devices/pair", owner, map[string]string{"outlet_id": kopi.outlet.ID.String(), "name": "Kasir 2"}).
		problem(t, http.StatusForbidden, "limit_reached")
	if v := ownerDevices(); v != 1 {
		t.Errorf("the owner sees limit.devices = %d, not the new override", v)
	}
	put("/admin/tenants/"+k+"/entitlements/limit.devices", map[string]any{"value": nil, "reason": "trial over"})
	if en := entitlement("limit.devices"); en.Source != "plan" {
		t.Errorf("after clearing = %+v", en)
	}
	put("/admin/tenants/"+k+"/entitlements/limit.nonsense", map[string]any{"value": 1, "reason": "x"}).problem(t, http.StatusNotFound, "not_found")
	put("/admin/tenants/"+k+"/entitlements/limit.devices", map[string]any{"value": 1, "reason": " "}).problem(t, http.StatusBadRequest, "validation_failed")
	put("/admin/tenants/00000000-0000-0000-0000-000000000001/entitlements/limit.devices", map[string]any{"value": 1, "reason": "x"}).
		problem(t, http.StatusNotFound, "not_found")

	// The plan.
	if r := put("/admin/tenants/"+k+"/plan", map[string]any{"plan": "free", "reason": "trial ended"}); r.Code != http.StatusNoContent {
		t.Fatalf("set plan: %d %s", r.Code, r.Body.String())
	}
	if d := detail(); d.Tenant.Plan != "free" || d.Tenant.SubscriptionStatus != "active" {
		t.Errorf("after the plan change: %+v", d.Tenant)
	}
	put("/admin/tenants/"+k+"/plan", map[string]any{"plan": "free", "reason": "x"}).problem(t, http.StatusConflict, "conflict")
	put("/admin/tenants/"+k+"/plan", map[string]any{"plan": "platinum", "reason": "x"}).problem(t, http.StatusBadRequest, "validation_failed")
	put("/admin/tenants/"+k+"/plan", map[string]any{"plan": "early_access", "reason": "design partner"})
	if d := detail(); d.Tenant.Plan != "early_access" || d.Tenant.SubscriptionStatus != "early_access" {
		t.Errorf("back on early access: %+v", d.Tenant)
	}

	// Suspension, twice, and back.
	if r := e.do(t, "POST", "/admin/tenants/"+k+"/suspend", ops, reason); r.Code != http.StatusNoContent {
		t.Fatalf("suspend: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "POST", "/admin/tenants/"+k+"/suspend", ops, reason).problem(t, http.StatusConflict, "conflict")
	e.do(t, "POST", "/v1/categories", owner, map[string]any{"name": "Kopi"}).problem(t, http.StatusForbidden, "tenant_suspended")
	if d := detail(); d.Tenant.SuspendedAt == nil {
		t.Error("detail does not show the suspension")
	}
	e.do(t, "POST", "/admin/tenants/"+k+"/reinstate", ops, reason)
	if r := e.do(t, "POST", "/v1/categories", owner, map[string]any{"name": "Kopi"}); r.Code != http.StatusCreated {
		t.Errorf("a write after reinstatement: %d", r.Code)
	}

	// A tablet that went quiet, then revoked by the operator.
	e.d.Exec(t, `UPDATE device SET last_sync_at = now() - interval '2 days' WHERE id = $1`, p.deviceID)
	var quiet struct {
		Items []struct {
			DeviceID   string `json:"device_id"`
			TenantSlug string `json:"tenant_slug"`
		} `json:"items"`
	}
	e.do(t, "GET", "/admin/devices/stopped-syncing?quiet_minutes=60", ops, nil).decode(t, &quiet)
	if len(quiet.Items) != 1 || quiet.Items[0].DeviceID != p.deviceID || quiet.Items[0].TenantSlug != "kopi" {
		t.Errorf("stopped syncing = %+v", quiet)
	}
	if r := e.do(t, "POST", "/admin/devices/"+p.deviceID+"/revoke", ops, reason); r.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "GET", "/v1/sync/pull", p.token, nil).problem(t, http.StatusForbidden, "device_revoked")
	e.do(t, "POST", "/admin/devices/"+p.deviceID+"/revoke", ops, reason).problem(t, http.StatusConflict, "conflict")
	e.do(t, "POST", "/admin/devices/00000000-0000-0000-0000-000000000001/revoke", ops, reason).problem(t, http.StatusNotFound, "not_found")
	if d := detail(); len(d.Devices) != 1 || !d.Devices[0].Revoked {
		t.Errorf("devices = %+v", d.Devices)
	}
	// The business sees it in its own audit log, done by Orion support.
	if n := e.count(t, `SELECT count(*) FROM tenant_audit_log WHERE tenant_id = $1 AND action = 'device.revoked' AND actor_type = 'system'
		AND detail->>'by' = 'orion_support'`, kopi.tenant.ID); n != 1 {
		t.Errorf("%d tenant audit entries for the revocation", n)
	}

	// Every change is in the platform audit log, filtered to the business.
	var log struct {
		Items []struct {
			Action   string  `json:"action"`
			TenantID *string `json:"tenant_id"`
		} `json:"items"`
	}
	e.do(t, "GET", "/admin/audit-log?limit=100&tenant_id="+k, ops, nil).decode(t, &log)
	var actions []string
	for _, it := range log.Items {
		if it.TenantID == nil || *it.TenantID != k {
			t.Errorf("an entry of another tenant: %+v", it)
		}
		actions = append(actions, it.Action)
	}
	want := "[device.revoked tenant.reinstated tenant.suspended tenant.plan_changed tenant.plan_changed entitlement.override_cleared entitlement.override_set]"
	if fmt.Sprint(actions) != want {
		t.Errorf("audit = %v\nwant    %s", actions, want)
	}
}

// A flag's default reaches every business at once; modules and limits are refused.
func TestOperatorsRollOutAFlag(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test").AccessToken
	ops := e.operatorSignIn(t, e.operator(t)).AccessToken
	e.d.Exec(t, `INSERT INTO entitlement_key (key, kind, category, description, owner, is_temporary, default_value)
		VALUES ('flag.new_receipt', 'bool', 'flag', 'The new receipt layout', 'product', true, 0)`)

	var keys struct {
		Items []struct {
			Key       string           `json:"key"`
			Default   int64            `json:"default"`
			Temporary bool             `json:"temporary"`
			Plans     map[string]int64 `json:"plans"`
		} `json:"items"`
	}
	e.do(t, "GET", "/admin/entitlement-keys", ops, nil).decode(t, &keys)
	byKey := map[string]int{}
	for i, k := range keys.Items {
		byKey[k.Key] = i
	}
	if fl := keys.Items[byKey["flag.new_receipt"]]; !fl.Temporary || fl.Default != 0 || len(fl.Plans) != 0 {
		t.Errorf("flag = %+v", fl)
	}
	if dv := keys.Items[byKey["limit.devices"]]; dv.Plans["early_access"] != -1 || dv.Plans["free"] != 2 {
		t.Errorf("limit.devices = %+v", dv)
	}

	flag := func() int64 {
		t.Helper()
		var ents struct {
			Items []struct {
				Key   string `json:"key"`
				Value int64  `json:"value"`
			} `json:"items"`
		}
		e.do(t, "GET", "/v1/entitlements", owner, nil).decode(t, &ents)
		for _, it := range ents.Items {
			if it.Key == "flag.new_receipt" {
				return it.Value
			}
		}
		t.Fatal("no flag.new_receipt")
		return 0
	}
	if flag() != 0 {
		t.Fatal("flag on before the rollout")
	}
	if r := e.do(t, "PATCH", "/admin/entitlement-keys/flag.new_receipt", ops, map[string]any{"default_value": 1, "reason": "rollout"}); r.Code != http.StatusNoContent {
		t.Fatalf("rollout: %d %s", r.Code, r.Body.String())
	}
	if flag() != 1 {
		t.Error("the owner does not see the rollout at once")
	}
	e.do(t, "PATCH", "/admin/entitlement-keys/limit.devices", ops, map[string]any{"default_value": 9, "reason": "x"}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "PATCH", "/admin/entitlement-keys/flag.new_receipt", ops, map[string]any{"default_value": 2, "reason": "x"}).
		problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "PATCH", "/admin/entitlement-keys/flag.nope", ops, map[string]any{"default_value": 1, "reason": "x"}).
		problem(t, http.StatusNotFound, "not_found")
}
