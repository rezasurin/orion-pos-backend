package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
)

type staffBody struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Active      bool   `json:"active"`
	HasPin      bool   `json:"has_pin"`
	IsOwner     bool   `json:"is_owner"`
	OutletRoles []struct {
		OutletID string `json:"outlet_id"`
		RoleID   string `json:"role_id"`
	} `json:"outlet_roles"`
}

type roleBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	IsSystem    bool     `json:"is_system"`
	Permissions []string `json:"permissions"`
}

func (e *env) roles(t *testing.T, token string) map[string]roleBody {
	t.Helper()
	r := e.do(t, "GET", "/v1/roles", token, nil)
	if r.Code != http.StatusOK {
		t.Fatalf("GET /v1/roles: %d %s", r.Code, r.Body.String())
	}
	var body struct {
		Items []roleBody `json:"items"`
	}
	r.decode(t, &body)
	out := map[string]roleBody{}
	for _, role := range body.Items {
		out[role.Name] = role
	}
	return out
}

// member adds a non-owner member with a role at the fixture's outlet and signs them in.
func (e *env) member(t *testing.T, f tenantFixture, email, role string) sessionBody {
	t.Helper()
	owner := e.login(t, "owner@"+strings.ToLower(f.tenant.Slug)+".test")
	roles := e.roles(t, owner.AccessToken)
	_, err := e.ids.CreateMember(context.Background(), identity.NewMember{
		TenantID: f.tenant.ID, Email: email, Password: password, DisplayName: role, EmailVerified: true,
		OutletRoles: []identity.OutletRole{{OutletID: f.outlet.ID, RoleID: uuid.MustParse(roles[role].ID)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e.login(t, email)
}

func TestRolesRequireStaffManage(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test")
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	manager := e.member(t, f, "mgr@kopi.test", "Manager")

	roles := e.roles(t, owner.AccessToken)
	if len(roles) != 4 || len(roles["Owner"].Permissions) == 0 || !roles["Owner"].IsSystem {
		t.Fatalf("roles = %+v", roles)
	}
	if got := e.roles(t, manager.AccessToken); len(got) != 4 {
		t.Errorf("a manager sees %d roles, want 4", len(got))
	}
	e.do(t, "GET", "/v1/roles", cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/staff", cashier.AccessToken, nil).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "POST", "/v1/staff", cashier.AccessToken, map[string]any{"display_name": "X"}).problem(t, http.StatusForbidden, "forbidden")
	e.do(t, "GET", "/v1/roles", "", nil).problem(t, http.StatusUnauthorized, "invalid_token")
}

func TestMeListsPermissions(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	cashier := e.member(t, f, "cash@kopi.test", "Cashier")
	owner := e.login(t, "owner@kopi.test")

	var me struct {
		IsOwner     bool     `json:"is_owner"`
		Permissions []string `json:"permissions"`
	}
	e.do(t, "GET", "/v1/me", cashier.AccessToken, nil).decode(t, &me)
	if me.IsOwner || strings.Join(me.Permissions, ",") != "sale.create,shift.close,shift.open" {
		t.Errorf("cashier me = %+v", me)
	}
	e.do(t, "GET", "/v1/me", owner.AccessToken, nil).decode(t, &me)
	if !me.IsOwner || len(me.Permissions) != len(identity.AllPermissions()) {
		t.Errorf("owner me = %+v", me)
	}
}

func TestStaffLifecycle(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	roles := e.roles(t, tok)
	cashier, kitchen := roles["Cashier"].ID, roles["Kitchen"].ID
	outlet := f.outlet.ID.String()

	r := e.do(t, "POST", "/v1/staff", tok, map[string]any{
		"display_name": "Sari", "pin": "4821",
		"outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": cashier}},
	})
	if r.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var st staffBody
	r.decode(t, &st)
	if st.DisplayName != "Sari" || !st.Active || !st.HasPin || st.IsOwner || len(st.OutletRoles) != 1 {
		t.Errorf("created = %+v", st)
	}
	if strings.Contains(r.Body.String(), "pin_hash") || strings.Contains(r.Body.String(), "argon2") || strings.Contains(r.Body.String(), "4821") {
		t.Errorf("the response exposes the PIN or its hash: %s", r.Body.String())
	}

	// Rename and reassign.
	r = e.do(t, "PATCH", "/v1/staff/"+st.ID, tok, map[string]any{
		"display_name": "Sari W", "outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": kitchen}},
	})
	if r.Code != http.StatusOK {
		t.Fatalf("update: %d %s", r.Code, r.Body.String())
	}
	r.decode(t, &st)
	if st.DisplayName != "Sari W" || len(st.OutletRoles) != 1 || st.OutletRoles[0].RoleID != kitchen {
		t.Errorf("updated = %+v", st)
	}

	// Rotate the PIN.
	if r := e.do(t, "PUT", "/v1/staff/"+st.ID+"/pin", tok, map[string]string{"pin": "9071"}); r.Code != http.StatusNoContent {
		t.Errorf("set PIN: %d %s", r.Code, r.Body.String())
	}
	e.do(t, "PUT", "/v1/staff/"+st.ID+"/pin", tok, map[string]string{"pin": "1234"}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "PUT", "/v1/staff/"+uuid.NewString()+"/pin", tok, map[string]string{"pin": "9071"}).problem(t, http.StatusNotFound, "not_found")

	// Deactivate.
	r = e.do(t, "PATCH", "/v1/staff/"+st.ID, tok, map[string]any{"active": false})
	r.decode(t, &st)
	if r.Code != http.StatusOK || st.Active {
		t.Errorf("deactivate: %d %+v", r.Code, st)
	}

	// Validation and not-found.
	e.do(t, "POST", "/v1/staff", tok, map[string]any{"display_name": ""}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/staff", tok, "{}").problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "POST", "/v1/staff", tok, map[string]any{
		"display_name": "X", "outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": uuid.NewString()}},
	}).problem(t, http.StatusBadRequest, "validation_failed")
	e.do(t, "PATCH", "/v1/staff/"+uuid.NewString(), tok, map[string]any{"active": true}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PATCH", "/v1/staff/nope", tok, map[string]any{"active": true}).problem(t, http.StatusBadRequest, "validation_failed")
}

