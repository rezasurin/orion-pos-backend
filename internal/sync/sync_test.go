package sync_test

// The sync protocol tests (BACKEND_PLAN.md section 5.3, task B1.3). They run the real push path
// against a real database with a stub projector, so the protocol is proven before any real event
// type exists, and they are written to run again against the real projectors (B1.5) unchanged in
// spirit: every property here is about what the protocol guarantees, not about what an event means.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	gosync "sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rezasurin/orion-pos-backend/internal/catalog"
	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/sync/syncstub"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
	"github.com/rezasurin/orion-pos-backend/internal/testdb"
)

func TestMain(m *testing.M) { testdb.Main(m) }

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

type world struct {
	t       *testing.T
	d       *testdb.DB
	ids     *identity.Service
	tenants *tenancy.Service
	stub    *syncstub.Projector
	svc     *syncsrv.Service
	now     time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	d := testdb.New(t)
	if err := syncstub.Install(context.Background(), d.Owner); err != nil {
		t.Fatal(err)
	}
	clock := kernel.FixedClock{T: t0}
	ids, err := identity.NewService(identity.Deps{
		Pool: d.App, Clock: clock, TenantKeys: identity.NewEphemeralKeyring(), DeviceKeys: identity.NewEphemeralKeyring(),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	stub := &syncstub.Projector{}
	svc, err := syncsrv.NewService(syncsrv.Deps{
		Pool: d.App, Identity: ids, Projectors: []syncsrv.Projector{stub}, Clock: clock,
		Catalog: catalog.NewService(d.App), Tenancy: tenancy.NewService(d.App), Entitlements: entitlements.NewResolver(d.App, clock),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &world{t: t, d: d, ids: ids, tenants: tenancy.NewService(d.App), stub: stub, svc: svc, now: t0}
}

// biz is one business with an outlet, an owner and a cashier.
type biz struct {
	w      *world
	tenant tenancy.Tenant
	outlet tenancy.Outlet
	owner  identity.Caller
	staff  uuid.UUID // the cashier who acts in events
}

func (w *world) business(slug, code string) *biz {
	w.t.Helper()
	ctx := context.Background()
	tn, o, err := w.tenants.CreateTenant(ctx, tenancy.NewTenant{Name: slug, Slug: slug, Outlet: tenancy.NewOutlet{Name: "Main", Code: code}})
	if err != nil {
		w.t.Fatal(err)
	}
	u, err := w.ids.CreateMember(ctx, identity.NewMember{
		TenantID: tn.ID, Email: "owner@" + slug + ".test", Password: "correct horse battery", DisplayName: "Owner", IsOwner: true, EmailVerified: true,
	})
	if err != nil {
		w.t.Fatal(err)
	}
	owner := identity.Caller{Principal: identity.Principal{Type: identity.PrincipalUser, TenantID: tn.ID, UserID: u.ID}, Access: identity.Access{IsOwner: true}}
	roles, err := w.ids.ListRoles(ctx, tn.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	var cashierRole uuid.UUID
	for _, r := range roles {
		if r.Name == "Cashier" {
			cashierRole = r.ID
		}
	}
	st, err := w.ids.CreateStaff(ctx, owner, identity.NewStaff{
		DisplayName: "Sari", PIN: "4821", OutletRoles: []identity.OutletRole{{OutletID: o.ID, RoleID: cashierRole}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	return &biz{w: w, tenant: tn, outlet: o, owner: owner, staff: st.ID}
}

// device pairs a new tablet at the business's outlet.
func (b *biz) device(name string) identity.Principal {
	b.w.t.Helper()
	paired, err := b.w.ids.PairDevice(context.Background(), b.owner, identity.NewDevice{OutletID: b.outlet.ID, Name: name})
	if err != nil {
		b.w.t.Fatal(err)
	}
	return identity.Principal{Type: identity.PrincipalDevice, TenantID: b.tenant.ID, DeviceID: paired.Device.ID, OutletID: b.outlet.ID}
}

// event builds an event from the business's cashier at the fixed time, with fresh ids.
func (b *biz) event(typ string, payload any) syncsrv.Event {
	id := kernel.NewID()
	return b.eventWithID(id, typ, payload)
}

func (b *biz) eventWithID(id uuid.UUID, typ string, payload any) syncsrv.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		b.w.t.Fatal(err)
	}
	return syncsrv.Event{ID: id, IdempotencyKey: id, Type: typ, StaffID: b.staff, DeviceTime: t0, SchemaVersion: 1, Payload: raw}
}

func (b *biz) created(label string) syncsrv.Event {
	return b.event(syncstub.Created, map[string]any{"label": label})
}

func (b *biz) push(dev identity.Principal, events ...syncsrv.Event) syncsrv.PushResponse {
	b.w.t.Helper()
	res, err := b.w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: events})
	if err != nil {
		b.w.t.Fatalf("push: %v", err)
	}
	if len(res.Results) != len(events) {
		b.w.t.Fatalf("%d results for %d events", len(res.Results), len(events))
	}
	for i, r := range res.Results {
		if r.ID != events[i].ID {
			b.w.t.Fatalf("result %d is for %s, want %s", i, r.ID, events[i].ID)
		}
	}
	return res
}

func statuses(res syncsrv.PushResponse) []syncsrv.Status {
	out := make([]syncsrv.Status, len(res.Results))
	for i, r := range res.Results {
		out[i] = r.Status
	}
	return out
}

func codes(res syncsrv.PushResponse) []string {
	out := make([]string, len(res.Results))
	for i, r := range res.Results {
		out[i] = r.Code
	}
	return out
}

type record struct {
	Label  string
	Voided bool
}

// records is the stub's table for a business, read around row-level security.
func (b *biz) records() map[uuid.UUID]record {
	b.w.t.Helper()
	rows, err := b.w.d.Owner.Query(context.Background(), `SELECT id, label, voided FROM stub_record WHERE tenant_id = $1`, b.tenant.ID)
	if err != nil {
		b.w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]record{}
	for rows.Next() {
		var id uuid.UUID
		var r record
		if err := rows.Scan(&id, &r.Label, &r.Voided); err != nil {
			b.w.t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

type inboxRow struct {
	Status, Code string
}

func (b *biz) inbox() map[uuid.UUID]inboxRow {
	b.w.t.Helper()
	rows, err := b.w.d.Owner.Query(context.Background(), `SELECT id, status, coalesce(code, '') FROM sync_inbox WHERE tenant_id = $1`, b.tenant.ID)
	if err != nil {
		b.w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]inboxRow{}
	for rows.Next() {
		var id uuid.UUID
		var r inboxRow
		if err := rows.Scan(&id, &r.Status, &r.Code); err != nil {
			b.w.t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

// settled fails the test if any inbox row is still in the transient 'received' state, which can
// only be left behind by a bug: it exists only inside the transaction that projects the event.
func (w *world) settled() {
	w.t.Helper()
	var n int
	if err := w.d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM sync_inbox WHERE status = 'received'`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	if n != 0 {
		w.t.Errorf("%d inbox rows left in the 'received' state", n)
	}
}

// ---- duplicates ----

func TestDuplicatePushesChangeNothing(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	batch := []syncsrv.Event{b.created("a"), b.created("b"), b.event(syncstub.Voided, map[string]any{"target": uuid.Nil}), b.event(syncstub.Rejects, map[string]any{})}
	// The void's target is the first create, so it applies and does not wait.
	batch[2] = b.eventWithID(batch[2].ID, syncstub.Voided, map[string]any{"target": batch[0].ID})

	first := b.push(dev, batch...)
	if got := statuses(first); fmt.Sprint(got) != "[accepted accepted accepted rejected]" {
		t.Fatalf("first push: %v (%v)", got, codes(first))
	}
	state, inbox := b.records(), b.inbox()
	if len(state) != 2 || !state[batch[0].ID].Voided || state[batch[1].ID].Voided {
		t.Fatalf("state after the first push: %+v", state)
	}

	var prev syncsrv.PushResponse
	for n := 2; n <= 5; n++ {
		res := b.push(dev, batch...)
		if got := statuses(res); fmt.Sprint(got) != "[duplicate duplicate duplicate rejected]" {
			t.Fatalf("push %d: %v", n, got)
		}
		// A rejected event stays rejected, with the same reason.
		if res.Results[3].Code != "stub_rejected" {
			t.Errorf("push %d: rejected event came back as %+v", n, res.Results[3])
		}
		if n > 2 && fmt.Sprint(res.Results) != fmt.Sprint(prev.Results) {
			t.Errorf("push %d answered differently from push %d:\n%+v\n%+v", n, n-1, res.Results, prev.Results)
		}
		prev = res
		if got := b.records(); fmt.Sprint(got) != fmt.Sprint(state) {
			t.Fatalf("push %d changed the records: %+v", n, got)
		}
		if got := b.inbox(); fmt.Sprint(got) != fmt.Sprint(inbox) {
			t.Fatalf("push %d changed the inbox: %+v", n, got)
		}
	}
	w.settled()
}

func TestConcurrentPushesOfOneBatchApplyEachEventOnce(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	var batch []syncsrv.Event
	for i := 0; i < 20; i++ {
		batch = append(batch, b.created(fmt.Sprint("e", i)))
	}

	const pushers = 8
	var wg gosync.WaitGroup
	results := make([]syncsrv.PushResponse, pushers)
	errs := make([]error, pushers)
	for i := 0; i < pushers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: batch})
		}()
	}
	wg.Wait()

	accepted := make([]int, len(batch))
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("pusher %d: %v", i, errs[i])
		}
		for j, r := range res.Results {
			switch r.Status {
			case syncsrv.StatusAccepted:
				accepted[j]++
			case syncsrv.StatusDuplicate:
			default:
				t.Errorf("pusher %d event %d: %+v", i, j, r)
			}
		}
	}
	for j, n := range accepted {
		if n != 1 {
			t.Errorf("event %d was accepted %d times, want exactly once", j, n)
		}
	}
	if got := len(b.records()); got != len(batch) {
		t.Errorf("%d records, want %d", got, len(batch))
	}
	w.settled()
}

// ---- partial failure ----

func TestAFailedEventDoesNotBlockTheRestAndRetryLosesNothing(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	e1, e3 := b.created("one"), b.created("three")
	crash := b.event(syncstub.Crashes, map[string]any{"times": 1, "label": "two"})
	writeThenCrash := b.event(syncstub.WriteThenCrash, map[string]any{"times": 1, "label": "four"})
	batch := []syncsrv.Event{e1, crash, e3, writeThenCrash}

	first := b.push(dev, batch...)
	if got := statuses(first); fmt.Sprint(got) != "[accepted retry accepted retry]" {
		t.Fatalf("first push: %v (%v)", got, codes(first))
	}
	if first.Results[1].Code != "internal" {
		t.Errorf("a server fault should say so: %+v", first.Results[1])
	}
	// The event that wrote and then failed left nothing behind, and neither failed event is recorded.
	if st := b.records(); len(st) != 2 {
		t.Fatalf("records after the first push: %+v", st)
	}
	if in := b.inbox(); len(in) != 2 {
		t.Fatalf("inbox after the first push: %+v", in)
	}

	// The device resends the whole batch.
	second := b.push(dev, batch...)
	if got := statuses(second); fmt.Sprint(got) != "[duplicate accepted duplicate accepted]" {
		t.Fatalf("second push: %v (%v)", got, codes(second))
	}
	st := b.records()
	if len(st) != 4 || st[crash.ID].Label != "two" || st[writeThenCrash.ID].Label != "four" {
		t.Errorf("records after the retry: %+v", st)
	}
	for id, row := range b.inbox() {
		if row.Status != "accepted" {
			t.Errorf("event %s is %+v", id, row)
		}
	}
	w.settled()
}

func TestTheProcessDyingMidBatchLosesAndDuplicatesNothing(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	var batch []syncsrv.Event
	for i := 0; i < 10; i++ {
		batch = append(batch, b.created(fmt.Sprint("e", i)))
	}

	// Kill the request while the fourth event is being projected.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.stub.OnProject = func(ev syncsrv.Event) {
		if ev.ID == batch[3].ID {
			cancel()
		}
	}
	if _, err := w.svc.Push(ctx, dev, syncsrv.PushRequest{Events: batch}); err == nil {
		t.Fatal("a push whose request was cancelled should not report success")
	}
	w.stub.OnProject = nil
	if got := len(b.records()); got < 3 || got > 4 {
		t.Fatalf("%d events were applied before the crash, expected 3 or 4", got)
	}

	res := b.push(dev, batch...)
	for i, r := range res.Results {
		if r.Status != syncsrv.StatusAccepted && r.Status != syncsrv.StatusDuplicate {
			t.Errorf("event %d after the restart: %+v", i, r)
		}
	}
	if got := len(b.records()); got != 10 {
		t.Errorf("%d records, want 10", got)
	}
	// And a further resend is all duplicates.
	for i, r := range b.push(dev, batch...).Results {
		if r.Status != syncsrv.StatusDuplicate {
			t.Errorf("event %d on the final resend: %+v", i, r)
		}
	}
	w.settled()
}

// ---- reordering ----

// Whatever order a valid stream arrives in, and however it is cut into pushes, the same state
// results: the sale before its void, the void before its sale, interleaved with unrelated events.
func TestEveryOrderOfAStreamConvergesToTheSameState(t *testing.T) {
	const creates, voids = 12, 7
	ids := make([]uuid.UUID, creates)
	for i := range ids {
		ids[i] = kernel.NewID()
	}
	voidIDs := make([]uuid.UUID, voids)
	for i := range voidIDs {
		voidIDs[i] = kernel.NewID()
	}

	w := newWorld(t)
	rng := rand.New(rand.NewSource(7))
	var want map[uuid.UUID]record

	for run := 0; run < 25; run++ {
		b := w.business(fmt.Sprintf("kopi%d", run), fmt.Sprintf("K%03d", run))
		dev := b.device("Kasir 1")
		var stream []syncsrv.Event
		for i, id := range ids {
			stream = append(stream, b.eventWithID(id, syncstub.Created, map[string]any{"label": fmt.Sprint("sale", i)}))
		}
		for i, id := range voidIDs {
			stream = append(stream, b.eventWithID(id, syncstub.Voided, map[string]any{"target": ids[i]}))
		}
		if run > 0 { // run 0 keeps the natural order, the rest are shuffled
			rng.Shuffle(len(stream), func(i, j int) { stream[i], stream[j] = stream[j], stream[i] })
		}
		for len(stream) > 0 {
			n := 1 + rng.Intn(5)
			if n > len(stream) {
				n = len(stream)
			}
			for i, r := range b.push(dev, stream[:n]...).Results {
				if r.Status != syncsrv.StatusAccepted {
					t.Fatalf("run %d: event %v: %+v", run, stream[i].Type, r)
				}
			}
			stream = stream[n:]
		}

		got := b.records()
		if run == 0 {
			want = got
			if len(want) != creates {
				t.Fatalf("natural order produced %d records", len(want))
			}
			for i, id := range ids {
				if want[id].Voided != (i < voids) {
					t.Fatalf("record %d voided = %v", i, want[id].Voided)
				}
			}
		} else if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d did not converge:\n got %+v\nwant %+v", run, got, want)
		}
		for id, row := range b.inbox() {
			if row.Status != "accepted" {
				t.Fatalf("run %d: event %s ended %+v; nothing may stay parked once its sale has arrived", run, id, row)
			}
		}
	}
	w.settled()
}

func TestAVoidAheadOfItsSaleWaitsAndRunsWhenTheSaleArrives(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	d1, d2 := b.device("Kasir 1"), b.device("Kasir 2")
	sale := b.created("late sale")
	void := b.event(syncstub.Voided, map[string]any{"target": sale.ID})

	res := b.push(d2, void)
	if r := res.Results[0]; r.Status != syncsrv.StatusAccepted || r.Code != "pending_dependency" {
		t.Fatalf("early void: %+v", r)
	}
	if row := b.inbox()[void.ID]; row.Status != "pending_dependency" {
		t.Fatalf("early void is %+v", row)
	}
	// Pushing it again while it waits is a duplicate that says it is still waiting.
	if r := b.push(d2, void).Results[0]; r.Status != syncsrv.StatusDuplicate || r.Code != "pending_dependency" {
		t.Errorf("resent parked void: %+v", r)
	}

	// The sale comes from another device; the void is released inside the same transaction.
	if r := b.push(d1, sale).Results[0]; r.Status != syncsrv.StatusAccepted || r.Code != "" {
		t.Fatalf("sale: %+v", r)
	}
	if rec := b.records()[sale.ID]; !rec.Voided {
		t.Errorf("the void did not apply when its sale arrived: %+v", rec)
	}
	if row := b.inbox()[void.ID]; row.Status != "accepted" {
		t.Errorf("released void is %+v", row)
	}
	if r := b.push(d2, void).Results[0]; r.Status != syncsrv.StatusDuplicate || r.Code != "" {
		t.Errorf("resent released void: %+v", r)
	}
	w.settled()
}

func TestAVoidForASaleThatNeverArrivesStaysParkedAndHarmless(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ghost := kernel.NewID()
	b.push(dev, b.event(syncstub.Voided, map[string]any{"target": ghost}), b.created("other"))
	if len(b.records()) != 1 {
		t.Errorf("records: %+v", b.records())
	}
	parked := 0
	for _, row := range b.inbox() {
		if row.Status == "pending_dependency" {
			parked++
		}
	}
	if parked != 1 {
		t.Errorf("%d parked events, want 1", parked)
	}
}

// ---- concurrency ----

func TestTwoDevicesPushingAtOnceToOneOutlet(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	d1, d2 := b.device("Kasir 1"), b.device("Kasir 2")

	stream := func(prefix string) []syncsrv.Event {
		var out []syncsrv.Event
		for i := 0; i < 60; i++ {
			out = append(out, b.created(fmt.Sprint(prefix, i)))
		}
		return out
	}
	s1, s2 := stream("a"), stream("b")

	pushAll := func(dev identity.Principal, events []syncsrv.Event, errc chan<- error) {
		for len(events) > 0 {
			n := 10
			if n > len(events) {
				n = len(events)
			}
			res, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: events[:n]})
			if err != nil {
				errc <- err
				return
			}
			for _, r := range res.Results {
				if r.Status == syncsrv.StatusRetry || r.Status == syncsrv.StatusRejected {
					errc <- fmt.Errorf("unexpected result %+v", r)
					return
				}
			}
			events = events[n:]
		}
		errc <- nil
	}
	errc := make(chan error, 3)
	go pushAll(d1, s1, errc)
	go pushAll(d2, s2, errc)
	go pushAll(d1, s1, errc) // a retry storm from the first device, overlapping its own first run
	for i := 0; i < 3; i++ {
		if err := <-errc; err != nil {
			t.Error(err)
		}
	}
	if got := len(b.records()); got != 120 {
		t.Errorf("%d records, want 120", got)
	}
	for id, row := range b.inbox() {
		if row.Status != "accepted" {
			t.Errorf("%s: %+v", id, row)
		}
	}
	w.settled()
}

// ---- clocks ----

func TestClockSkewIsRecordedAndDeviceTimeKept(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")

	// The device's clock is three hours behind the server's, and its events say so: one from five
	// hours before the server's now, one from two hours ahead of it.
	behind, ahead := b.created("behind"), b.created("ahead")
	behind.DeviceTime, ahead.DeviceTime = t0.Add(-5*time.Hour), t0.Add(2*time.Hour)
	clientNow := t0.Add(-3 * time.Hour)
	ver := "1.4.2"
	res, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: []syncsrv.Event{behind, ahead}, Health: syncsrv.DeviceHealth{ClientTime: &clientNow, AppVersion: &ver}})
	if err != nil {
		t.Fatal(err)
	}
	if got := statuses(res); fmt.Sprint(got) != "[accepted accepted]" {
		t.Fatalf("results: %v", got)
	}

	// Each event keeps the time the device gave and the time the server received it: the server
	// never rewrites history from its own clock.
	for _, c := range []struct {
		ev   syncsrv.Event
		want time.Time
	}{{behind, t0.Add(-5 * time.Hour)}, {ahead, t0.Add(2 * time.Hour)}} {
		var deviceTime, received time.Time
		if err := w.d.Owner.QueryRow(context.Background(),
			`SELECT device_time, received_at FROM sync_inbox WHERE id = $1`, c.ev.ID).Scan(&deviceTime, &received); err != nil {
			t.Fatal(err)
		}
		if !deviceTime.Equal(c.want) || !received.Equal(t0) {
			t.Errorf("device_time %v, received_at %v; want %v and %v", deviceTime, received, c.want, t0)
		}
	}

	// The skew is server minus device: positive means the device is behind.
	var skew int64
	var version string
	var syncedAt time.Time
	if err := w.d.Owner.QueryRow(context.Background(),
		`SELECT clock_skew_ms, app_version, last_sync_at FROM device WHERE id = $1`, dev.DeviceID).Scan(&skew, &version, &syncedAt); err != nil {
		t.Fatal(err)
	}
	if skew != (3*time.Hour).Milliseconds() || version != "1.4.2" || !syncedAt.Equal(t0) {
		t.Errorf("device row: skew %d ms, version %q, last_sync_at %v", skew, version, syncedAt)
	}
}

// ---- revocation ----

func TestARevokedDeviceIsRefusedOutright(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	if err := w.ids.RevokeDevice(context.Background(), b.owner, dev.DeviceID); err != nil {
		t.Fatal(err)
	}
	_, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: []syncsrv.Event{b.created("x")}})
	if !errors.Is(err, identity.ErrDeviceRevoked) {
		t.Fatalf("err = %v, want ErrDeviceRevoked", err)
	}
	if len(b.records()) != 0 || len(b.inbox()) != 0 {
		t.Error("a revoked device got something accepted")
	}
}

func TestADeviceRevokedMidBatchHasTheRestRefused(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	e1 := b.created("before")
	revoke := b.event(syncstub.CreatedThenRevoke, map[string]any{"label": "last one", "device": dev.DeviceID, "by": b.owner.Principal.UserID})
	e3, e4 := b.created("after 1"), b.created("after 2")

	res := b.push(dev, e1, revoke, e3, e4)
	if got := statuses(res); fmt.Sprint(got) != "[accepted accepted rejected rejected]" {
		t.Fatalf("results: %v (%v)", got, codes(res))
	}
	if c := codes(res); c[2] != "device_revoked" || c[3] != "device_revoked" {
		t.Errorf("codes: %v", c)
	}
	// What was accepted before the revocation stays; what came after is neither applied nor kept.
	st := b.records()
	if len(st) != 2 || st[e1.ID].Label != "before" || st[revoke.ID].Label != "last one" {
		t.Errorf("records: %+v", st)
	}
	if got := len(b.inbox()); got != 2 {
		t.Errorf("%d inbox rows, want 2", got)
	}
	if _, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: []syncsrv.Event{e3}}); !errors.Is(err, identity.ErrDeviceRevoked) {
		t.Errorf("the next push: %v", err)
	}
	w.settled()
}

// Revoking waits for events in flight: nothing commits after the revocation has returned, and the
// event being applied is not half done.
func TestRevocationWaitsForAnEventInFlight(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	slow := b.event(syncstub.Slow, map[string]any{"ms": 500, "label": "slow"})

	started := make(chan struct{})
	w.stub.OnProject = func(ev syncsrv.Event) {
		if ev.ID == slow.ID {
			close(started)
		}
	}
	done := make(chan syncsrv.PushResponse, 1)
	go func() {
		res, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: []syncsrv.Event{slow}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	<-started
	begin := time.Now()
	if err := w.ids.RevokeDevice(context.Background(), b.owner, dev.DeviceID); err != nil {
		t.Fatal(err)
	}
	waited := time.Since(begin)
	res := <-done
	if res.Results[0].Status != syncsrv.StatusAccepted {
		t.Errorf("the event in flight: %+v", res.Results[0])
	}
	if waited < 300*time.Millisecond {
		t.Errorf("RevokeDevice returned after %v while an event was still being applied", waited)
	}
	if len(b.records()) != 1 {
		t.Errorf("records: %+v", b.records())
	}
}

// ---- tenants ----

func TestADeviceCannotReachAnotherBusiness(t *testing.T) {
	w := newWorld(t)
	a, bz := w.business("kopi", "JKT1"), w.business("teh", "BDG1")
	devA, devB := a.device("Kasir A"), bz.device("Kasir B")

	secret := bz.created("B's sale")
	bz.push(devB, secret)
	beforeB, beforeInboxB := bz.records(), bz.inbox()

	// A's tablet names B's cashier, tries to void B's sale, to write a row for B, and to reuse one
	// of B's event ids.
	asB := a.created("by B's cashier")
	asB.StaffID = bz.staff
	voidB := a.event(syncstub.Voided, map[string]any{"target": secret.ID})
	forB := a.event(syncstub.Reference, map[string]any{"tenant_of": bz.tenant.ID})
	reuse := a.eventWithID(secret.ID, syncstub.Created, map[string]any{"label": "A's own, same id"})
	wrongOutlet := a.event(syncstub.Created, map[string]any{"label": "x", "outlet_id": bz.outlet.ID})

	res := a.push(devA, asB, voidB, forB, reuse, wrongOutlet)
	if got := codes(res); fmt.Sprint(got) != "[unknown_staff pending_dependency unknown_reference  wrong_outlet]" {
		t.Fatalf("codes: %q (%v)", got, statuses(res))
	}
	if got := statuses(res); fmt.Sprint(got) != "[rejected accepted rejected accepted rejected]" {
		t.Fatalf("statuses: %v", got)
	}

	// B is exactly as it was, and A holds only A's own.
	if fmt.Sprint(bz.records()) != fmt.Sprint(beforeB) || fmt.Sprint(bz.inbox()) != fmt.Sprint(beforeInboxB) {
		t.Errorf("tenant B changed:\n%+v\n%+v", bz.records(), bz.inbox())
	}
	if r := bz.records()[secret.ID]; r.Voided || r.Label != "B's sale" {
		t.Errorf("B's sale: %+v", r)
	}
	if r := a.records()[secret.ID]; r.Label != "A's own, same id" {
		t.Errorf("an event id is only unique within a business: %+v", r)
	}
	if _, ok := a.records()[forB.ID]; ok {
		t.Error("the cross-tenant write left a row")
	}
	w.settled()
}

func TestAnEventIdCannotBeUsedByTwoDevices(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	d1, d2 := b.device("Kasir 1"), b.device("Kasir 2")
	ev := b.created("mine")
	b.push(d1, ev)
	other := b.eventWithID(ev.ID, syncstub.Created, map[string]any{"label": "theirs"})
	res := b.push(d2, other)
	if r := res.Results[0]; r.Status != syncsrv.StatusRejected || r.Code != "id_conflict" {
		t.Errorf("second device: %+v", r)
	}
	if got := b.records()[ev.ID].Label; got != "mine" {
		t.Errorf("label = %q", got)
	}
}

// ---- idempotency ----

func TestTheSameKeyWithDifferentContentIsRejected(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	orig := b.created("original")
	b.push(dev, orig)

	changed := orig
	changed.Payload = json.RawMessage(`{"label":"tampered"}`)
	if r := b.push(dev, changed).Results[0]; r.Status != syncsrv.StatusRejected || r.Code != "idempotency_conflict" {
		t.Errorf("changed payload: %+v", r)
	}
	otherStaff := orig
	otherStaff.StaffID = kernel.NewID()
	if r := b.push(dev, otherStaff).Results[0]; r.Code != "idempotency_conflict" {
		t.Errorf("changed staff: %+v", r)
	}
	otherTime := orig
	otherTime.DeviceTime = t0.Add(time.Minute)
	if r := b.push(dev, otherTime).Results[0]; r.Code != "idempotency_conflict" {
		t.Errorf("changed device time: %+v", r)
	}
	if got := b.records()[orig.ID].Label; got != "original" {
		t.Errorf("a conflicting resend changed the record: %q", got)
	}
	// The original is still answered as a duplicate: the conflict did not disturb it.
	if r := b.push(dev, orig).Results[0]; r.Status != syncsrv.StatusDuplicate {
		t.Errorf("original after the conflict: %+v", r)
	}

	// The way the JSON is written is not content: key order and spacing do not matter.
	spaced := orig
	spaced.Payload = json.RawMessage("{ \"label\" :  \"original\" }")
	if r := b.push(dev, spaced).Results[0]; r.Status != syncsrv.StatusDuplicate {
		t.Errorf("same content, different spelling: %+v", r)
	}
}

// ---- what is rejected, and what is not ----

func TestOnlyMalformedEventsAreRejected(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")

	badType := b.event("test.nonexistent", map[string]any{})
	newer := b.created("v3")
	newer.SchemaVersion = 3
	zeroVersion := b.created("v0")
	zeroVersion.SchemaVersion = 0
	notObject := b.created("x")
	notObject.Payload = json.RawMessage(`[1,2]`)
	garbage := b.created("x")
	garbage.Payload = json.RawMessage(`{"label": `)
	noStaff := b.created("x")
	noStaff.StaffID = uuid.Nil
	noTime := b.created("x")
	noTime.DeviceTime = time.Time{}
	shout := b.event("Sale.Completed", map[string]any{})
	good := b.created("fine")

	events := []syncsrv.Event{badType, newer, zeroVersion, notObject, garbage, noStaff, noTime, shout, good}
	res := b.push(dev, events...)
	wantCodes := []string{"unknown_type", "unsupported_schema_version", "malformed", "malformed", "malformed", "malformed", "malformed", "malformed", ""}
	if got := codes(res); fmt.Sprint(got) != fmt.Sprint(wantCodes) {
		t.Errorf("codes:\n got %q\nwant %q", got, wantCodes)
	}
	for i, st := range statuses(res)[:8] {
		if st != syncsrv.StatusRejected {
			t.Errorf("event %d: %v", i, st)
		}
	}
	if st := res.Results[8].Status; st != syncsrv.StatusAccepted {
		t.Errorf("the good event after the bad ones: %v", st)
	}
	if got := len(b.records()); got != 1 {
		t.Errorf("%d records, want only the good one", got)
	}
	// A rejection the server could store is kept, so a resend gets the same answer and support can
	// look at it.
	if row := b.inbox()[badType.ID]; row.Status != "rejected" || row.Code != "unknown_type" {
		t.Errorf("stored rejection: %+v", row)
	}
	if r := b.push(dev, badType).Results[0]; r.Status != syncsrv.StatusRejected || r.Code != "unknown_type" {
		t.Errorf("resent rejection: %+v", r)
	}
	w.settled()
}

func TestARejectedEventLeavesNoWritesBehind(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ev := b.event(syncstub.WriteThenReject, map[string]any{"label": "ghost"})
	if r := b.push(dev, ev).Results[0]; r.Status != syncsrv.StatusRejected || r.Code != "stub_rejected" {
		t.Fatalf("result: %+v", r)
	}
	if len(b.records()) != 0 {
		t.Errorf("a rejected event wrote %+v", b.records())
	}
	if row := b.inbox()[ev.ID]; row.Status != "rejected" {
		t.Errorf("inbox: %+v", row)
	}
}

func TestAnUnknownStaffEventIsRejectedButAnInactiveOneIsAccepted(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")

	// A cashier deactivated since the sale was rung up: the money moved, so it is accepted (a
	// projector will flag it), not rejected.
	if _, err := w.ids.UpdateStaff(context.Background(), b.owner, b.staff, identity.UpdateStaff{Active: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if r := b.push(dev, b.created("late")).Results[0]; r.Status != syncsrv.StatusAccepted {
		t.Errorf("inactive staff: %+v", r)
	}
	stranger := b.created("who")
	stranger.StaffID = kernel.NewID()
	if r := b.push(dev, stranger).Results[0]; r.Code != "unknown_staff" {
		t.Errorf("unknown staff: %+v", r)
	}
}

func ptr[T any](v T) *T { return &v }

// ---- limits and wiring ----

func TestPushSizeLimits(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	if _, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("empty push: %v", err)
	}
	many := make([]syncsrv.Event, syncsrv.MaxEvents+1)
	for i := range many {
		many[i] = b.created("x")
	}
	if _, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: many}); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("%d events: %v", len(many), err)
	}
	if len(b.inbox()) != 0 {
		t.Error("an oversized push was partly accepted")
	}
	full := many[:syncsrv.MaxEvents]
	res, err := w.svc.Push(context.Background(), dev, syncsrv.PushRequest{Events: full})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.Results {
		if r.Status != syncsrv.StatusAccepted {
			t.Fatalf("event %d of a full push: %+v", i, r)
		}
	}
}

func TestTwoProjectorsCannotClaimOneType(t *testing.T) {
	w := newWorld(t)
	_, err := syncsrv.NewService(syncsrv.Deps{Pool: w.d.App, Identity: w.ids, Projectors: []syncsrv.Projector{&syncstub.Projector{}, &syncstub.Projector{}}})
	if err == nil {
		t.Error("two projectors for the same event types were accepted")
	}
}

// ---- the inbox itself ----

func TestTheInboxCannotBeRewritten(t *testing.T) {
	w := newWorld(t)
	b := w.business("kopi", "JKT1")
	dev := b.device("Kasir 1")
	ev := b.created("x")
	b.push(dev, ev)

	ctx := context.Background()
	exec := func(sql string, args ...any) error {
		return kernel.TenantTx(ctx, w.d.App, b.tenant.ID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	for name, sql := range map[string]string{
		"change the payload": `UPDATE sync_inbox SET payload = '{}'`,
		"change the type":    `UPDATE sync_inbox SET event_type = 'test.other'`,
		"delete":             `DELETE FROM sync_inbox`,
	} {
		if err := exec(sql); err == nil {
			t.Errorf("%s: the app role was allowed to", name)
		}
	}
	// A settled outcome cannot change either, even in the columns the role may write.
	if err := exec(`UPDATE sync_inbox SET status = 'rejected', code = 'x'`); err == nil {
		t.Error("the outcome of an accepted event was changed")
	}
	if b.inbox()[ev.ID].Status != "accepted" {
		t.Error("the accepted event changed")
	}
}
