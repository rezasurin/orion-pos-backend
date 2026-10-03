package kernel

import (
	"context"
	"encoding/json"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Actor says who is performing a request, for the audit log. Authentication middleware sets it.
type Actor struct {
	Type string // "user", "device" or "system"
	ID   uuid.UUID
	IP   string // as the service saw it; empty when unknown
}

type actorKey struct{}

// WithActor returns a context carrying the acting principal.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor in ctx, or the system actor outside a request (jobs, commands).
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok {
		return a
	}
	return Actor{Type: "system"}
}

// AuditEntry is one line of the tenant audit log (BACKEND_PLAN.md section 4.7).
type AuditEntry struct {
	Action     string // "staff.pin_rotated": lowercase words joined by dots
	TargetType string
	TargetID   uuid.UUID
	// Detail is stored as JSON. It must never hold secrets: no PINs, passwords or tokens.
	Detail map[string]any
}

// RecordAudit appends an entry for the current tenant, attributed to the actor in ctx. Call it in
// the same transaction as the action it records, so the two commit or roll back together.
func RecordAudit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	a := ActorFrom(ctx)
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	var actorID, targetID *uuid.UUID
	if a.ID != uuid.Nil {
		actorID = &a.ID
	}
	if e.TargetID != uuid.Nil {
		targetID = &e.TargetID
	}
	var ip *netip.Addr
	if addr, err := netip.ParseAddr(a.IP); err == nil {
		ip = &addr
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO tenant_audit_log (id, tenant_id, actor_type, actor_id, action, target_type, target_id, detail, ip)
		VALUES ($1, current_tenant_id(), $2, $3, $4, $5, $6, $7, $8)`,
		NewID(), a.Type, actorID, e.Action, e.TargetType, targetID, raw, ip)
	return err
}
