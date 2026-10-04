// Package sync is the server side of the POS sync protocol (BACKEND_PLAN.md section 5): the push
// that takes events from a device, applies each one exactly once, and tells the device what became
// of it. The protocol and its tests are the most important code in the system (ADR 0004).
//
// A push is a list of events. Each event runs in its own transaction, so one bad event never
// blocks the rest:
//
//  1. The device must not be revoked. The transaction takes a share lock on the device row, so a
//     revocation waits for events in flight and nothing commits after it returns.
//  2. The event is claimed in sync_inbox by (device, idempotency key). If it was already there, the
//     stored outcome is returned without applying anything again, which is what makes every retry
//     safe. The same key with different content is a client bug (idempotency_conflict).
//  3. A projector for the event's type validates it and writes the module's own rows. It reports
//     whether the event was applied, rejected (malformed) or must wait for another record
//     (pending_dependency, for a void that arrives before its sale).
//  4. The outcome is stored on the inbox row, and applied events release any parked event that was
//     waiting for what they created, in the same transaction.
//
// Only malformed events are rejected. A sale whose totals look wrong, or made by someone without
// the permission, is accepted and flagged by its projector, because the money has already changed
// hands.
package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/sync/db"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

// MaxEvents is the most events one push may carry; the device pages through its outbox.
const MaxEvents = 500

// maxParkedRuns bounds how many parked events one applied event may release, a safety net against a
// loop of events waiting on each other.
const maxParkedRuns = 1000

// Event is one thing that happened on a device, as pushed.
type Event struct {
	ID             uuid.UUID // client-generated UUIDv7; the id of the record a create event makes
	IdempotencyKey uuid.UUID
	Type           string
	StaffID        uuid.UUID
	DeviceTime     time.Time
	SchemaVersion  int
	Payload        json.RawMessage // a JSON object
}

// Status is what the server says about one event.
type Status string

const (
	// StatusAccepted: the event is stored. When Code is "pending_dependency" it is parked until
	// the record it refers to arrives; the device should still treat it as delivered.
	StatusAccepted Status = "accepted"
	// StatusDuplicate: the event had already been accepted; nothing was applied again.
	StatusDuplicate Status = "duplicate"
	// StatusRejected: the event is malformed and will never be applied. The device keeps it for
	// support and does not resend it.
	StatusRejected Status = "rejected"
	// StatusRetry: the server could not process the event right now (an internal failure). Nothing
	// was recorded; the device keeps it and sends it again later.
	StatusRetry Status = "retry"
)

// Result is the outcome of one event.
type Result struct {
	ID     uuid.UUID
	Status Status
	Code   string
	Detail string
}

// PushRequest is a batch of events from one device.
type PushRequest struct {
	Events []Event
	Health DeviceHealth
}

// DeviceHealth is what a device volunteers about itself on a push or a pull, for the support view
// and the monitor that alerts when events stay unsynced (BACKEND_PLAN.md section 4.11). All of it is
// optional, and a bad value is ignored rather than failing the sync it came with.
type DeviceHealth struct {
	ClientTime *time.Time // the device's clock now, to measure skew
	AppVersion *string
	// Unsynced is how many events remain in the device's outbox after this call, and
	// OldestUnsynced the device time of the oldest; nil means the device did not say.
	Unsynced       *int
	OldestUnsynced *time.Time
}

// maxReportedUnsynced is the largest outbox count taken at face value.
const maxReportedUnsynced = 1_000_000

func (h DeviceHealth) stamp(log *slog.Logger) identity.SyncStamp {
	st := identity.SyncStamp{ClientTime: h.ClientTime, AppVersion: h.AppVersion}
	switch {
	case h.Unsynced == nil:
	case *h.Unsynced < 0 || *h.Unsynced > maxReportedUnsynced || (*h.Unsynced > 0 && h.OldestUnsynced == nil):
		log.Warn("sync: ignoring an outbox report that does not make sense")
	default:
		st.Outbox = &identity.Outbox{Unsynced: *h.Unsynced, Oldest: h.OldestUnsynced}
	}
	return st
}

// PushResponse has one result per event, in order.
type PushResponse struct {
	Results    []Result
	ServerTime time.Time
}

