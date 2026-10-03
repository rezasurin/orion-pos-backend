package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Caller is the signed-in user making a request, with what they may do. Methods that change staff
// take it so they can refuse to exceed it.
type Caller struct {
	Principal Principal
	Access    Access
}

// CallerFrom builds the caller from a request context that authentication middleware has filled.
func CallerFrom(ctx context.Context) (Caller, bool) {
	p, okP := PrincipalFrom(ctx)
	a, okA := AccessFrom(ctx)
	return Caller{Principal: p, Access: a}, okP && okA && p.Type == PrincipalUser
}

// Role is a named set of permissions, per tenant.
type Role struct {
	ID          uuid.UUID
	Name        string
	IsSystem    bool
	Permissions []Permission
}

// OutletRole is one role held at one outlet.
type OutletRole struct {
	OutletID uuid.UUID
	RoleID   uuid.UUID
}

// Staff is a person who acts in the tenant: a cashier with a PIN, or an owner or manager.
type Staff struct {
	ID           uuid.UUID
	UserID       *uuid.UUID // set when the person also signs in by email
	DisplayName  string
	Active       bool
	HasPIN       bool
	PINRotatedAt *time.Time
	IsOwner      bool
	OutletRoles  []OutletRole
	CreatedAt    time.Time
}

// SeedRoles creates the system roles (Owner, Manager, Cashier, Kitchen) for a new tenant inside
// the caller's transaction. Run it once, when the tenant is created.
func SeedRoles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	q := db.New(tx)
	for _, r := range systemRoles {
		id := kernel.NewID()
		if err := q.InsertRole(ctx, db.InsertRoleParams{ID: id, TenantID: tenantID, Name: r.Name, IsSystem: true}); err != nil {
			return err
		}
		perms := make([]string, len(r.Permissions))
		for i, p := range r.Permissions {
			perms[i] = string(p)
		}
		if err := q.InsertRolePermissions(ctx, db.InsertRolePermissionsParams{TenantID: tenantID, RoleID: id, Permissions: perms}); err != nil {
			return err
		}
	}
	return nil
}

// ListRoles returns the tenant's roles with their permissions: two queries however many roles.
func (s *Service) ListRoles(ctx context.Context, tenantID uuid.UUID) ([]Role, error) {
	var out []Role
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		roles, err := q.ListRoles(ctx, tenantID)
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(roles))
		for i, r := range roles {
			ids[i] = r.ID
		}
		perms, err := rolePermissions(ctx, q, tenantID, ids)
		if err != nil {
			return err
		}
		out = make([]Role, len(roles))
		for i, r := range roles {
			out[i] = Role{ID: r.ID, Name: r.Name, IsSystem: r.IsSystem, Permissions: perms[r.ID]}
		}
		return nil
	})
	return out, err
}

func rolePermissions(ctx context.Context, q *db.Queries, tenantID uuid.UUID, roleIDs []uuid.UUID) (map[uuid.UUID][]Permission, error) {
	rows, err := q.ListRolePermissions(ctx, db.ListRolePermissionsParams{TenantID: tenantID, RoleIds: roleIDs})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]Permission, len(roleIDs))
	for _, id := range roleIDs {
		out[id] = []Permission{}
	}
	for _, r := range rows {
		out[r.RoleID] = append(out[r.RoleID], Permission(r.Permission))
	}
	return out, nil
}

var pinPattern = regexp.MustCompile(`^[0-9]{4,6}$`)

// ValidatePIN checks a staff PIN: 4 to 6 digits, not all the same and not a run such as 1234 or
// 8765. A PIN is a convenience switch, not a security boundary (BACKEND_PLAN.md section 4.3), so
// this only rules out the guesses everyone tries first.
func ValidatePIN(pin string) error {
	if !pinPattern.MatchString(pin) {
		return fmt.Errorf("%w: PIN must be 4 to 6 digits", kernel.ErrValidation)
	}
	if isRepeated(pin) || isRun(pin, 1) || isRun(pin, -1) {
		return fmt.Errorf("%w: PIN is too easy to guess", kernel.ErrValidation)
	}
	return nil
}

