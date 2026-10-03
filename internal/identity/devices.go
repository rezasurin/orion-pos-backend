package identity

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// maxClockSkew bounds the clock skew we record; a device a day off has a bigger problem than a
// number to show support.
const maxClockSkew = 24 * time.Hour

// Device is a paired POS tablet.
type Device struct {
	ID          uuid.UUID
	OutletID    uuid.UUID
	Code        int // the middle of receipt numbers: {outlet}-{code}-{counter}
	Name        string
	PairedBy    uuid.UUID
	PairedAt    time.Time
	RevokedAt   *time.Time
	LastSeenAt  *time.Time
	LastSyncAt  *time.Time
	AppVersion  *string
	ClockSkewMs *int // server time minus device time; positive means the device is behind
}

// NewDevice describes a device to pair.
type NewDevice struct {
	OutletID uuid.UUID
	Name     string
}

// PairedDevice is a newly paired device together with its secret, which is returned this once and
// stored only as a hash.
type PairedDevice struct {
	Device Device
	Secret string
}

// PairDevice registers a tablet at an outlet and returns its secret. The caller needs
// device.manage at that outlet. The device code comes from a per-outlet counter, so it is never
// reused even after the device is revoked.
func (s *Service) PairDevice(ctx context.Context, c Caller, in NewDevice) (PairedDevice, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len([]rune(name)) > 60 {
		return PairedDevice{}, fmt.Errorf("%w: name is required and at most 60 characters", kernel.ErrValidation)
	}
	if in.OutletID == uuid.Nil {
		return PairedDevice{}, fmt.Errorf("%w: outlet_id is required", kernel.ErrValidation)
	}
	if !c.Access.HasAt(PermDeviceManage, in.OutletID) {
		return PairedDevice{}, &ForbiddenError{Permission: string(PermDeviceManage), Reason: "device.manage is needed at the outlet"}
	}

	tenantID, id := c.Principal.TenantID, kernel.NewID()
	secret, hash := mintOpaque(prefixDeviceSecret, tenantID, id)
	var out Device
	err := kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		// limit.devices counts devices that are not revoked, checked under the tenant lock.
		if s.Entitlements != nil {
			n, err := q.CountActiveDevices(ctx, tenantID)
			if err != nil {
				return err
			}
			if err := s.Entitlements.CheckLimit(ctx, tx, tenantID, entitlements.LimitDevices, n); err != nil {
				return err
			}
		}
		code, err := q.AllocateDeviceCode(ctx, db.AllocateDeviceCodeParams{OutletID: in.OutletID, TenantID: tenantID})
		if err != nil {
			return mapRefErr(err, "outlet")
		}
		if err := q.InsertDevice(ctx, db.InsertDeviceParams{
			ID: id, TenantID: tenantID, OutletID: in.OutletID, DeviceCode: code, Name: name,
			SecretHash: hash, PairedBy: c.Principal.UserID,
		}); err != nil {
			return mapRefErr(err, "outlet")
		}
		if err := kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "device.paired", TargetType: "device", TargetID: id,
			Detail: map[string]any{"outlet_id": in.OutletID, "device_code": code, "name": name},
		}); err != nil {
			return err
		}
		d, err := q.GetDevice(ctx, db.GetDeviceParams{TenantID: tenantID, ID: id})
		out = toDevice(d)
		return err
	})
	if err != nil {
		return PairedDevice{}, err
	}
	return PairedDevice{Device: out, Secret: secret}, nil
}

// ListDevices returns a page of devices, ordered by id. Owners see every device; others only
// those at outlets where they hold device.manage.
func (s *Service) ListDevices(ctx context.Context, c Caller, page kernel.Page) (kernel.Paged[Device], error) {
	all, outlets := c.Access.OutletsWith(PermDeviceManage)
	var out kernel.Paged[Device]
	err := kernel.TenantTx(ctx, s.Pool, c.Principal.TenantID, func(tx pgx.Tx) error {
		rows, err := db.New(tx).ListDevices(ctx, db.ListDevicesParams{
			TenantID: c.Principal.TenantID, After: page.After, AllOutlets: all, OutletIds: outlets, PageSize: page.Fetch(),
		})
		if err != nil {
			return err
		}
		paged := kernel.Trim(page, rows, func(d db.Device) uuid.UUID { return d.ID })
		out = kernel.Paged[Device]{Next: paged.Next, Items: make([]Device, len(paged.Items))}
		for i, d := range paged.Items {
			out.Items[i] = toDevice(d)
		}
		return nil
	})
	return out, err
}

