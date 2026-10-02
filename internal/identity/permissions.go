package identity

import (
	"slices"

	"github.com/google/uuid"
)

// Permission is a fixed string in code (BACKEND_PLAN.md section 4.4). Roles are per tenant and
// are sets of permissions.
type Permission string

const (
	PermSaleCreate          Permission = "sale.create"
	PermSaleVoid            Permission = "sale.void"
	PermSaleRefund          Permission = "sale.refund"
	PermDiscountApplyManual Permission = "discount.apply_manual"
	PermDrawerOpenNoSale    Permission = "drawer.open_no_sale"
	PermShiftOpen           Permission = "shift.open"
	PermShiftClose          Permission = "shift.close"
	PermCatalogManage       Permission = "catalog.manage"
	PermInventoryManage     Permission = "inventory.manage"
	PermReportView          Permission = "report.view"
	PermStaffManage         Permission = "staff.manage"
	PermDeviceManage        Permission = "device.manage"
	PermSettingsManage      Permission = "settings.manage"
	PermKitchenView         Permission = "kitchen.view"
)

// AllPermissions lists every permission, sorted. The Owner role holds all of them.
func AllPermissions() []Permission {
	return slices.Clone(allPermissions)
}

var allPermissions = []Permission{
	PermCatalogManage, PermDeviceManage, PermDiscountApplyManual, PermDrawerOpenNoSale,
	PermInventoryManage, PermKitchenView, PermReportView, PermSaleCreate, PermSaleRefund,
	PermSaleVoid, PermSettingsManage, PermShiftClose, PermShiftOpen, PermStaffManage,
}

// IsPermission reports whether p is a known permission.
func IsPermission(p string) bool {
	_, found := slices.BinarySearch(allPermissions, Permission(p))
	return found
}

// systemRole is a role every tenant starts with. System roles cannot be edited.
type systemRole struct {
	Name        string
	Permissions []Permission
}

// SystemRoles are seeded for every tenant (SeedRoles). Manager is Owner without settings.
var systemRoles = []systemRole{
	{"Owner", allPermissions},
	{"Manager", without(allPermissions, PermSettingsManage)},
	{"Cashier", []Permission{PermSaleCreate, PermShiftOpen, PermShiftClose}},
	{"Kitchen", []Permission{PermKitchenView}},
}

func without(all []Permission, drop ...Permission) []Permission {
	return slices.DeleteFunc(slices.Clone(all), func(p Permission) bool { return slices.Contains(drop, p) })
}

// Access is what a user may do in the current tenant: everything for an owner, otherwise the
// permissions of the roles they hold, per outlet.
type Access struct {
	IsOwner  bool
	byOutlet map[uuid.UUID]map[Permission]struct{}
}

// Has reports whether the user holds p at any outlet, or is an owner.
func (a Access) Has(p Permission) bool {
	if a.IsOwner {
		return true
	}
	for _, perms := range a.byOutlet {
		if _, ok := perms[p]; ok {
			return true
		}
	}
	return false
}

// HasAt reports whether the user holds p at the given outlet, or is an owner.
func (a Access) HasAt(p Permission, outletID uuid.UUID) bool {
	if a.IsOwner {
		return true
	}
	_, ok := a.byOutlet[outletID][p]
	return ok
}

// Permissions is every permission the user holds at any outlet, sorted.
func (a Access) Permissions() []Permission {
	if a.IsOwner {
		return AllPermissions()
	}
	set := map[Permission]struct{}{}
	for _, perms := range a.byOutlet {
		for p := range perms {
			set[p] = struct{}{}
		}
	}
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func (a *Access) grant(outletID uuid.UUID, p Permission) {
	if a.byOutlet == nil {
		a.byOutlet = map[uuid.UUID]map[Permission]struct{}{}
	}
	if a.byOutlet[outletID] == nil {
		a.byOutlet[outletID] = map[Permission]struct{}{}
	}
	a.byOutlet[outletID][p] = struct{}{}
}

// holdsAll reports whether the user holds every permission in perms at outletID. Used so nobody
// grants a role stronger than their own.
func (a Access) holdsAll(outletID uuid.UUID, perms []Permission) bool {
	for _, p := range perms {
		if !a.HasAt(p, outletID) {
			return false
		}
	}
	return true
}
