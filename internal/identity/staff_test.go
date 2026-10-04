package identity_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// outlet inserts an outlet directly; identity does not depend on the tenancy module.
func (e *env) outlet(t *testing.T, tenant uuid.UUID, code string) uuid.UUID {
	t.Helper()
	id := kernel.NewID()
	e.d.Exec(t, `INSERT INTO outlet (id, tenant_id, name, code) VALUES ($1, $2, $3, $3)`, id, tenant, code)
	return id
}

func (e *env) role(t *testing.T, tenant uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := e.d.Owner.QueryRow(context.Background(),
		`SELECT id FROM role WHERE tenant_id = $1 AND name = $2`, tenant, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// caller signs a member in and loads what they may do, as the HTTP middleware does.
func (e *env) caller(t *testing.T, email string) identity.Caller {
	t.Helper()
	s := e.login(t, email)
	p, err := e.svc.Authenticate(identity.AudienceTenant, s.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.LoadAccess(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return identity.Caller{Principal: p, Access: a}
}

// manager adds a non-owner member holding a role at an outlet.
func (e *env) manager(t *testing.T, email, role string, outlet uuid.UUID) {
	t.Helper()
	_, err := e.svc.CreateMember(context.Background(), identity.NewMember{
		TenantID: e.tenant, Email: email, Password: password, DisplayName: role + " " + email, EmailVerified: true,
		OutletRoles: []identity.OutletRole{{OutletID: outlet, RoleID: e.role(t, e.tenant, role)}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isForbidden(err error) bool { return errors.Is(err, identity.ErrForbidden) }

func TestValidatePIN(t *testing.T) {
	for _, pin := range []string{"4821", "0931", "135790", "9071", "123457", "1235"} {
		if err := identity.ValidatePIN(pin); err != nil {
			t.Errorf("ValidatePIN(%q) = %v, want ok", pin, err)
		}
	}
	for _, pin := range []string{"", "123", "1234567", "12a4", "12 34", "abcd", "1111", "000000", "1234", "4321", "123456", "987654", "2345", "٣٤٥٦"} {
		if err := identity.ValidatePIN(pin); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("ValidatePIN(%q) = %v, want ErrValidation", pin, err)
		}
	}
}

func TestListRoles(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	roles, err := e.svc.ListRoles(ctx, e.tenant)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]identity.Role{}
	for _, r := range roles {
		names = append(names, r.Name)
		byName[r.Name] = r
		if !r.IsSystem {
			t.Errorf("%s is not marked as a system role", r.Name)
		}
	}
	if want := []string{"Cashier", "Kitchen", "Manager", "Owner"}; !slices.Equal(names, want) {
		t.Errorf("roles = %v, want %v", names, want)
	}
	if got := byName["Owner"].Permissions; len(got) != len(identity.AllPermissions()) {
		t.Errorf("Owner has %d permissions, want all %d", len(got), len(identity.AllPermissions()))
	}
	if slices.Contains(byName["Manager"].Permissions, identity.PermSettingsManage) || !slices.Contains(byName["Manager"].Permissions, identity.PermStaffManage) {
		t.Errorf("Manager = %v: want staff.manage without settings.manage", byName["Manager"].Permissions)
	}
	if want := []identity.Permission{identity.PermSaleCreate, identity.PermShiftClose, identity.PermShiftOpen}; !slices.Equal(byName["Cashier"].Permissions, want) {
		t.Errorf("Cashier = %v, want %v", byName["Cashier"].Permissions, want)
	}
	// Another tenant's roles are invisible.
	other := e.newTenant(t, "other")
	if rs, err := e.svc.ListRoles(ctx, other); err != nil || len(rs) != 4 {
		t.Errorf("other tenant: %d roles (%v)", len(rs), err)
	}
	var total int
	_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM role`).Scan(&total)
	if total != 8 {
		t.Errorf("roles in the database = %d, want 4 per tenant", total)
	}
}

func TestOwnerAndManagerAccess(t *testing.T) {
	e := newEnv(t)
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	e.member(t, "owner@kopi.test", true)
	e.manager(t, "mgr@kopi.test", "Manager", a)
	e.manager(t, "cash@kopi.test", "Cashier", b)

	owner := e.caller(t, "owner@kopi.test")
	if !owner.Access.IsOwner || !owner.Access.Has(identity.PermSettingsManage) || !owner.Access.HasAt(identity.PermSaleVoid, a) {
		t.Errorf("owner access = %+v", owner.Access)
	}
	if len(owner.Access.Permissions()) != len(identity.AllPermissions()) {
		t.Errorf("owner permissions = %v", owner.Access.Permissions())
	}

	mgr := e.caller(t, "mgr@kopi.test").Access
	if mgr.IsOwner || !mgr.HasAt(identity.PermStaffManage, a) || mgr.HasAt(identity.PermStaffManage, b) || mgr.Has(identity.PermSettingsManage) {
		t.Errorf("manager at A: %+v", mgr)
	}
	cash := e.caller(t, "cash@kopi.test").Access
	if !cash.HasAt(identity.PermSaleCreate, b) || cash.HasAt(identity.PermSaleCreate, a) || cash.Has(identity.PermStaffManage) {
		t.Errorf("cashier at B: %+v", cash)
	}
	if got := cash.Permissions(); !slices.Equal(got, []identity.Permission{identity.PermSaleCreate, identity.PermShiftClose, identity.PermShiftOpen}) {
		t.Errorf("cashier permissions = %v", got)
	}
}

func TestCreateStaff(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	outlet := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier := e.role(t, e.tenant, "Cashier")

	st, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{
		DisplayName: "  Sari  ", PIN: "4821",
		OutletRoles: []identity.OutletRole{{OutletID: outlet, RoleID: cashier}, {OutletID: outlet, RoleID: cashier}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.DisplayName != "Sari" || !st.Active || !st.HasPIN || st.PINRotatedAt == nil || st.UserID != nil || st.IsOwner ||
		len(st.OutletRoles) != 1 || st.OutletRoles[0] != (identity.OutletRole{OutletID: outlet, RoleID: cashier}) {
		t.Errorf("staff = %+v", st)
	}

	// The PIN is stored as an argon2id hash with the cheap PIN parameters, and the hash checks out.
	var hash string
	if err := e.d.Owner.QueryRow(ctx, `SELECT pin_hash FROM staff WHERE id = $1`, st.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("pin_hash = %q", hash)
	}
	if ok, err := identity.NewHasher(1).Verify(ctx, "4821", hash); err != nil || !ok {
		t.Errorf("stored hash does not verify the PIN: %v, %v", ok, err)
	}

	// The change reaches the pull feed, and the audit log says who did what, without the PIN.
	var changes int
	_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM change_log WHERE entity_type = 'staff' AND entity_id = $1 AND outlet_id IS NULL`, st.ID).Scan(&changes)
	if changes != 1 {
		t.Errorf("change_log entries for the new staff = %d, want 1", changes)
	}
	var action, actorType, detail string
	var actor uuid.UUID
	if err := e.d.Owner.QueryRow(ctx,
		`SELECT action, actor_type, actor_id, detail::text FROM tenant_audit_log WHERE target_id = $1`, st.ID).Scan(&action, &actorType, &actor, &detail); err != nil {
		t.Fatal(err)
	}
	if action != "staff.created" || actorType != "system" {
		// The service tests call it without the HTTP middleware, so the actor is the system.
		t.Errorf("audit = %s by %s", action, actorType)
	}
	if strings.Contains(detail, "4821") || strings.Contains(detail, "argon2") {
		t.Errorf("audit detail leaks the PIN: %s", detail)
	}

	for name, in := range map[string]identity.NewStaff{
		"no name":        {},
		"long name":      {DisplayName: strings.Repeat("x", 61)},
		"weak pin":       {DisplayName: "A", PIN: "1234"},
		"unknown role":   {DisplayName: "A", OutletRoles: []identity.OutletRole{{OutletID: outlet, RoleID: uuid.New()}}},
		"unknown outlet": {DisplayName: "A", OutletRoles: []identity.OutletRole{{OutletID: uuid.New(), RoleID: cashier}}},
	} {
		if _, err := e.svc.CreateStaff(ctx, owner, in); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("%s: err = %v, want ErrValidation", name, err)
		}
	}
	// A failed create leaves nothing behind: no staff row, no change, no audit.
	var staff int
	_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM staff`).Scan(&staff)
	if staff != 2 { // the owner's own record and Sari
		t.Errorf("staff rows = %d, want 2", staff)
	}
}

func TestStaffAuthorization(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	ownerUser := e.member(t, "owner@kopi.test", true)
	e.manager(t, "mgr@kopi.test", "Manager", a)
	e.manager(t, "cash@kopi.test", "Cashier", a)
	mgr, cash := e.caller(t, "mgr@kopi.test"), e.caller(t, "cash@kopi.test")
	cashier, manager, owner := e.role(t, e.tenant, "Cashier"), e.role(t, e.tenant, "Manager"), e.role(t, e.tenant, "Owner")

	// A manager manages staff at their own outlet.
	st, err := e.svc.CreateStaff(ctx, mgr, identity.NewStaff{DisplayName: "Budi", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: cashier}}})
	if err != nil {
		t.Fatalf("manager creating a cashier at their outlet: %v", err)
	}

	for name, fn := range map[string]func() error{
		"another outlet": func() error {
			_, err := e.svc.CreateStaff(ctx, mgr, identity.NewStaff{DisplayName: "X", OutletRoles: []identity.OutletRole{{OutletID: b, RoleID: cashier}}})
			return err
		},
		"a role stronger than their own": func() error {
			_, err := e.svc.CreateStaff(ctx, mgr, identity.NewStaff{DisplayName: "X", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: owner}}})
			return err
		},
		"a cashier cannot add staff": func() error {
			_, err := e.svc.CreateStaff(ctx, cash, identity.NewStaff{DisplayName: "X", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: cashier}}})
			return err
		},
		"a cashier cannot add staff without outlets either": func() error {
			_, err := e.svc.CreateStaff(ctx, cash, identity.NewStaff{DisplayName: "X"})
			return err
		},
		"moving staff to another outlet": func() error {
			_, err := e.svc.UpdateStaff(ctx, mgr, st.ID, identity.UpdateStaff{OutletRoles: &[]identity.OutletRole{{OutletID: b, RoleID: cashier}}})
			return err
		},
		"promoting staff above themselves": func() error {
			_, err := e.svc.UpdateStaff(ctx, mgr, st.ID, identity.UpdateStaff{OutletRoles: &[]identity.OutletRole{{OutletID: a, RoleID: owner}}})
			return err
		},
		"a cashier cannot rename staff": func() error {
			n := "Hacked"
			_, err := e.svc.UpdateStaff(ctx, cash, st.ID, identity.UpdateStaff{DisplayName: &n})
			return err
		},
		"a cashier cannot set a PIN": func() error { return e.svc.SetPIN(ctx, cash, st.ID, "4821") },
	} {
		if err := fn(); !isForbidden(err) {
			t.Errorf("%s: err = %v, want ErrForbidden", name, err)
		}
	}

	// A manager can grant a role they hold.
	if _, err := e.svc.UpdateStaff(ctx, mgr, st.ID, identity.UpdateStaff{OutletRoles: &[]identity.OutletRole{{OutletID: a, RoleID: manager}}}); err != nil {
		t.Errorf("manager granting Manager: %v", err)
	}

	// Only an owner can change an owner's staff record, set their PIN or deactivate them.
	var ownerStaff uuid.UUID
	if err := e.d.Owner.QueryRow(ctx, `SELECT id FROM staff WHERE user_id = $1`, ownerUser.ID).Scan(&ownerStaff); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := e.svc.UpdateStaff(ctx, mgr, ownerStaff, identity.UpdateStaff{Active: &off}); !isForbidden(err) {
		t.Errorf("manager deactivating the owner: err = %v, want ErrForbidden", err)
	}
	if err := e.svc.SetPIN(ctx, mgr, ownerStaff, "4821"); !isForbidden(err) {
		t.Errorf("manager setting the owner's PIN: err = %v, want ErrForbidden", err)
	}
	if err := e.svc.SetPIN(ctx, e.caller(t, "owner@kopi.test"), ownerStaff, "4821"); err != nil {
		t.Errorf("owner setting their own PIN: %v", err)
	}
}

func TestUpdateStaff(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier, kitchen := e.role(t, e.tenant, "Cashier"), e.role(t, e.tenant, "Kitchen")

	st, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "Sari", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: cashier}}})
	if err != nil {
		t.Fatal(err)
	}
	countAudit := func() (n int) {
		_ = e.d.Owner.QueryRow(ctx, `SELECT count(*) FROM tenant_audit_log WHERE action = 'staff.updated'`).Scan(&n)
		return n
	}

	name := " Sari Wulandari "
	got, err := e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{DisplayName: &name})
	if err != nil || got.DisplayName != "Sari Wulandari" || !got.Active || len(got.OutletRoles) != 1 {
		t.Fatalf("rename: %+v, %v", got, err)
	}

	// Replacing the assignment adds and removes in one step.
	want := []identity.OutletRole{{OutletID: a, RoleID: kitchen}, {OutletID: b, RoleID: cashier}}
	got, err = e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{OutletRoles: &want})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OutletRoles) != 2 || !slices.Contains(got.OutletRoles, want[0]) || !slices.Contains(got.OutletRoles, want[1]) {
		t.Errorf("outlet roles = %+v, want %+v", got.OutletRoles, want)
	}

	// An update that changes nothing writes neither a change nor an audit entry.
	auditBefore := countAudit()
	var seqBefore int64
	_ = e.d.Owner.QueryRow(ctx, `SELECT change_seq FROM tenant WHERE id = $1`, e.tenant).Scan(&seqBefore)
	same := "Sari Wulandari"
	if _, err := e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{DisplayName: &same, OutletRoles: &want}); err != nil {
		t.Fatal(err)
	}
	var seqAfter int64
	_ = e.d.Owner.QueryRow(ctx, `SELECT change_seq FROM tenant WHERE id = $1`, e.tenant).Scan(&seqAfter)
	if countAudit() != auditBefore || seqAfter != seqBefore {
		t.Error("a no-op update was recorded")
	}

	off := false
	if got, err = e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{Active: &off}); err != nil || got.Active {
		t.Errorf("deactivate: %+v, %v", got, err)
	}
	empty := []identity.OutletRole{}
	if got, err = e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{OutletRoles: &empty}); err != nil || len(got.OutletRoles) != 0 {
		t.Errorf("clearing the assignment: %+v, %v", got, err)
	}

	if _, err := e.svc.UpdateStaff(ctx, owner, uuid.New(), identity.UpdateStaff{Active: &off}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown staff: err = %v, want ErrNotFound", err)
	}
	bad := []identity.OutletRole{{OutletID: a, RoleID: uuid.New()}}
	if _, err := e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{OutletRoles: &bad}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("unknown role: err = %v, want ErrValidation", err)
	}
	long := strings.Repeat("x", 61)
	if _, err := e.svc.UpdateStaff(ctx, owner, st.ID, identity.UpdateStaff{DisplayName: &long}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("long name: err = %v, want ErrValidation", err)
	}
}

func TestDeactivatedManagerLosesPermissions(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	e.manager(t, "mgr@kopi.test", "Manager", a)
	mgr := e.caller(t, "mgr@kopi.test")
	if !mgr.Access.Has(identity.PermStaffManage) {
		t.Fatal("setup: the manager lacks staff.manage")
	}

	var staffID uuid.UUID
	if err := e.d.Owner.QueryRow(ctx, `SELECT id FROM staff WHERE user_id = $1`, mgr.Principal.UserID).Scan(&staffID); err != nil {
		t.Fatal(err)
	}
	off := false
	if _, err := e.svc.UpdateStaff(ctx, e.caller(t, "owner@kopi.test"), staffID, identity.UpdateStaff{Active: &off}); err != nil {
		t.Fatal(err)
	}
	acc, err := e.svc.LoadAccess(ctx, mgr.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Has(identity.PermStaffManage) || len(acc.Permissions()) != 0 || acc.IsOwner {
		t.Errorf("a deactivated manager still has %v", acc.Permissions())
	}
}

func TestSetPIN(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	st, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "Sari", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: e.role(t, e.tenant, "Cashier")}}})
	if err != nil {
		t.Fatal(err)
	}
	if st.HasPIN {
		t.Fatal("a new staff member without a PIN has one")
	}
	read := func() (hash string, rotated *struct{}) {
		var h *string
		var r *string
		_ = e.d.Owner.QueryRow(ctx, `SELECT pin_hash, pin_rotated_at::text FROM staff WHERE id = $1`, st.ID).Scan(&h, &r)
		if h != nil {
			hash = *h
		}
		if r != nil {
			rotated = &struct{}{}
		}
		return hash, rotated
	}
	if h, r := read(); h != "" || r != nil {
		t.Fatalf("before: hash %q, rotated %v", h, r)
	}

	if err := e.svc.SetPIN(ctx, owner, st.ID, "4821"); err != nil {
		t.Fatal(err)
	}
	first, r := read()
	if first == "" || r == nil {
		t.Fatal("PIN not stored")
	}
	if err := e.svc.SetPIN(ctx, owner, st.ID, "9071"); err != nil {
		t.Fatal(err)
	}
	second, _ := read()
	if second == first {
		t.Error("rotating the PIN left the hash unchanged")
	}
	h := identity.NewHasher(1)
	if ok, _ := h.Verify(ctx, "9071", second); !ok {
		t.Error("the new hash does not verify the new PIN")
	}
	if ok, _ := h.Verify(ctx, "4821", second); ok {
		t.Error("the old PIN still verifies")
	}

	// Both rotations are audited without the PIN, the first flagged as such.
	rows, err := e.d.Owner.Query(ctx, `SELECT detail::text FROM tenant_audit_log WHERE action = 'staff.pin_rotated' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	details, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(details) != 2 {
		t.Fatalf("audit rows = %v (%v)", details, err)
	}
	var d1, d2 map[string]any
	_ = json.Unmarshal([]byte(details[0]), &d1)
	_ = json.Unmarshal([]byte(details[1]), &d2)
	if d1["first_pin"] != true || d2["first_pin"] != false {
		t.Errorf("first_pin flags = %v, %v", d1, d2)
	}
	for _, d := range details {
		if strings.Contains(d, "4821") || strings.Contains(d, "9071") {
			t.Errorf("audit detail leaks a PIN: %s", d)
		}
	}

	for _, pin := range []string{"1234", "12", "abcd"} {
		if err := e.svc.SetPIN(ctx, owner, st.ID, pin); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("SetPIN(%q) = %v, want ErrValidation", pin, err)
		}
	}
	if err := e.svc.SetPIN(ctx, owner, uuid.New(), "4821"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown staff: err = %v, want ErrNotFound", err)
	}
}

func TestListStaffPagesWithoutNPlusOne(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a, b := e.outlet(t, e.tenant, "AAA"), e.outlet(t, e.tenant, "BBB")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier := e.role(t, e.tenant, "Cashier")

	list := func(page kernel.Page) kernel.Paged[identity.Staff] {
		t.Helper()
		res, err := e.svc.ListStaff(ctx, e.tenant, page)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	// The owner's own record is the only row so far.
	var one kernel.Paged[identity.Staff]
	queriesFor1 := e.d.Queries.During(func() { one = list(kernel.Page{Limit: 50}) })
	if len(one.Items) != 1 || one.Next != uuid.Nil {
		t.Fatalf("first page = %+v", one)
	}

	for i := range 59 {
		_, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{
			DisplayName: "Staff " + string(rune('A'+i%26)) + string(rune('a'+i/26)),
			OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: cashier}, {OutletID: b, RoleID: cashier}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A page of 50 people with 100 assignments costs exactly as many queries as a page of 1.
	var fifty kernel.Paged[identity.Staff]
	queriesFor50 := e.d.Queries.During(func() { fifty = list(kernel.Page{Limit: 50}) })
	if len(fifty.Items) != 50 || fifty.Next == uuid.Nil {
		t.Fatalf("page of 50 = %d items, next %v", len(fifty.Items), fifty.Next)
	}
	if queriesFor1 != queriesFor50 {
		t.Errorf("queries for 1 row = %d, for 50 rows = %d: the list is not constant-query", queriesFor1, queriesFor50)
	}
	if n := len(fifty.Items[1].OutletRoles); n != 2 {
		t.Errorf("outlet roles of the 2nd person = %d, want 2 (children not loaded)", n)
	}

	// Walking the pages visits everyone once, in id order.
	var seen []uuid.UUID
	page := kernel.Page{Limit: 25}
	for range 10 {
		res := list(page)
		for _, st := range res.Items {
			seen = append(seen, st.ID)
		}
		if res.Next == uuid.Nil {
			break
		}
		page.After = res.Next
	}
	if len(seen) != 60 || !slices.IsSortedFunc(seen, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }) {
		t.Errorf("paged through %d staff, sorted: %v", len(seen), slices.IsSortedFunc(seen, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) }))
	}
	if len(slices.Compact(slices.Clone(seen))) != 60 {
		t.Error("a staff member appeared on two pages")
	}
	if res := list(kernel.Page{Limit: 1000}); len(res.Items) != 60 {
		t.Errorf("limit above the cap returned %d, want all 60 (cap is 200)", len(res.Items))
	}
}

func TestStaffIsolation(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")

	other := e.newTenant(t, "other")
	otherOutlet := e.outlet(t, other, "ZZZ")
	e.memberIn(t, other, "owner@other.test", true)
	otherOwner := e.caller(t, "owner@other.test")
	otherStaff, err := e.svc.CreateStaff(ctx, otherOwner, identity.NewStaff{DisplayName: "Theirs"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := e.svc.ListStaff(ctx, e.tenant, kernel.Page{})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("tenant A sees %d staff (%v), want only their own owner record", len(res.Items), err)
	}
	n := "Mine now"
	if _, err := e.svc.UpdateStaff(ctx, owner, otherStaff.ID, identity.UpdateStaff{DisplayName: &n}); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("updating another tenant's staff: err = %v, want ErrNotFound", err)
	}
	if err := e.svc.SetPIN(ctx, owner, otherStaff.ID, "4821"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("setting another tenant's PIN: err = %v, want ErrNotFound", err)
	}
	// Roles and outlets of another tenant cannot be assigned, whatever their ids.
	for name, as := range map[string][]identity.OutletRole{
		"their role":   {{OutletID: a, RoleID: e.role(t, other, "Cashier")}},
		"their outlet": {{OutletID: otherOutlet, RoleID: e.role(t, e.tenant, "Cashier")}},
	} {
		if _, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "X", OutletRoles: as}); !errors.Is(err, kernel.ErrValidation) {
			t.Errorf("assigning %s: err = %v, want ErrValidation", name, err)
		}
	}
}