// RevokeDevice cuts a device off: its next token exchange fails, and requests with a token it
// already holds fail at once. Revoking twice is not an error.
func (s *Service) RevokeDevice(ctx context.Context, c Caller, id uuid.UUID) error {
	return kernel.TenantTx(ctx, s.Pool, c.Principal.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		d, err := q.GetDeviceForUpdate(ctx, db.GetDeviceForUpdateParams{TenantID: c.Principal.TenantID, ID: id})
		if err != nil {
			return mapNoRows(err, kernel.ErrNotFound)
		}
		if !c.Access.HasAt(PermDeviceManage, d.OutletID) {
			return &ForbiddenError{Permission: string(PermDeviceManage), Reason: "device.manage is needed at the device's outlet"}
		}
		if d.RevokedAt != nil {
			return nil
		}
		now := s.Clock.Now()
		if err := q.RevokeDevice(ctx, db.RevokeDeviceParams{TenantID: c.Principal.TenantID, ID: id, Now: &now, RevokedBy: &c.Principal.UserID}); err != nil {
			return err
		}
		return kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
			Action: "device.revoked", TargetType: "device", TargetID: id,
			Detail: map[string]any{"outlet_id": d.OutletID, "device_code": d.DeviceCode, "name": d.Name},
		})
	})
}

// DeviceExchange is a device presenting its secret for an access token.
type DeviceExchange struct {
	Secret     string
	AppVersion string     // optional, for support
	ClientTime *time.Time // optional: the device's clock, to measure skew
}

// DeviceSession is the result of a successful exchange.
type DeviceSession struct {
	TenantID, DeviceID, OutletID uuid.UUID
	AccessToken                  string
	AccessExpiresAt              time.Time
}

// ExchangeDeviceSecret trades a device secret for a short-lived access token (audience device). A
// revoked device is refused here, which is how revocation reaches a tablet that comes back online.
func (s *Service) ExchangeDeviceSecret(ctx context.Context, in DeviceExchange) (DeviceSession, error) {
	ids, hash, err := parseOpaque(prefixDeviceSecret, 2, in.Secret)
	if err != nil {
		return DeviceSession{}, err
	}
	tenantID, deviceID := ids[0], ids[1]
	if err := s.gate(ctx, tenantID); err != nil {
		return DeviceSession{}, err
	}

	now := s.Clock.Now()
	var sess DeviceSession
	err = kernel.TenantTx(ctx, s.Pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		d, err := q.GetDevice(ctx, db.GetDeviceParams{TenantID: tenantID, ID: deviceID})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		if subtle.ConstantTimeCompare(d.SecretHash, hash) != 1 {
			return ErrInvalidToken
		}
		if d.RevokedAt != nil {
			return ErrDeviceRevoked
		}

		contact := db.RecordDeviceContactParams{TenantID: tenantID, ID: deviceID, Now: &now}
		if v := strings.TrimSpace(in.AppVersion); v != "" && len(v) <= 40 {
			contact.AppVersion = &v
		}
		if in.ClientTime != nil {
			if skew := now.Sub(*in.ClientTime); skew.Abs() <= maxClockSkew {
				ms := int32(skew.Milliseconds()) //nolint:gosec // bounded by maxClockSkew
				contact.ClockSkewMs = &ms
			}
		}
		if err := q.RecordDeviceContact(ctx, contact); err != nil {
			return err
		}
		sess = DeviceSession{TenantID: tenantID, DeviceID: deviceID, OutletID: d.OutletID}
		return nil
	})
	if err != nil {
		return DeviceSession{}, err
	}

	sess.AccessExpiresAt = now.Add(s.Config.DeviceAccessTTL)
	sess.AccessToken, err = s.DeviceKeys.sign(accessClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   sess.DeviceID.String(),
			Audience:  jwt.ClaimStrings{string(AudienceDevice)},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(sess.AccessExpiresAt),
			ID:        kernel.NewID().String(),
		},
		TenantID: tenantID.String(),
		DeviceID: sess.DeviceID.String(),
		OutletID: sess.OutletID.String(),
	})
	return sess, err
}

