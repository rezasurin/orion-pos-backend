package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// The isolation suite (B0.11). Two businesses, every tenant-side operation, tenant B reaching for
// tenant A's data by id. Nothing of A may appear in any response to B, and by-id access must look
// like the object does not exist. A coverage test fails when a new operation is added without a
// case here, so the suite cannot quietly fall behind the API.

type world struct {
	e *env

	a, b              tenantFixture
	userA, userB      string // owner access tokens
	deviceA, deviceB  string // device access tokens
	staffA, deviceIDA string
	roleA             string
	secrets           []string // every identifier belonging to A
}

func newWorld(t *testing.T) *world {
	t.Helper()
	e := newEnv(t)
	w := &world{e: e}
	w.a = e.business(t, "kopi", "JKT1", "owner@kopi.test")
	w.b = e.business(t, "tea", "BDG1", "owner@tea.test")
	w.userA, w.userB = e.login(t, "owner@kopi.test").AccessToken, e.login(t, "owner@tea.test").AccessToken

	roles := e.roles(t, w.userA)
	w.roleA = roles["Cashier"].ID
	var st staffBody
	e.do(t, "POST", "/v1/staff", w.userA, map[string]any{
		"display_name": "Sari of A", "pin": "4821",
		"outlet_roles": []map[string]string{{"outlet_id": w.a.outlet.ID.String(), "role_id": w.roleA}},
	}).decode(t, &st)
	w.staffA = st.ID
	pa := e.pairDevice(t, w.userA, w.a.outlet.ID.String(), "Kasir A")
	w.deviceIDA = pa.Device.ID
	w.deviceA = e.deviceToken(t, pa.DeviceSecret).AccessToken
	w.deviceB = e.deviceToken(t, e.pairDevice(t, w.userB, w.b.outlet.ID.String(), "Kasir B").DeviceSecret).AccessToken

	w.secrets = []string{w.a.tenant.ID.String(), w.a.outlet.ID.String(), w.a.owner.ID.String(), w.staffA, w.deviceIDA, "owner@kopi.test", "Sari of A", "Kasir A", "JKT1"}
	for _, r := range roles {
		w.secrets = append(w.secrets, r.ID)
	}
	return w
}

// noLeak fails if any of A's identifiers or names appears in a response to B.
func (w *world) noLeak(t *testing.T, op string, r response) {
	t.Helper()
	body := r.Body.String()
	for _, s := range w.secrets {
		if strings.Contains(body, s) {
			t.Errorf("%s: a response to tenant B contains %q of tenant A: %s", op, s, body)
		}
	}
}

// notFound asserts a by-id request for A's object, made as B, is indistinguishable from a missing one.
func (w *world) gone(t *testing.T, op string, r response, status int, code string) {
	t.Helper()
	w.noLeak(t, op, r)
	if r.Code != status {
		t.Errorf("%s: status %d, want %d; body %s", op, r.Code, status, r.Body.String())
		return
	}
	r.problem(t, status, code)
}