func TestStaffListPaging(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	for i := range 5 {
		if r := e.do(t, "POST", "/v1/staff", tok, map[string]any{"display_name": "S" + string(rune('A'+i))}); r.Code != http.StatusCreated {
			t.Fatalf("create %d: %d %s", i, r.Code, r.Body.String())
		}
	}

	var seen []string
	path := "/v1/staff?limit=2"
	for range 10 {
		r := e.do(t, "GET", path, tok, nil)
		if r.Code != http.StatusOK {
			t.Fatalf("list: %d %s", r.Code, r.Body.String())
		}
		var page struct {
			Items      []staffBody `json:"items"`
			NextCursor *string     `json:"next_cursor"`
		}
		r.decode(t, &page)
		for _, s := range page.Items {
			seen = append(seen, s.ID)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/v1/staff?limit=2&cursor=" + *page.NextCursor
	}
	if len(seen) != 6 { // five created plus the owner's own record
		t.Errorf("paged through %d staff, want 6", len(seen))
	}
	for _, bad := range []string{"limit=0", "limit=201", "limit=abc", "cursor=nope"} {
		e.do(t, "GET", "/v1/staff?"+bad, tok, nil).problem(t, http.StatusBadRequest, "validation_failed")
	}
}

func TestManagerCannotExceedTheirScope(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	owner := e.login(t, "owner@kopi.test")
	mgr := e.member(t, f, "mgr@kopi.test", "Manager")
	roles := e.roles(t, owner.AccessToken)
	outlet := f.outlet.ID.String()

	// Allowed: a cashier at their own outlet.
	if r := e.do(t, "POST", "/v1/staff", mgr.AccessToken, map[string]any{
		"display_name": "Budi", "outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": roles["Cashier"].ID}},
	}); r.Code != http.StatusCreated {
		t.Fatalf("manager creating a cashier: %d %s", r.Code, r.Body.String())
	}
	// Refused: a role stronger than the manager's own.
	p := e.do(t, "POST", "/v1/staff", mgr.AccessToken, map[string]any{
		"display_name": "Evil", "outlet_roles": []map[string]string{{"outlet_id": outlet, "role_id": roles["Owner"].ID}},
	}).problem(t, http.StatusForbidden, "forbidden")
	if d, _ := p["detail"].(string); !strings.Contains(d, "permissions you do not have") {
		t.Errorf("detail = %q", d)
	}
	// Refused: touching the owner's record.
	var list struct {
		Items []staffBody `json:"items"`
	}
	e.do(t, "GET", "/v1/staff", mgr.AccessToken, nil).decode(t, &list)
	for _, s := range list.Items {
		if s.IsOwner {
			e.do(t, "PATCH", "/v1/staff/"+s.ID, mgr.AccessToken, map[string]any{"active": false}).problem(t, http.StatusForbidden, "forbidden")
			e.do(t, "PUT", "/v1/staff/"+s.ID+"/pin", mgr.AccessToken, map[string]string{"pin": "4821"}).problem(t, http.StatusForbidden, "forbidden")
		}
	}
}

