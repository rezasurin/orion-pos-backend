// Package syncstub is a stand-in projector for testing the sync protocol itself, before and apart
// from any real event type (BACKEND_PLAN.md section 5.3). It handles a handful of made-up event
// types that exercise every outcome and failure the core must survive, and it writes to a table of
// its own (Install creates it), so the protocol tests do not depend on the sales module.
//
// It is test support: nothing in the running server uses it.
package syncstub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
)

// Event types the stub handles. Payloads are small JSON objects; see Project.
const (
	Created           = "test.created"             // {"label": "x"}: creates a record with the event's id
	Voided            = "test.voided"              // {"target": id}: voids a record; waits for it if it does not exist yet
	Rejects           = "test.rejects"             // always rejected
	WriteThenReject   = "test.write_then_reject"   // writes a record, then is rejected: the write must vanish
	WriteThenCrash    = "test.write_then_crash"    // writes a record, then fails like a server fault, "times" times
	Crashes           = "test.crashes"             // fails like a server fault, "times" times, then behaves like Created
	Slow              = "test.slow"                // {"ms": 200}: creates a record after holding its transaction open
	Reference         = "test.reference"           // {"tenant_of": id}: writes a row that belongs to another tenant
	CreatedThenRevoke = "test.created_then_revoke" // {"device": id, "by": userId}: creates a record and revokes a device in the same transaction
)

// ErrCrash is the server fault the stub simulates.
var ErrCrash = errors.New("syncstub: simulated server fault")

// Projector is the stub. The zero value is ready; Install must have been run on the database.
type Projector struct {
	// OnProject, when set, is called at the start of every projection. A test can cancel a context
	// from it to simulate the process dying part way through a push.
	OnProject func(syncsrv.Event)

	mu      sync.Mutex
	crashes map[uuid.UUID]int // event id -> how many times it has failed so far
}

// Handles implements sync.Projector.
func (p *Projector) Handles() map[string]int {
	out := map[string]int{}
	for _, t := range []string{Created, Voided, Rejects, WriteThenReject, WriteThenCrash, Crashes, Slow, Reference, CreatedThenRevoke} {
		out[t] = 2 // schema version 3 is unsupported, to test that
	}
	return out
}

type payload struct {
	Label    string     `json:"label"`
	Target   uuid.UUID  `json:"target"`
	Times    int        `json:"times"`
	MS       int        `json:"ms"`
	TenantOf *uuid.UUID `json:"tenant_of"`
	Device   uuid.UUID  `json:"device"`
	By       uuid.UUID  `json:"by"`
}

// Project implements sync.Projector.
func (p *Projector) Project(ctx context.Context, tx pgx.Tx, env syncsrv.Env, ev syncsrv.Event) (syncsrv.Outcome, error) {
	if p.OnProject != nil {
		p.OnProject(ev)
	}
	var in payload
	if err := json.Unmarshal(ev.Payload, &in); err != nil {
		return syncsrv.Rejected("bad_payload", err.Error()), nil
	}
	insert := func(tenant uuid.UUID) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO stub_record (tenant_id, id, label, device_id, staff_id, outlet_id, device_time, received_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			tenant, ev.ID, in.Label, env.DeviceID, ev.StaffID, env.OutletID, ev.DeviceTime, env.ReceivedAt)
		return err
	}

	switch ev.Type {
	case Created:
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		return syncsrv.Applied(ev.ID), nil

	case Voided:
		tag, err := tx.Exec(ctx, `UPDATE stub_record SET voided = true WHERE tenant_id = $1 AND id = $2 AND NOT voided`, env.TenantID, in.Target)
		if err != nil {
			return syncsrv.Outcome{}, err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stub_record WHERE tenant_id = $1 AND id = $2)`, env.TenantID, in.Target).Scan(&exists); err != nil {
				return syncsrv.Outcome{}, err
			}
			if !exists {
				return syncsrv.PendingOn(in.Target), nil
			}
			// Already voided: a second void is harmless.
		}
		return syncsrv.Applied(), nil

	case Rejects:
		return syncsrv.Rejected("stub_rejected", "the stub rejects this on purpose"), nil

	case WriteThenReject:
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		return syncsrv.Rejected("stub_rejected", "rejected after writing"), nil

	case WriteThenCrash:
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		if p.crashNow(ev.ID, in.Times) {
			return syncsrv.Outcome{}, ErrCrash
		}
		return syncsrv.Applied(ev.ID), nil

	case Crashes:
		if p.crashNow(ev.ID, in.Times) {
			return syncsrv.Outcome{}, ErrCrash
		}
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		return syncsrv.Applied(ev.ID), nil

	case Slow:
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		select {
		case <-time.After(time.Duration(in.MS) * time.Millisecond):
		case <-ctx.Done():
			return syncsrv.Outcome{}, ctx.Err()
		}
		return syncsrv.Applied(ev.ID), nil

	case Reference:
		// A write for a tenant other than the one this transaction is scoped to: row-level
		// security must refuse it, and the core must turn that into a rejection.
		if in.TenantOf == nil {
			return syncsrv.Rejected("bad_payload", "tenant_of is required"), nil
		}
		if err := insert(*in.TenantOf); err != nil {
			return syncsrv.Outcome{}, err
		}
		return syncsrv.Applied(ev.ID), nil

	case CreatedThenRevoke:
		if err := insert(env.TenantID); err != nil {
			return syncsrv.Outcome{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE device SET revoked_at = now(), revoked_by = $3 WHERE tenant_id = $1 AND id = $2`,
			env.TenantID, in.Device, in.By); err != nil {
			return syncsrv.Outcome{}, err
		}
		return syncsrv.Applied(ev.ID), nil
	}
	return syncsrv.Outcome{}, fmt.Errorf("syncstub: no handler for %q", ev.Type)
}

// crashNow reports whether event id should fail again, counting up to times failures in all.
func (p *Projector) crashNow(id uuid.UUID, times int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.crashes == nil {
		p.crashes = map[uuid.UUID]int{}
	}
	if p.crashes[id] >= times {
		return false
	}
	p.crashes[id]++
	return true
}

// Install creates the stub's table in a test database, with row-level security like every tenant
// table, using a connection that owns the schema.
func Install(ctx context.Context, owner *pgxpool.Pool) error {
	_, err := owner.Exec(ctx, `
		CREATE TABLE stub_record (
			tenant_id   uuid NOT NULL REFERENCES tenant (id),
			id          uuid NOT NULL,
			label       text NOT NULL DEFAULT '',
			voided      boolean NOT NULL DEFAULT false,
			device_id   uuid NOT NULL,
			staff_id    uuid NOT NULL,
			outlet_id   uuid NOT NULL,
			device_time timestamptz NOT NULL,
			received_at timestamptz NOT NULL,
			PRIMARY KEY (tenant_id, id)
		);
		ALTER TABLE stub_record ENABLE ROW LEVEL SECURITY;
		CREATE POLICY tenant_isolation ON stub_record TO orion_app
			USING (tenant_id = current_tenant_id()) WITH CHECK (tenant_id = current_tenant_id());
		GRANT SELECT, INSERT, UPDATE ON stub_record TO orion_app;`)
	return err
}