// Env is what a projector knows about where an event came from.
type Env struct {
	TenantID   uuid.UUID
	DeviceID   uuid.UUID
	OutletID   uuid.UUID
	DeviceCode int
	// Staff is the person who performed the action, as the server knows them now.
	Staff identity.Acting
	// ReceivedAt is when the server first got the event; a parked event keeps its original time.
	ReceivedAt time.Time
}

// Deps are what a Service needs.
type Deps struct {
	Pool       *pgxpool.Pool
	Identity   *identity.Service
	Projectors []Projector
	Clock      kernel.Clock
	Logger     *slog.Logger

	// Needed for Pull; a Service used only to push may leave them nil.
	Catalog      *catalog.Service
	Tenancy      *tenancy.Service
	Entitlements *entitlements.Resolver
}

// Service accepts pushes from devices.
type Service struct {
	pool   *pgxpool.Pool
	ids    *identity.Service
	byType map[string]projectorEntry
	clock  kernel.Clock
	log    *slog.Logger
	cat    *catalog.Service
	ten    *tenancy.Service
	ent    *entitlements.Resolver
}

type projectorEntry struct {
	p          Projector
	maxVersion int
}

// NewService registers the projectors. Two projectors may not claim one event type.
func NewService(d Deps) (*Service, error) {
	s := &Service{pool: d.Pool, ids: d.Identity, byType: map[string]projectorEntry{}, clock: d.Clock, log: d.Logger, cat: d.Catalog, ten: d.Tenancy, ent: d.Entitlements}
	if s.clock == nil {
		s.clock = kernel.SystemClock{}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	for _, p := range d.Projectors {
		for typ, max := range p.Handles() {
			if _, dup := s.byType[typ]; dup {
				return nil, fmt.Errorf("sync: event type %q has two projectors", typ)
			}
			if max < 1 {
				return nil, fmt.Errorf("sync: event type %q must support at least schema version 1", typ)
			}
			s.byType[typ] = projectorEntry{p: p, maxVersion: max}
		}
	}
	return s, nil
}

// Push applies a device's events in order and reports each outcome. A revoked device gets
// identity.ErrDeviceRevoked and nothing is accepted; if the device is revoked part way through, the
// events after that point are rejected as device_revoked and not recorded.
func (s *Service) Push(ctx context.Context, p identity.Principal, req PushRequest) (PushResponse, error) {
	if len(req.Events) == 0 {
		return PushResponse{}, fmt.Errorf("%w: a push needs at least one event", kernel.ErrValidation)
	}
	if len(req.Events) > MaxEvents {
		return PushResponse{}, fmt.Errorf("%w: a push has at most %d events", kernel.ErrValidation, MaxEvents)
	}
	start := s.clock.Now()
	dev, err := s.ids.GetDevice(ctx, p)
	if err != nil {
		return PushResponse{}, err
	}

	run := &pushRun{s: s, tenantID: p.TenantID, deviceID: p.DeviceID, outletID: p.OutletID, deviceCode: dev.Code, acting: map[uuid.UUID]identity.Acting{}}
	results := make([]Result, len(req.Events))
	revoked := false
	counts := map[Status]int{}
	for i, ev := range req.Events {
		if err := ctx.Err(); err != nil {
			return PushResponse{}, err
		}
		if revoked {
			results[i] = Result{ID: ev.ID, Status: StatusRejected, Code: "device_revoked", Detail: "the device was revoked"}
			counts[StatusRejected]++
			continue
		}
		res, err := run.one(ctx, ev)
		switch {
		case errors.Is(err, identity.ErrDeviceRevoked):
			if i == 0 {
				return PushResponse{}, err
			}
			revoked = true
			results[i] = Result{ID: ev.ID, Status: StatusRejected, Code: "device_revoked", Detail: "the device was revoked"}
		case err != nil:
			s.log.ErrorContext(ctx, "sync: event could not be processed",
				slog.String("event_id", ev.ID.String()), slog.String("event_type", ev.Type), slog.Any("error", err))
			results[i] = Result{ID: ev.ID, Status: StatusRetry, Code: "internal", Detail: "the server could not process this event; send it again later"}
		default:
			results[i] = res
		}
		counts[results[i].Status]++
	}

	// Bookkeeping for the support view; it must not turn a successful push into a failure.
	if err := kernel.TenantTx(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		return s.ids.RecordSync(ctx, tx, p.TenantID, p.DeviceID, req.Health.stamp(s.log))
	}); err != nil {
		s.log.WarnContext(ctx, "sync: could not record the push on the device", slog.Any("error", err))
	}
	s.log.InfoContext(ctx, "sync push",
		slog.Int("events", len(req.Events)), slog.Int("accepted", counts[StatusAccepted]), slog.Int("duplicate", counts[StatusDuplicate]),
		slog.Int("rejected", counts[StatusRejected]), slog.Int("retry", counts[StatusRetry]),
		slog.Duration("duration", s.clock.Now().Sub(start)))
	return PushResponse{Results: results, ServerTime: s.clock.Now()}, nil
}