// CheckDevice confirms a device principal still exists and has not been revoked, and notes that it
// was seen (at most one write a minute). It runs on every request to a device route, so
// revocation takes effect at once rather than when the access token expires.
func (s *Service) CheckDevice(ctx context.Context, p Principal) error {
	return kernel.TenantTx(ctx, s.Pool, p.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		d, err := q.GetDevice(ctx, db.GetDeviceParams{TenantID: p.TenantID, ID: p.DeviceID})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		if d.RevokedAt != nil {
			return ErrDeviceRevoked
		}
		now := s.Clock.Now()
		stale := now.Add(-time.Minute)
		return q.TouchDevice(ctx, db.TouchDeviceParams{TenantID: p.TenantID, ID: p.DeviceID, Now: &now, StaleBefore: &stale})
	})
}

// GetDevice returns the device behind a device principal.
func (s *Service) GetDevice(ctx context.Context, p Principal) (Device, error) {
	var out Device
	err := kernel.TenantTx(ctx, s.Pool, p.TenantID, func(tx pgx.Tx) error {
		d, err := db.New(tx).GetDevice(ctx, db.GetDeviceParams{TenantID: p.TenantID, ID: p.DeviceID})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		out = toDevice(d)
		return nil
	})
	return out, err
}

// RosterStaff is one person on a device's lock screen.
type RosterStaff struct {
	StaffID     uuid.UUID
	DisplayName string
	// PINHash is the argon2id PHC string, or nil if the person has no PIN yet.
	PINHash     *string
	Permissions []Permission
}

// Roster is the staff a device at an outlet works with, with their permissions there.
type Roster struct {
	Device   Device
	OutletID uuid.UUID
	Staff    []RosterStaff
}

// GetRoster returns the roster for a device principal's outlet: active staff assigned to the
// outlet plus owners, with PIN hashes so the device can check PINs offline, and the permissions
// each person holds at the outlet so it can enforce them offline too. Two queries however many
// people there are.
func (s *Service) GetRoster(ctx context.Context, p Principal) (Roster, error) {
	out := Roster{OutletID: p.OutletID}
	err := kernel.TenantTx(ctx, s.Pool, p.TenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		d, err := q.GetDevice(ctx, db.GetDeviceParams{TenantID: p.TenantID, ID: p.DeviceID})
		if err != nil {
			return mapNoRows(err, ErrInvalidToken)
		}
		out.Device = toDevice(d)
		staff, err := q.ListRosterStaff(ctx, db.ListRosterStaffParams{TenantID: p.TenantID, OutletID: p.OutletID})
		if err != nil {
			return err
		}
		perms, err := q.ListRosterPermissions(ctx, db.ListRosterPermissionsParams{TenantID: p.TenantID, OutletID: p.OutletID})
		if err != nil {
			return err
		}
		byStaff := map[uuid.UUID][]Permission{}
		for _, r := range perms {
			byStaff[r.StaffID] = append(byStaff[r.StaffID], Permission(r.Permission))
		}
		out.Staff = make([]RosterStaff, len(staff))
		for i, st := range staff {
			ps := byStaff[st.ID]
			if st.IsOwner {
				ps = AllPermissions()
			}
			if ps == nil {
				ps = []Permission{}
			}
			out.Staff[i] = RosterStaff{StaffID: st.ID, DisplayName: st.DisplayName, PINHash: st.PinHash, Permissions: ps}
		}
		return nil
	})
	return out, err
}

func toDevice(d db.Device) Device {
	var skew *int
	if d.ClockSkewMs != nil {
		v := int(*d.ClockSkewMs)
		skew = &v
	}
	return Device{
		ID: d.ID, OutletID: d.OutletID, Code: int(d.DeviceCode), Name: d.Name, PairedBy: d.PairedBy,
		PairedAt: d.PairedAt, RevokedAt: d.RevokedAt, LastSeenAt: d.LastSeenAt, LastSyncAt: d.LastSyncAt,
		AppVersion: d.AppVersion, ClockSkewMs: skew,
	}
}