func isRepeated(pin string) bool {
	return strings.Count(pin, pin[:1]) == len(pin)
}

func isRun(pin string, step int) bool {
	for i := 1; i < len(pin); i++ {
		if int(pin[i])-int(pin[i-1]) != step {
			return false
		}
	}
	return true
}

// NewStaff describes a person to add. They get no email login; owners and managers who sign in are
// created with CreateMember.
type NewStaff struct {
	DisplayName string
	OutletRoles []OutletRole
	PIN         string // optional; can be set later
}

// CreateStaff adds a staff member. The caller needs staff.manage at every outlet named, and may
// only grant roles whose permissions they hold themselves at that outlet.
func (s *Service) CreateStaff(ctx context.Context, c Caller, in NewStaff) (Staff, error) {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if err := validateStaffName(in.DisplayName); err != nil {
		return Staff{}, err
	}
	in.OutletRoles = dedupe(in.OutletRoles)
	var pinHash *string
	if in.PIN != "" {
		if err := ValidatePIN(in.PIN); err != nil {
			return Staff{}, err
		}
		h, err := s.Hasher.Hash(ctx, in.PIN, PINParams) // outside the transaction: it is slow
		if err != nil {
			return Staff{}, err
		}
		pinHash = &h
	}

	id, now := kernel.NewID(), s.Clock.Now()
	var out Staff
	err := kernel.TenantTx(ctx, s.Pool, c.Principal.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		if err := authorizeAssignments(ctx, q, c, in.OutletRoles); err != nil {
			return err
		}
		if len(in.OutletRoles) == 0 && !c.Access.Has(PermStaffManage) {
			return &ForbiddenError{Permission: string(PermStaffManage)}
		}
		if err := s.checkStaffLimit(ctx, tx, q, c.Principal.TenantID); err != nil {
			return err
		}
		var rotated *time.Time
		if pinHash != nil {
			rotated = &now
		}
		if err := q.InsertStaff(ctx, db.InsertStaffParams{
			ID: id, TenantID: c.Principal.TenantID, DisplayName: in.DisplayName, PinHash: pinHash, PinRotatedAt: rotated,
		}); err != nil {
			return mapStaffErr(err)
		}
		if err := insertAssignments(ctx, q, c.Principal.TenantID, id, in.OutletRoles); err != nil {
			return err
		}
		if _, err := kernel.RecordChange(ctx, tx, "staff", id, "upsert", nil); err != nil {
			return err
		}
		if err := kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "staff.created", TargetType: "staff", TargetID: id,
			Detail: map[string]any{"display_name": in.DisplayName, "outlet_roles": len(in.OutletRoles), "has_pin": pinHash != nil},
		}); err != nil {
			return err
		}
		var err error
		out, err = loadStaff(ctx, q, c.Principal.TenantID, id)
		return err
	})
	return out, err
}

// ListStaff returns a page of the tenant's staff, ordered by id. Their outlet roles come from one
// more query, not one per person.
func (s *Service) ListStaff(ctx context.Context, tenantID uuid.UUID, page kernel.Page) (kernel.Paged[Staff], error) {
	var out kernel.Paged[Staff]
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListStaff(ctx, db.ListStaffParams{TenantID: tenantID, After: page.After, PageSize: page.Fetch()})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(r db.ListStaffRow) uuid.UUID { return r.ID })
		ids := make([]uuid.UUID, len(paged.Items))
		for i, r := range paged.Items {
			ids[i] = r.ID
		}
		roles, err := q.ListStaffOutletRoles(ctx, db.ListStaffOutletRolesParams{TenantID: tenantID, StaffIds: ids})
		if err != nil {
			return err
		}
		byStaff := map[uuid.UUID][]OutletRole{}
		for _, r := range roles {
			byStaff[r.StaffID] = append(byStaff[r.StaffID], OutletRole{OutletID: r.OutletID, RoleID: r.RoleID})
		}
		out = kernel.Paged[Staff]{Next: paged.Next, Items: make([]Staff, len(paged.Items))}
		for i, r := range paged.Items {
			out.Items[i] = toStaff(r.ID, r.UserID, r.DisplayName, r.PinHash, r.PinRotatedAt, r.Active, r.IsOwner, r.CreatedAt, byStaff[r.ID])
		}
		return nil
	})
	return out, err
}

