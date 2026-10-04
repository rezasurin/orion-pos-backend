package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Acting is a staff member named in a synced event, as the server knows them now. Events are
// accepted even when the person lacks a permission (the money has already moved), so Has is what
// the projector uses to flag, never to refuse (BACKEND_PLAN.md section 4.4).
type Acting struct {
	StaffID uuid.UUID
	Known   bool // a staff row with this id exists in the tenant
	Active  bool
	IsOwner bool
	perms   map[Permission]struct{}
}

// Has reports whether the person holds p at the outlet the event came from. An inactive or unknown
// person holds nothing; an owner holds everything.
func (a Acting) Has(p Permission) bool {
	if !a.Known || !a.Active {
		return false
	}
	if a.IsOwner {
		return true
	}
	_, ok := a.perms[p]
	return ok
}

// LoadActing returns who staffID is and what they may do at outletID, in the caller's transaction.
// An unknown id is not an error: Acting.Known is false.
func (s *Service) LoadActing(ctx context.Context, tx pgx.Tx, tenantID, staffID, outletID uuid.UUID) (Acting, error) {
	q := db.New(tx)
	st, err := q.GetStaff(ctx, db.GetStaffParams{TenantID: tenantID, ID: staffID})
	if err != nil {
		if mapNoRows(err, kernel.ErrNotFound) == kernel.ErrNotFound {
			return Acting{StaffID: staffID}, nil
		}
		return Acting{}, err
	}
	a := Acting{StaffID: staffID, Known: true, Active: st.Active, IsOwner: st.IsOwner, perms: map[Permission]struct{}{}}
	perms, err := q.ListStaffPermissionsAt(ctx, db.ListStaffPermissionsAtParams{TenantID: tenantID, StaffID: staffID, OutletID: outletID})
	if err != nil {
		return Acting{}, err
	}
	for _, p := range perms {
		a.perms[Permission(p)] = struct{}{}
	}
	return a, nil
}

// LockDeviceActive returns ErrDeviceRevoked if the device is revoked, and otherwise keeps it from
// being revoked until the caller's transaction ends: revocation waits for events being projected.
func (s *Service) LockDeviceActive(ctx context.Context, tx pgx.Tx, tenantID, deviceID uuid.UUID) error {
	d, err := db.New(tx).GetDeviceForShare(ctx, db.GetDeviceForShareParams{TenantID: tenantID, ID: deviceID})
	if err != nil {
		return mapNoRows(err, ErrInvalidToken)
	}
	if d.RevokedAt != nil {
		return ErrDeviceRevoked
	}
	return nil
}

// RecordSync stamps a push on the device for the support view: when it last synced, its app
// version, and how far its clock is off (server time minus the device's clientTime, in ms).
func (s *Service) RecordSync(ctx context.Context, tx pgx.Tx, tenantID, deviceID uuid.UUID, clientTime *time.Time, appVersion *string) error {
	now := s.Clock.Now()
	var skew *int32
	if clientTime != nil {
		ms := now.Sub(*clientTime).Milliseconds()
		const lim = 1 << 30 // about 12 days; beyond that the number is noise anyway
		if ms > lim {
			ms = lim
		} else if ms < -lim {
			ms = -lim
		}
		v := int32(ms) //nolint:gosec // clamped above
		skew = &v
	}
	return db.New(tx).RecordDeviceSync(ctx, db.RecordDeviceSyncParams{
		TenantID: tenantID, ID: deviceID, Now: &now, AppVersion: appVersion, ClockSkewMs: skew,
	})
}

// DeviceInTx returns a device in the caller's transaction, for projectors that need its code (the
// middle of receipt numbers) or its outlet.
func (s *Service) DeviceInTx(ctx context.Context, tx pgx.Tx, tenantID, deviceID uuid.UUID) (Device, error) {
	d, err := db.New(tx).GetDevice(ctx, db.GetDeviceParams{TenantID: tenantID, ID: deviceID})
	if err != nil {
		return Device{}, mapNoRows(err, kernel.ErrNotFound)
	}
	return toDevice(d), nil
}