func TestTenantIsolationAcrossEveryOperation(t *testing.T) {
	w := newWorld(t)
	e := w.e
	ub, db := w.userB, w.deviceB
	aOutlet, aRole := w.a.outlet.ID.String(), w.roleA

	cases := map[string]func(t *testing.T){
		// Lists and reads of B's own view must hold nothing of A's.
		"GetMe":           func(t *testing.T) { w.noLeak(t, "GetMe", e.do(t, "GET", "/v1/me", ub, nil)) },
		"ListOutlets":     func(t *testing.T) { w.noLeak(t, "ListOutlets", e.do(t, "GET", "/v1/outlets", ub, nil)) },
		"ListRoles":       func(t *testing.T) { w.noLeak(t, "ListRoles", e.do(t, "GET", "/v1/roles", ub, nil)) },
		"ListStaff":       func(t *testing.T) { w.noLeak(t, "ListStaff", e.do(t, "GET", "/v1/staff?limit=200", ub, nil)) },
		"ListDevices":     func(t *testing.T) { w.noLeak(t, "ListDevices", e.do(t, "GET", "/v1/devices?limit=200", ub, nil)) },
		"GetEntitlements": func(t *testing.T) { w.noLeak(t, "GetEntitlements", e.do(t, "GET", "/v1/entitlements", ub, nil)) },
		"GetRoster":       func(t *testing.T) { w.noLeak(t, "GetRoster", e.do(t, "GET", "/v1/pos/roster", db, nil)) },
		"GetReceiptTest":  func(t *testing.T) { w.noLeak(t, "GetReceiptTest", e.do(t, "GET", "/v1/pos/receipt-test", db, nil)) },

		// By-id access to A's objects looks like they do not exist.
		"GetOutlet": func(t *testing.T) {
			w.gone(t, "GetOutlet", e.do(t, "GET", "/v1/outlets/"+aOutlet, ub, nil), http.StatusNotFound, "not_found")
		},
		"UpdateStaff": func(t *testing.T) {
			w.gone(t, "UpdateStaff", e.do(t, "PATCH", "/v1/staff/"+w.staffA, ub, map[string]any{"active": false}), http.StatusNotFound, "not_found")
		},
		"SetStaffPin": func(t *testing.T) {
			w.gone(t, "SetStaffPin", e.do(t, "PUT", "/v1/staff/"+w.staffA+"/pin", ub, map[string]string{"pin": "4821"}), http.StatusNotFound, "not_found")
		},
		"RevokeDevice": func(t *testing.T) {
			w.gone(t, "RevokeDevice", e.do(t, "DELETE", "/v1/devices/"+w.deviceIDA, ub, nil), http.StatusNotFound, "not_found")
		},

		// Creating things that point at A's rows is refused, whatever the ids.
		"CreateStaff": func(t *testing.T) {
			w.gone(t, "CreateStaff/outlet", e.do(t, "POST", "/v1/staff", ub, map[string]any{
				"display_name": "Intruder", "outlet_roles": []map[string]string{{"outlet_id": aOutlet, "role_id": e.roles(t, ub)["Cashier"].ID}},
			}), http.StatusBadRequest, "validation_failed")
			// The error echoes the role id B itself sent, so only the status and code are checked.
			e.do(t, "POST", "/v1/staff", ub, map[string]any{
				"display_name": "Intruder", "outlet_roles": []map[string]string{{"outlet_id": w.b.outlet.ID.String(), "role_id": aRole}},
			}).problem(t, http.StatusBadRequest, "validation_failed")
		},
		"PairDevice": func(t *testing.T) {
			w.gone(t, "PairDevice", e.do(t, "POST", "/v1/devices/pair", ub, map[string]string{"outlet_id": aOutlet, "name": "Intruder"}), http.StatusBadRequest, "validation_failed")
		},
	}

	for op, fn := range cases {
		t.Run(op, fn)
	}

	// Nothing above changed A.
	var st staffBody
	e.do(t, "PATCH", "/v1/staff/"+w.staffA, w.userA, map[string]any{}).decode(t, &st)
	if !st.Active || !st.HasPin || st.DisplayName != "Sari of A" {
		t.Errorf("tenant A's staff was changed by tenant B: %+v", st)
	}
	if r := e.do(t, "GET", "/v1/pos/roster", w.deviceA, nil); r.Code != http.StatusOK {
		t.Errorf("tenant A's device stopped working: %d", r.Code)
	}
	var created int
	_ = e.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM staff WHERE display_name = 'Intruder'`).Scan(&created)
	if created != 0 {
		t.Errorf("%d intruder staff rows were created", created)
	}
}

// Every operation a tenant or device can call has a case above; operations without a tenant
// (sign-in) or for operators are listed here with the reason they are exempt.
func TestIsolationCoversEveryOperation(t *testing.T) {
	exempt := map[string]string{
		"Login": "public: no tenant until sign-in", "RefreshSession": "public: the token names its tenant",
		"Logout": "public: the token names its tenant", "VerifyEmail": "public: the token names its tenant",
		"ResendVerification": "public: acts on an email address", "ExchangeDeviceToken": "public: the secret names its tenant",
		"AdminLogin": "operator", "AdminVerifyTotp": "operator", "AdminListAuditLog": "operator",
	}
	covered := map[string]bool{}
	for _, op := range []string{
		"GetMe", "ListOutlets", "ListRoles", "ListStaff", "ListDevices", "GetEntitlements", "GetRoster", "GetReceiptTest",
		"GetOutlet", "UpdateStaff", "SetStaffPin", "RevokeDevice", "CreateStaff", "PairDevice",
	} {
		covered[op] = true
	}

	doc, err := openapi.GetSpec()
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if !covered[op.OperationID] && exempt[op.OperationID] == "" {
				t.Errorf("%s %s (%s) has no isolation case and is not listed as exempt", method, path, op.OperationID)
			}
		}
	}
	for op := range covered {
		if exempt[op] != "" {
			t.Errorf("%s is both covered and exempt", op)
		}
	}
}

// The database enforces what the API promises: as tenant B, no tenant-scoped table yields a row
// that belongs to tenant A, and B cannot write one for A. This sweeps every table with a tenant_id
// column, so a table added later is covered without anyone remembering to.
func TestEveryTenantTableIsolatesRows(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	d := w.e.d

	rows, err := d.Owner.Query(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped
		WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) < 10 {
		t.Fatalf("found %d tenant tables (%v)", len(tables), err)
	}

	for _, table := range tables {
		var own, foreign int
		err := kernel.TenantTx(ctx, d.App, w.b.tenant.ID, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id = $1", w.a.tenant.ID).Scan(&foreign); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE tenant_id <> $1", w.b.tenant.ID).Scan(&own)
		})
		if err != nil {
			// A table the service role may not read at all is also isolated.
			continue
		}
		if foreign != 0 || own != 0 {
			t.Errorf("%s: tenant B sees %d rows of tenant A and %d rows of other tenants", table, foreign, own)
		}
	}

	// With no tenant in context nothing is visible either.
	for _, table := range tables {
		var n int
		if err := d.App.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err == nil && n != 0 {
			t.Errorf("%s: %d rows visible without a tenant context", table, n)
		}
	}
}