// UpdateStaff changes a staff member. Nil fields are left alone; OutletRoles, when set, replaces
// the whole assignment.
type UpdateStaff struct {
	DisplayName *string
	Active      *bool
	OutletRoles *[]OutletRole
}

// UpdateStaff applies changes to one staff member, under the same rules as CreateStaff: the caller
// needs staff.manage at every outlet the person is or becomes assigned to, cannot grant a stronger
// role than their own, and only an owner may change an owner's record.
func (s *Service) UpdateStaff(ctx context.Context, c Caller, id uuid.UUID, in UpdateStaff) (Staff, error) {
	if in.DisplayName != nil {
		name := strings.TrimSpace(*in.DisplayName)
		if err := validateStaffName(name); err != nil {
			return Staff{}, err
		}
		in.DisplayName = &name
	}
	var want []OutletRole
	if in.OutletRoles != nil {
		want = dedupe(*in.OutletRoles)
	}

	var out Staff
	err := kernel.TenantTx(ctx, s.Pool, c.Principal.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		tenantID := c.Principal.TenantID
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		cur, err := q.GetStaffForUpdate(ctx, db.GetStaffForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapNoRows(err, kernel.ErrNotFound)
		}
		have, err := outletRoles(ctx, q, tenantID, id)
		if err != nil {
			return err
		}
		if err := authorizeStaff(c, cur.IsOwner, outletsOf(have, want)); err != nil {
			return err
		}

		changes := map[string]any{}
		name, active := cur.DisplayName, cur.Active
		if in.DisplayName != nil && *in.DisplayName != name {
			changes["display_name"] = *in.DisplayName
			name = *in.DisplayName
		}
		if in.Active != nil && *in.Active != active {
			changes["active"] = *in.Active
			active = *in.Active
		}
		if len(changes) > 0 {
			if err := q.UpdateStaff(ctx, db.UpdateStaffParams{TenantID: tenantID, ID: id, DisplayName: name, Active: active}); err != nil {
				return err
			}
		}
		if in.OutletRoles != nil {
			added, removed := diff(have, want)
			if err := authorizeAssignments(ctx, q, c, added); err != nil {
				return err
			}
			if len(removed) > 0 {
				oids, rids := split(removed)
				if err := q.DeleteStaffOutletRoles(ctx, db.DeleteStaffOutletRolesParams{TenantID: tenantID, StaffID: id, OutletIds: oids, RoleIds: rids}); err != nil {
					return err
				}
			}
			if err := insertAssignments(ctx, q, tenantID, id, added); err != nil {
				return err
			}
			if len(added)+len(removed) > 0 {
				changes["roles_added"], changes["roles_removed"] = len(added), len(removed)
			}
		}
		if len(changes) > 0 {
			if _, err := kernel.RecordChange(ctx, tx, "staff", id, "upsert", nil); err != nil {
				return err
			}
			if err := kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
				Action: "staff.updated", TargetType: "staff", TargetID: id, Detail: changes,
			}); err != nil {
				return err
			}
		}
		out, err = loadStaff(ctx, q, tenantID, id)
		return err
	})
	return out, err
}