func TestStaffIsTenantScoped(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	e.business(t, "tea", "BDG1", "owner@tea.test")
	a, b := e.login(t, "owner@kopi.test").AccessToken, e.login(t, "owner@tea.test").AccessToken

	var theirs staffBody
	r := e.do(t, "POST", "/v1/staff", b, map[string]any{"display_name": "Theirs"})
	r.decode(t, &theirs)

	e.do(t, "PATCH", "/v1/staff/"+theirs.ID, a, map[string]any{"active": false}).problem(t, http.StatusNotFound, "not_found")
	e.do(t, "PUT", "/v1/staff/"+theirs.ID+"/pin", a, map[string]string{"pin": "4821"}).problem(t, http.StatusNotFound, "not_found")
	var list struct {
		Items []staffBody `json:"items"`
	}
	e.do(t, "GET", "/v1/staff", a, nil).decode(t, &list)
	for _, s := range list.Items {
		if s.ID == theirs.ID {
			t.Error("tenant A lists tenant B's staff")
		}
	}
	// A's roles cannot be used in B and vice versa.
	rolesA := e.roles(t, a)
	e.do(t, "POST", "/v1/staff", b, map[string]any{
		"display_name": "X", "outlet_roles": []map[string]string{{"outlet_id": uuid.NewString(), "role_id": rolesA["Cashier"].ID}},
	}).problem(t, http.StatusBadRequest, "validation_failed")
}

func TestPINChangesAreRateLimited(t *testing.T) {
	e := newEnv(t)
	e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken
	var st staffBody
	e.do(t, "POST", "/v1/staff", tok, map[string]any{"display_name": "Sari"}).decode(t, &st)

	var limited bool
	for range 15 {
		r := e.do(t, "PUT", "/v1/staff/"+st.ID+"/pin", tok, map[string]string{"pin": "4821"})
		if r.Code == http.StatusTooManyRequests {
			limited = true
			r.problem(t, http.StatusTooManyRequests, "rate_limited")
			break
		}
	}
	if !limited {
		t.Error("15 PIN changes in a row were all accepted")
	}
}

// Requests made through the API are attributed in the audit log to the signed-in user and the
// address they came from, and the log never contains a PIN.
func TestAuditLogNamesTheActor(t *testing.T) {
	e := newEnv(t)
	f := e.business(t, "kopi", "JKT1", "owner@kopi.test")
	tok := e.login(t, "owner@kopi.test").AccessToken

	var st staffBody
	e.do(t, "POST", "/v1/staff", tok, map[string]any{"display_name": "Sari", "pin": "4821"}).decode(t, &st)
	e.do(t, "PUT", "/v1/staff/"+st.ID+"/pin", tok, map[string]string{"pin": "9071"})

	rows, err := e.d.Owner.Query(context.Background(),
		`SELECT action, actor_type, actor_id, host(ip), detail::text FROM tenant_audit_log
		 WHERE target_id = $1 ORDER BY id`, uuid.MustParse(st.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var action, actorType, ip, detail string
		var actor uuid.UUID
		if err := rows.Scan(&action, &actorType, &actor, &ip, &detail); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
		if actorType != "user" || actor != f.owner.ID || ip != "192.0.2.1" {
			t.Errorf("%s: actor %s %s from %s, want user %s from 192.0.2.1", action, actorType, actor, ip, f.owner.ID)
		}
		if strings.Contains(detail, "4821") || strings.Contains(detail, "9071") {
			t.Errorf("%s: detail leaks a PIN: %s", action, detail)
		}
	}
	if strings.Join(actions, ",") != "staff.created,staff.pin_rotated" {
		t.Errorf("audit actions = %v", actions)
	}
}