// pushRun is the state of one push: who is pushing, and what has been looked up already.
type pushRun struct {
	s          *Service
	tenantID   uuid.UUID
	deviceID   uuid.UUID
	outletID   uuid.UUID
	deviceCode int
	acting     map[uuid.UUID]identity.Acting // staff seen so far in this push
}

func (r *pushRun) one(ctx context.Context, ev Event) (Result, error) {
	if code, detail := checkEnvelope(ev); code != "" {
		// Not even storable, so there is nothing to look up or keep.
		return Result{ID: ev.ID, Status: StatusRejected, Code: code, Detail: detail}, nil
	}
	hash, err := envelopeHash(ev)
	if err != nil {
		return Result{ID: ev.ID, Status: StatusRejected, Code: "malformed", Detail: "the payload is not valid JSON"}, nil
	}

	var res Result
	err = kernel.TenantTx(ctx, r.s.pool, r.tenantID, func(tx pgx.Tx) error {
		res = Result{} // the transaction may run again after a deadlock
		if err := r.s.ids.LockDeviceActive(ctx, tx, r.tenantID, r.deviceID); err != nil {
			return err
		}
		q := db.New(tx)
		received := r.s.clock.Now()
		n, err := q.InsertInbox(ctx, db.InsertInboxParams{
			TenantID: r.tenantID, DeviceID: r.deviceID, IdempotencyKey: ev.IdempotencyKey, ID: ev.ID, OutletID: r.outletID,
			StaffID: ev.StaffID, EventType: ev.Type, SchemaVersion: int32(ev.SchemaVersion), //nolint:gosec // checked by checkEnvelope
			DeviceTime: ev.DeviceTime, PayloadHash: hash, Payload: ev.Payload, ReceivedAt: received,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			res, err = r.existing(ctx, q, ev, hash)
			return err
		}

		acting, ok := r.acting[ev.StaffID]
		if !ok {
			if acting, err = r.s.ids.LoadActing(ctx, tx, r.tenantID, ev.StaffID, r.outletID); err != nil {
				return err
			}
			r.acting[ev.StaffID] = acting
		}
		env := Env{TenantID: r.tenantID, DeviceID: r.deviceID, OutletID: r.outletID, DeviceCode: r.deviceCode, Staff: acting, ReceivedAt: received}
		out, err := r.s.apply(ctx, tx, env, ev)
		if err != nil {
			return err
		}
		if err := setOutcome(ctx, q, r.tenantID, r.deviceID, ev.IdempotencyKey, out, received); err != nil {
			return err
		}
		res = out.result(ev.ID)
		if out.Status == outcomeApplied {
			return r.s.releaseParked(ctx, tx, r.tenantID, out.Provides)
		}
		return nil
	})
	return res, err
}

// existing answers for an event that was already pushed, from what was stored.
func (r *pushRun) existing(ctx context.Context, q *db.Queries, ev Event, hash []byte) (Result, error) {
	row, err := q.GetInboxByKey(ctx, db.GetInboxByKeyParams{TenantID: r.tenantID, DeviceID: r.deviceID, IdempotencyKey: ev.IdempotencyKey})
	if errors.Is(err, pgx.ErrNoRows) {
		// The key is new, so the clash was on the event id: another device already used it.
		r.s.log.ErrorContext(ctx, "sync: event id already used by another device",
			slog.String("event_id", ev.ID.String()), slog.String("device_id", r.deviceID.String()))
		return Result{ID: ev.ID, Status: StatusRejected, Code: "id_conflict", Detail: "this event id was already used by another device"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(row.PayloadHash, hash) {
		r.s.log.ErrorContext(ctx, "sync: idempotency key reused with different content; this is a client bug",
			slog.String("event_id", ev.ID.String()), slog.String("idempotency_key", ev.IdempotencyKey.String()),
			slog.String("device_id", r.deviceID.String()), slog.String("event_type", ev.Type))
		return Result{ID: ev.ID, Status: StatusRejected, Code: "idempotency_conflict", Detail: "this idempotency key was already used for different content"}, nil
	}
	switch row.Status {
	case "accepted":
		return Result{ID: ev.ID, Status: StatusDuplicate}, nil
	case "pending_dependency":
		return Result{ID: ev.ID, Status: StatusDuplicate, Code: "pending_dependency"}, nil
	case "rejected":
		return Result{ID: ev.ID, Status: StatusRejected, Code: derefOr(row.Code, "rejected"), Detail: derefOr(row.Detail, "")}, nil
	}
	return Result{}, fmt.Errorf("sync: inbox row in state %q outside its transaction", row.Status)
}

// apply validates what the core can check and hands the event to its projector, inside a savepoint
// so a rejected or parked event leaves nothing behind.
func (s *Service) apply(ctx context.Context, tx pgx.Tx, env Env, ev Event) (Outcome, error) {
	entry, ok := s.byType[ev.Type]
	switch {
	case !ok:
		return Rejected("unknown_type", fmt.Sprintf("event type %q is not known", ev.Type)), nil
	case ev.SchemaVersion > entry.maxVersion:
		return Rejected("unsupported_schema_version", fmt.Sprintf("%s supports schema versions up to %d", ev.Type, entry.maxVersion)), nil
	case !env.Staff.Known:
		return Rejected("unknown_staff", "the staff member is not part of this business"), nil
	}
	if code, detail := checkOutlet(ev.Payload, env.OutletID); code != "" {
		return Rejected(code, detail), nil
	}

	sp, err := tx.Begin(ctx)
	if err != nil {
		return Outcome{}, err
	}
	out, err := entry.p.Project(ctx, sp, env, ev)
	if err != nil {
		_ = sp.Rollback(ctx)
		if rej, ok := rejectionFor(err); ok {
			s.log.WarnContext(ctx, "sync: event rejected by the database",
				slog.String("event_id", ev.ID.String()), slog.String("event_type", ev.Type), slog.Any("error", err))
			return rej, nil
		}
		return Outcome{}, err
	}
	if out.Status == outcomeApplied {
		return out, sp.Commit(ctx)
	}
	return out, sp.Rollback(ctx)
}

// releaseParked runs the events waiting for any of the given records, now that they exist, and
// keeps going with whatever those events create in turn.
func (s *Service) releaseParked(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ids []uuid.UUID) error {
	q := db.New(tx)
	for runs := 0; len(ids) > 0; {
		rows, err := q.ListParkedOn(ctx, db.ListParkedOnParams{TenantID: tenantID, Ids: ids})
		if err != nil {
			return err
		}
		ids = nil
		for _, row := range rows {
			if runs++; runs > maxParkedRuns {
				return fmt.Errorf("sync: more than %d parked events released by one event", maxParkedRuns)
			}
			ev := Event{
				ID: row.ID, IdempotencyKey: row.IdempotencyKey, Type: row.EventType, StaffID: row.StaffID,
				DeviceTime: row.DeviceTime, SchemaVersion: int(row.SchemaVersion), Payload: row.Payload,
			}
			acting, err := s.ids.LoadActing(ctx, tx, row.TenantID, row.StaffID, row.OutletID)
			if err != nil {
				return err
			}
			dev, err := s.ids.DeviceInTx(ctx, tx, row.TenantID, row.DeviceID)
			if err != nil {
				return err
			}
			env := Env{TenantID: row.TenantID, DeviceID: row.DeviceID, OutletID: row.OutletID, DeviceCode: dev.Code, Staff: acting, ReceivedAt: row.ReceivedAt}
			out, err := s.apply(ctx, tx, env, ev)
			if err != nil {
				return err
			}
			if err := setOutcome(ctx, q, row.TenantID, row.DeviceID, row.IdempotencyKey, out, s.clock.Now()); err != nil {
				return err
			}
			if out.Status == outcomeApplied {
				ids = append(ids, out.Provides...)
			}
		}
	}
	return nil
}

func setOutcome(ctx context.Context, q *db.Queries, tenantID, deviceID, key uuid.UUID, out Outcome, now time.Time) error {
	p := db.SetInboxOutcomeParams{TenantID: tenantID, DeviceID: deviceID, IdempotencyKey: key}
	switch out.Status {
	case outcomeApplied:
		p.Status, p.AppliedAt = "accepted", &now
	case outcomeRejected:
		p.Status, p.Code, p.Detail = "rejected", &out.Code, &out.Detail
	case outcomePending:
		code := "pending_dependency"
		p.Status, p.Code, p.DependsOn = "pending_dependency", &code, &out.DependsOn
	default:
		return fmt.Errorf("sync: projector returned no outcome")
	}
	return q.SetInboxOutcome(ctx, p)
}

// rejectionFor turns a database error that says "this data cannot be stored" into a rejection: a
// reference to a row that does not exist (or exists in another business, which row-level security
// hides), a value out of range, a duplicate of something unique. Anything else is the server's
// problem and is retried.
func rejectionFor(err error) (Outcome, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return Outcome{}, false
	}
	switch {
	case pgErr.Code == "23503" || (pgErr.Code == "42501" && containsRLS(pgErr.Message)):
		return Rejected("unknown_reference", "the event refers to something that does not exist in this business"), true
	case pgErr.Code == "23505":
		return Rejected("duplicate", "the event repeats something that already exists ("+pgErr.ConstraintName+")"), true
	case len(pgErr.Code) == 5 && (pgErr.Code[:2] == "22" || pgErr.Code[:2] == "23"):
		return Rejected("invalid_value", "a value in the event is not acceptable ("+pgErr.ConstraintName+")"), true
	}
	return Outcome{}, false
}

func containsRLS(msg string) bool { return bytes.Contains([]byte(msg), []byte("row-level security")) }

// checkEnvelope rejects what cannot even be stored: a malformed type, version, or ids.
func checkEnvelope(ev Event) (code, detail string) {
	switch {
	case ev.ID == uuid.Nil || ev.IdempotencyKey == uuid.Nil:
		return "malformed", "id and idempotency_key are required"
	case ev.StaffID == uuid.Nil:
		return "malformed", "staff_id is required"
	case ev.DeviceTime.IsZero():
		return "malformed", "device_time is required"
	case ev.SchemaVersion < 1 || ev.SchemaVersion > 1000:
		return "malformed", "schema_version must be a positive number"
	case !validType(ev.Type):
		return "malformed", "type must be lowercase words joined by dots, like sale.completed"
	}
	trimmed := bytes.TrimSpace(ev.Payload)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return "malformed", "payload must be a JSON object"
	}
	return "", ""
}

func validType(t string) bool {
	if len(t) < 3 || len(t) > 60 {
		return false
	}
	dots := 0
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c == '.' && i > 0 && i < len(t)-1 && t[i-1] != '.':
			dots++
		default:
			return false
		}
	}
	return dots >= 1
}

// checkOutlet rejects an event whose payload names a different outlet from the device's.
func checkOutlet(payload json.RawMessage, outletID uuid.UUID) (code, detail string) {
	var probe struct {
		OutletID *uuid.UUID `json:"outlet_id"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return "malformed", "outlet_id is not a UUID"
	}
	if probe.OutletID != nil && *probe.OutletID != outletID {
		return "wrong_outlet", "the event belongs to a different outlet from this device's"
	}
	return "", ""
}

// envelopeHash identifies an event's content, independent of key order and whitespace in the JSON.
func envelopeHash(ev Event) ([]byte, error) {
	canon, err := canonicalJSON(ev.Payload)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%d\n%s\n", ev.Type, ev.StaffID, ev.SchemaVersion, ev.DeviceTime.UTC().Format(time.RFC3339Nano))
	h.Write(canon)
	return h.Sum(nil), nil
}

// canonicalJSON re-encodes JSON with sorted object keys and numbers left exactly as written.
func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