// Staff writers on one tenant queue on the tenant row lock, so concurrent creates, updates and PIN
// changes never deadlock and the change numbers stay gapless.
func TestConcurrentStaffWritesDoNotDeadlock(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	a := e.outlet(t, e.tenant, "AAA")
	e.member(t, "owner@kopi.test", true)
	owner := e.caller(t, "owner@kopi.test")
	cashier, kitchen := e.role(t, e.tenant, "Cashier"), e.role(t, e.tenant, "Kitchen")
	shared, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "Shared"})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers*3)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			role := cashier
			if i%2 == 0 {
				role = kitchen
			}
			if _, err := e.svc.CreateStaff(ctx, owner, identity.NewStaff{DisplayName: "W", OutletRoles: []identity.OutletRole{{OutletID: a, RoleID: role}}}); err != nil {
				errs <- err
			}
			if err := e.svc.SetPIN(ctx, owner, shared.ID, []string{"4821", "9071", "1357", "2468"}[i%4]); err != nil {
				errs <- err
			}
			name := "Shared " + string(rune('A'+i))
			if _, err := e.svc.UpdateStaff(ctx, owner, shared.ID, identity.UpdateStaff{DisplayName: &name, OutletRoles: &[]identity.OutletRole{{OutletID: a, RoleID: role}}}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	var maxSeq, rows, distinct int64
	if err := e.d.Owner.QueryRow(ctx, `SELECT max(seq), count(*), count(DISTINCT seq) FROM change_log WHERE tenant_id = $1`, e.tenant).Scan(&maxSeq, &rows, &distinct); err != nil {
		t.Fatal(err)
	}
	if maxSeq != rows || rows != distinct {
		t.Errorf("change numbers have gaps or repeats: max %d, rows %d, distinct %d", maxSeq, rows, distinct)
	}
}