// SetPIN sets or rotates a staff member's PIN. The new hash reaches paired devices at their next
// sync. The audit log records that it happened, never the PIN.
func (s *Service) SetPIN(ctx context.Context, c Caller, id uuid.UUID, pin string) error {
	if err := ValidatePIN(pin); err != nil {
		return err
	}
	hash, err := s.Hasher.Hash(ctx, pin, PINParams) // outside the transaction: it is slow
	if err != nil {
		return err
	}
	now := s.Clock.Now()
	return kernel.TenantTx(ctx, s.Pool, c.Principal.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		tenantID := c.Principal.TenantID
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		cur, err := q.GetStaffForUpdate(ctx, db.GetStaffForUpdateParams{TenantID: tenantID, ID: id})
		if err != nil {
			return mapNoRows(err, kernel.ErrNotFound)
		}
		have, err := outletRoles(ctx, q, tenantID, id)
		if err != nil {
			return err
		}
		if err := authorizeStaff(c, cur.IsOwner, outletsOf(have, nil)); err != nil {
			return err
		}
		if err := q.SetStaffPIN(ctx, db.SetStaffPINParams{TenantID: tenantID, ID: id, PinHash: &hash, Now: &now}); err != nil {
			return err
		}
		if _, err := kernel.RecordChange(ctx, tx, "staff", id, "upsert", nil); err != nil {
			return err
		}
		return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "staff.pin_rotated", TargetType: "staff", TargetID: id,
			Detail: map[string]any{"first_pin": cur.PinHash == nil},
		})
	})
}

// authorizeStaff says whether c may manage a staff member who is (or will be) assigned to the
// given outlets.
func authorizeStaff(c Caller, targetIsOwner bool, outlets []uuid.UUID) error {
	if c.Access.IsOwner {
		return nil
	}
	if targetIsOwner {
		return &ForbiddenError{Permission: string(PermStaffManage), Reason: "only an owner can change an owner"}
	}
	if len(outlets) == 0 {
		if !c.Access.Has(PermStaffManage) {
			return &ForbiddenError{Permission: string(PermStaffManage)}
		}
		return nil
	}
	for _, o := range outlets {
		if !c.Access.HasAt(PermStaffManage, o) {
			return &ForbiddenError{Permission: string(PermStaffManage), Reason: "staff.manage is needed at every outlet the person works at"}
		}
	}
	return nil
}

// authorizeAssignments checks that c may give each assignment: staff.manage at that outlet, and
// every permission of the role held by c at that outlet, so nobody hands out more than they have.
// It also rejects roles that do not exist in this tenant.
func authorizeAssignments(ctx context.Context, q *db.Queries, c Caller, as []OutletRole) error {
	if len(as) == 0 {
		return nil
	}
	roleIDs := make([]uuid.UUID, len(as))
	for i, a := range as {
		roleIDs[i] = a.RoleID
	}
	roles, err := q.ListRolesByIDs(ctx, db.ListRolesByIDsParams{TenantID: c.Principal.TenantID, RoleIds: roleIDs})
	if err != nil {
		return err
	}
	known := make(map[uuid.UUID]bool, len(roles))
	for _, r := range roles {
		known[r.ID] = true
	}
	perms, err := rolePermissions(ctx, q, c.Principal.TenantID, roleIDs)
	if err != nil {
		return err
	}
	for _, a := range as {
		if !known[a.RoleID] {
			return fmt.Errorf("%w: unknown role %s", kernel.ErrValidation, a.RoleID)
		}
		if !c.Access.HasAt(PermStaffManage, a.OutletID) {
			return &ForbiddenError{Permission: string(PermStaffManage), Reason: "staff.manage is needed at the outlet"}
		}
		if !c.Access.holdsAll(a.OutletID, perms[a.RoleID]) {
			return &ForbiddenError{Permission: string(PermStaffManage), Reason: "you cannot grant a role with permissions you do not have"}
		}
	}
	return nil
}

func insertAssignments(ctx context.Context, q *db.Queries, tenantID, staffID uuid.UUID, as []OutletRole) error {
	if len(as) == 0 {
		return nil
	}
	oids, rids := split(as)
	return mapStaffErr(q.InsertStaffOutletRoles(ctx, db.InsertStaffOutletRolesParams{
		TenantID: tenantID, StaffID: staffID, OutletIds: oids, RoleIds: rids,
	}))
}

