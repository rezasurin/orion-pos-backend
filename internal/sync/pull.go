package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

const (
	// DefaultPullLimit and MaxPullLimit bound how many change log entries one pull turns into
	// entities; the device asks again while HasMore is set.
	DefaultPullLimit = 500
	MaxPullLimit     = 1000

	// EntitlementsTTL is how long a device may keep applying the entitlements it was sent without
	// hearing from the server again (BACKEND_PLAN.md section 4.5). The POS applies new ones at its
	// next sync and never in the middle of a shift.
	EntitlementsTTL = 7 * 24 * time.Hour
)

// PullRequest asks for what changed since a cursor.
type PullRequest struct {
	// Cursor is the opaque value from the previous response. Empty, unreadable, or ahead of the
	// server (a restored database) all mean "send everything".
	Cursor string
	Limit  int
}

// PullResponse is everything the device needs to bring its local copy up to date.
type PullResponse struct {
	// Cursor is what to send next time. Store it only after applying the response.
	Cursor string
	// Snapshot says this response is the whole state, not a delta: the device replaces its copy of
	// the catalog and roster instead of merging into it.
	Snapshot bool
	// HasMore says more changes are waiting; ask again straight away with Cursor.
	HasMore    bool
	ServerTime time.Time

	// Outlet is the device's outlet with its settings, present when either changed (and in a
	// snapshot).
	Outlet *tenancy.Outlet
	// Catalog is the current state of each changed catalog entity. Archived entities are included,
	// marked archived.
	Catalog catalog.PullState
	// Staff are roster entries that changed, with PIN hashes and permissions at this outlet;
	// RemovedStaff are people who changed and are no longer on the roster.
	Staff        []identity.RosterStaff
	RemovedStaff []uuid.UUID
	// Deleted lists entities removed outright. Nothing in Phase 1 is deleted (the catalog is
	// archived), so this stays empty; it is part of the contract for later.
	Deleted []Deleted

	Entitlements          entitlements.Snapshot
	EntitlementsExpiresAt time.Time
}

// Deleted is a tombstone.
type Deleted struct {
	Type string
	ID   uuid.UUID
}

// Pull returns the changes since req.Cursor for the device's outlet: the current state of each
// entity that changed (not its history), found through the tenant's change log (section 5.2).
//
// A fixed number of queries runs whatever changed: one for the log, one per kind of entity.
func (s *Service) Pull(ctx context.Context, p identity.Principal, req PullRequest) (PullResponse, error) {
	if s.cat == nil || s.ten == nil || s.ent == nil {
		return PullResponse{}, fmt.Errorf("sync: pull is not configured")
	}
	limit := req.Limit
	switch {
	case limit <= 0:
		limit = DefaultPullLimit
	case limit > MaxPullLimit:
		limit = MaxPullLimit
	}

	resp := PullResponse{ServerTime: s.clock.Now()}
	err := kernel.TenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		resp = PullResponse{ServerTime: resp.ServerTime} // the transaction may run again

		// Read the head first: everything numbered up to it is committed and visible below, and
		// what commits later is numbered higher and comes in the next pull.
		head, err := kernel.ChangeHead(ctx, tx)
		if err != nil {
			return err
		}
		after, ok := parseCursor(req.Cursor)
		resp.Snapshot = !ok || after > head
		resp.Cursor = formatCursor(head)

		var changes []kernel.Change
		if !resp.Snapshot {
			if changes, err = kernel.ChangesSince(ctx, tx, after, head, p.OutletID, limit+1); err != nil {
				return err
			}
			if len(changes) > limit {
				changes = changes[:limit]
				resp.HasMore = true
				resp.Cursor = formatCursor(changes[limit-1].Seq)
			}
		}

		ids, outletChanged, deleted := sortChanges(changes)
		resp.Deleted = deleted
		if resp.Catalog, err = s.cat.PullState(ctx, tx, p.TenantID, p.OutletID, ids.catalog, resp.Snapshot); err != nil {
			return err
		}
		roster, err := s.ids.PullRoster(ctx, tx, p.TenantID, p.OutletID, ids.staff, resp.Snapshot)
		if err != nil {
			return err
		}
		resp.Staff, resp.RemovedStaff = roster.Staff, roster.Removed

		if resp.Snapshot || outletChanged {
			o, err := s.ten.OutletInTx(ctx, tx, p.TenantID, p.OutletID)
			if err != nil {
				return err
			}
			resp.Outlet = &o
		}
		return nil
	})
	if err != nil {
		return PullResponse{}, err
	}

	// Entitlements ride on every pull: they can change without the change log (an operator sets an
	// override), and the snapshot is cached.
	if resp.Entitlements, err = s.ent.Snapshot(ctx, p.TenantID); err != nil {
		return PullResponse{}, err
	}
	resp.EntitlementsExpiresAt = resp.ServerTime.Add(EntitlementsTTL)
	return resp, nil
}

type changedIDs struct {
	catalog catalog.PullIDs
	staff   []uuid.UUID
}

// sortChanges turns a run of change log entries into the ids of the entities to reload, each once,
// and the tombstones. The last entry for an entity decides whether it is reloaded or deleted.
func sortChanges(changes []kernel.Change) (ids changedIDs, outletChanged bool, deleted []Deleted) {
	last := map[string]map[uuid.UUID]string{}
	for _, c := range changes {
		if last[c.EntityType] == nil {
			last[c.EntityType] = map[uuid.UUID]string{}
		}
		last[c.EntityType][c.EntityID] = c.Op
	}
	deleted = []Deleted{}
	pick := func(entityType string) []uuid.UUID {
		var out []uuid.UUID
		for id, op := range last[entityType] {
			if op == "delete" {
				deleted = append(deleted, Deleted{Type: entityType, ID: id})
				continue
			}
			out = append(out, id)
		}
		return out
	}
	ids.catalog.Categories = pick(catalog.EntityCategory)
	ids.catalog.Items = pick(catalog.EntityItem)
	ids.catalog.Groups = pick(catalog.EntityModifierGroup)
	ids.catalog.Variants = pick(catalog.EntityOutletVariant)
	ids.staff = pick("staff")
	outletChanged = len(last["outlet"])+len(last["outlet_settings"]) > 0

	known := map[string]bool{
		catalog.EntityCategory: true, catalog.EntityItem: true, catalog.EntityModifierGroup: true, catalog.EntityOutletVariant: true,
		"staff": true, "outlet": true, "outlet_settings": true,
	}
	for entityType := range last {
		if !known[entityType] {
			slog.Warn("sync: change log entry of a kind the pull does not send", slog.String("entity_type", entityType))
		}
	}
	return ids, outletChanged, deleted
}

// formatCursor and parseCursor keep the cursor opaque to devices. It is the last change number the
// device has seen.
func formatCursor(seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("c1:" + strconv.FormatInt(seq, 10)))
}

func parseCursor(c string) (int64, bool) {
	if c == "" {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	n, ok := strings.CutPrefix(string(raw), "c1:")
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseInt(n, 10, 64)
	if err != nil || seq < 0 {
		return 0, false
	}
	return seq, true
}
