package identity

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/identity/db"
)

// RosterDelta is what a device needs to hear about staff after some of their records changed.
type RosterDelta struct {
	// Staff are the people now on the outlet's roster whose records changed, with PIN hashes and
	// permissions at the outlet.
	Staff []RosterStaff
	// Removed are people who changed and are no longer on the roster: deactivated, or no longer
	// assigned to the outlet. The device forgets them.
	Removed []uuid.UUID
}

// PullRoster returns the roster entries for the pull, inside the caller's transaction: for the
// staff ids given, or with all set for the whole roster (a full snapshot). A fixed number of
// queries however many people changed.
func (s *Service) PullRoster(ctx context.Context, tx pgx.Tx, tenantID, outletID uuid.UUID, staffIDs []uuid.UUID, all bool) (RosterDelta, error) {
	q := db.New(tx)
	if all {
		rows, err := q.ListRosterStaff(ctx, db.ListRosterStaffParams{TenantID: tenantID, OutletID: outletID})
		if err != nil {
			return RosterDelta{}, err
		}
		staffIDs = make([]uuid.UUID, len(rows))
		for i, r := range rows {
			staffIDs[i] = r.ID
		}
	}
	if len(staffIDs) == 0 {
		return RosterDelta{Staff: []RosterStaff{}, Removed: []uuid.UUID{}}, nil
	}
	people, err := q.ListRosterCandidates(ctx, db.ListRosterCandidatesParams{TenantID: tenantID, OutletID: outletID, StaffIds: staffIDs})
	if err != nil {
		return RosterDelta{}, err
	}
	perms, err := q.ListPermissionsOfStaffAt(ctx, db.ListPermissionsOfStaffAtParams{TenantID: tenantID, OutletID: outletID, StaffIds: staffIDs})
	if err != nil {
		return RosterDelta{}, err
	}
	byStaff := map[uuid.UUID][]Permission{}
	for _, p := range perms {
		byStaff[p.StaffID] = append(byStaff[p.StaffID], Permission(p.Permission))
	}

	out := RosterDelta{Staff: []RosterStaff{}, Removed: []uuid.UUID{}}
	for _, p := range people {
		if !p.OnRoster {
			out.Removed = append(out.Removed, p.ID)
			continue
		}
		ps := byStaff[p.ID]
		if p.IsOwner {
			ps = AllPermissions()
		}
		if ps == nil {
			ps = []Permission{}
		}
		out.Staff = append(out.Staff, RosterStaff{StaffID: p.ID, DisplayName: p.DisplayName, PINHash: p.PinHash, Permissions: ps})
	}
	return out, nil
}