func outletRoles(ctx context.Context, q *db.Queries, tenantID, staffID uuid.UUID) ([]OutletRole, error) {
	rows, err := q.ListStaffOutletRoles(ctx, db.ListStaffOutletRolesParams{TenantID: tenantID, StaffIds: []uuid.UUID{staffID}})
	if err != nil {
		return nil, err
	}
	out := make([]OutletRole, len(rows))
	for i, r := range rows {
		out[i] = OutletRole{OutletID: r.OutletID, RoleID: r.RoleID}
	}
	return out, nil
}

func loadStaff(ctx context.Context, q *db.Queries, tenantID, id uuid.UUID) (Staff, error) {
	r, err := q.GetStaff(ctx, db.GetStaffParams{TenantID: tenantID, ID: id})
	if err != nil {
		return Staff{}, mapNoRows(err, kernel.ErrNotFound)
	}
	roles, err := outletRoles(ctx, q, tenantID, id)
	if err != nil {
		return Staff{}, err
	}
	return toStaff(r.ID, r.UserID, r.DisplayName, r.PinHash, r.PinRotatedAt, r.Active, r.IsOwner, r.CreatedAt, roles), nil
}

func toStaff(id uuid.UUID, userID *uuid.UUID, name string, pinHash *string, rotated *time.Time, active, isOwner bool, created time.Time, roles []OutletRole) Staff {
	if roles == nil {
		roles = []OutletRole{}
	}
	return Staff{
		ID: id, UserID: userID, DisplayName: name, Active: active, HasPIN: pinHash != nil,
		PINRotatedAt: rotated, IsOwner: isOwner, OutletRoles: roles, CreatedAt: created,
	}
}

func validateStaffName(name string) error {
	if name == "" || len([]rune(name)) > 60 {
		return fmt.Errorf("%w: display name is required and at most 60 characters", kernel.ErrValidation)
	}
	return nil
}

func dedupe(as []OutletRole) []OutletRole {
	out := make([]OutletRole, 0, len(as))
	for _, a := range as {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

func split(as []OutletRole) (outlets, roles []uuid.UUID) {
	for _, a := range as {
		outlets, roles = append(outlets, a.OutletID), append(roles, a.RoleID)
	}
	return outlets, roles
}

// diff returns what to add and remove to turn have into want.
func diff(have, want []OutletRole) (added, removed []OutletRole) {
	for _, w := range want {
		if !slices.Contains(have, w) {
			added = append(added, w)
		}
	}
	for _, h := range have {
		if !slices.Contains(want, h) {
			removed = append(removed, h)
		}
	}
	return added, removed
}

// outletsOf is the distinct outlets of two assignment lists, in a stable order.
func outletsOf(lists ...[]OutletRole) []uuid.UUID {
	var out []uuid.UUID
	for _, l := range lists {
		for _, a := range l {
			if !slices.Contains(out, a.OutletID) {
				out = append(out, a.OutletID)
			}
		}
	}
	return out
}

// mapStaffErr turns a foreign key violation (an outlet or role that does not exist, or one that
// belongs to another tenant) into a validation error.
func mapStaffErr(err error) error { return mapRefErr(err, "outlet or role") }

// mapRefErr turns a reference to something that is not there into a validation error naming what
// was unknown: a foreign key violation, or a row-level security violation, which is what an upsert
// onto another tenant's row (a counter keyed by an outlet id from elsewhere) raises. A plain
// missing-grant error (also 42501) is not mapped, so it still surfaces as the bug it is.
func mapRefErr(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code == "23503" || (pgErr.Code == "42501" && strings.Contains(pgErr.Message, "row-level security")) {
			return fmt.Errorf("%w: unknown %s", kernel.ErrValidation, what)
		}
	}
	return mapErr(err)
}
