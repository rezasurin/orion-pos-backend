package sales_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rezasurin/orion-pos-backend/internal/notify"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
)

// The device health monitor (B1.10): owners are told, once per incident, when a tablet holds
// unsynced events for too long or goes quiet with a shift open, and incidents clear on their own.

type clockAt struct{ t atomic.Pointer[time.Time] }

func newClockAt(t time.Time) *clockAt      { c := &clockAt{}; c.set(t); return c }
func (c *clockAt) set(t time.Time)         { c.t.Store(&t) }
func (c *clockAt) advance(d time.Duration) { c.set(c.Now().Add(d)) }
func (c *clockAt) Now() time.Time          { return *c.t.Load() }

type monitor struct {
	f      *fx
	clock  *clockAt
	sender *notify.MemorySender
	jobs   *syncsrv.HealthJobs
}

func newMonitor(t *testing.T, f *fx, operator string) *monitor {
	t.Helper()
	m := &monitor{f: f, clock: newClockAt(time.Now().UTC()), sender: &notify.MemorySender{}}
	m.jobs = syncsrv.NewHealthJobs(syncsrv.HealthDeps{
		Platform: f.d.Platform, Sender: m.sender, Clock: m.clock, UnsyncedAfter: 30 * time.Minute, SilentAfter: 3 * time.Hour, OperatorEmail: operator,
	})
	return m
}

func (m *monitor) check() syncsrv.HealthReport {
	m.f.t.Helper()
	r, err := m.jobs.Check(context.Background())
	if err != nil {
		m.f.t.Fatal(err)
	}
	return r
}

// pushWithOutbox pushes one harmless event with the device's outbox report.
func (f *fx) pushWithOutbox(unsynced *int, oldest *time.Time) {
	f.t.Helper()
	ev := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, f.t0)
	_, err := f.svc.Push(context.Background(), f.dev, syncsrv.PushRequest{
		Events: []syncsrv.Event{ev}, Health: syncsrv.DeviceHealth{Unsynced: unsynced, OldestUnsynced: oldest},
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func ip(n int) *int { return &n }

func TestOwnersAreToldOnceWhenEventsStayUnsynced(t *testing.T) {
	f := newFx(t)
	m := newMonitor(t, f, "")

	// The device pushes and says three events are still waiting, the oldest 10 minutes old: too soon.
	old := m.clock.Now().Add(-10 * time.Minute)
	f.pushWithOutbox(ip(3), &old)
	if r := m.check(); r.Opened != 0 {
		t.Fatalf("10 minutes is not too long: %+v", r)
	}

	// Twenty-five minutes later the oldest is 35 minutes old.
	m.clock.advance(25 * time.Minute)
	r := m.check()
	if r.Opened != 1 || r.Notified != 1 {
		t.Fatalf("after 35 minutes: %+v", r)
	}
	sent := m.sender.Sent()
	if len(sent) != 1 || sent[0].To != "owner@kopi.test" || !strings.Contains(sent[0].Subject, "Kasir 1") || !strings.Contains(sent[0].Text, "3") {
		t.Fatalf("email: %+v", sent)
	}
	// The default language is Indonesian, and the text says what to do.
	if !strings.Contains(sent[0].Subject, "belum tersinkron") || !strings.Contains(sent[0].Text, "internet") {
		t.Errorf("email text: %q / %q", sent[0].Subject, sent[0].Text)
	}

	// Checking again, however often, does not email again.
	for i := 0; i < 3; i++ {
		m.clock.advance(5 * time.Minute)
		if r := m.check(); r.Opened != 0 || r.Notified != 0 || r.Resolved != 0 {
			t.Errorf("run %d: %+v", i, r)
		}
	}
	eq(t, "emails", len(m.sender.Sent()), 1)

	// The device delivers and says so: the incident clears, with nothing sent.
	f.pushWithOutbox(ip(0), nil)
	if r := m.check(); r.Resolved != 1 || r.Notified != 0 {
		t.Errorf("after delivering: %+v", r)
	}
	eq(t, "emails after resolving", len(m.sender.Sent()), 1)

	// A new incident later is a new email.
	later := m.clock.Now().Add(-time.Hour)
	f.pushWithOutbox(ip(1), &later)
	if r := m.check(); r.Opened != 1 || r.Notified != 1 {
		t.Errorf("a second incident: %+v", r)
	}
	eq(t, "emails after a second incident", len(m.sender.Sent()), 2)
}

func TestADeviceThatGoesQuietWithAShiftOpen(t *testing.T) {
	f := newFx(t)
	m := newMonitor(t, f, "ops@orion.test")
	shift := f.openShift(f.dev, 50000)
	ctx := context.Background()

	// Heard from a moment ago: fine.
	if r := m.check(); r.Opened != 0 {
		t.Fatalf("a device that just spoke: %+v", r)
	}
	// Five hours of silence with the shift still open.
	silent := m.clock.Now().Add(-5 * time.Hour)
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET last_seen_at = $2, last_sync_at = $2 WHERE id = $1`, f.dev.DeviceID, silent); err != nil {
		t.Fatal(err)
	}
	r := m.check()
	if r.Opened != 1 || r.Notified != 1 {
		t.Fatalf("five hours quiet: %+v", r)
	}
	sent := m.sender.Sent()
	// The owner, and a copy to the operator naming the business.
	if len(sent) != 2 || sent[0].To != "owner@kopi.test" || sent[1].To != "ops@orion.test" || !strings.Contains(sent[1].Text, f.tenant.ID.String()) {
		t.Fatalf("emails: %+v", sent)
	}
	if !strings.Contains(sent[0].Subject, "shift terbuka") {
		t.Errorf("subject: %q", sent[0].Subject)
	}

	// The tablet comes back: the incident clears.
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET last_seen_at = now(), last_sync_at = now() WHERE id = $1`, f.dev.DeviceID); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Resolved != 1 {
		t.Errorf("after the device came back: %+v", r)
	}

	// Silent again, but the shift was closed: nothing to worry about.
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET last_seen_at = $2, last_sync_at = $2 WHERE id = $1`, f.dev.DeviceID, silent); err != nil {
		t.Fatal(err)
	}
	f.one(f.dev, f.event(f.cashier, sales.TypeShiftClosed, map[string]any{"shift_id": shift, "counted_cash": 50000}, f.t0.Add(time.Minute)))
	// The close itself was a contact, so make the device silent again after it.
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET last_seen_at = $2, last_sync_at = $2 WHERE id = $1`, f.dev.DeviceID, silent); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Opened != 0 {
		t.Errorf("a quiet device with no open shift: %+v", r)
	}
}

func TestNobodyIsAlertedAboutDevicesThatShouldNotAlert(t *testing.T) {
	f := newFx(t)
	m := newMonitor(t, f, "")
	ctx := context.Background()
	old := m.clock.Now().Add(-3 * time.Hour)
	f.pushWithOutbox(ip(4), &old)

	// A revoked device.
	if err := f.ids.RevokeDevice(ctx, f.owner, f.dev.DeviceID); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Opened != 0 {
		t.Errorf("a revoked device: %+v", r)
	}

	// A device not seen for over a week (a tablet in a drawer), and a suspended business.
	d2 := f.device("Kasir 2")
	var id = d2.DeviceID
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET unsynced_events = 2, oldest_unsynced_at = $2, health_reported_at = $2, last_seen_at = $3 WHERE id = $1`,
		id, old, m.clock.Now().Add(-10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Opened != 0 {
		t.Errorf("a device unseen for ten days: %+v", r)
	}
	if _, err := f.d.Owner.Exec(ctx, `UPDATE device SET last_seen_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.Owner.Exec(ctx, `UPDATE tenant SET suspended_at = now() WHERE id = $1`, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Opened != 0 {
		t.Errorf("a suspended business: %+v", r)
	}
	if _, err := f.d.Owner.Exec(ctx, `UPDATE tenant SET suspended_at = NULL WHERE id = $1`, f.tenant.ID); err != nil {
		t.Fatal(err)
	}
	if r := m.check(); r.Opened != 1 {
		t.Errorf("an active device with old events: %+v", r)
	}
}

type flakySender struct {
	inner notify.MemorySender
	fails atomic.Int32
}

func (s *flakySender) Send(ctx context.Context, m notify.Message) error {
	if s.fails.Add(-1) >= 0 {
		return errors.New("the mail server is down")
	}
	return s.inner.Send(ctx, m)
}

func TestAFailedEmailIsRetriedOnTheNextRun(t *testing.T) {
	f := newFx(t)
	sender := &flakySender{}
	sender.fails.Store(1)
	clock := newClockAt(time.Now().UTC())
	jobs := syncsrv.NewHealthJobs(syncsrv.HealthDeps{Platform: f.d.Platform, Sender: sender, Clock: clock})
	old := clock.Now().Add(-2 * time.Hour)
	f.pushWithOutbox(ip(2), &old)

	r, err := jobs.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Opened != 1 || r.Notified != 0 || len(sender.inner.Sent()) != 0 {
		t.Fatalf("first run, mail server down: %+v", r)
	}
	r, err = jobs.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Opened != 0 || r.Notified != 1 || len(sender.inner.Sent()) != 1 {
		t.Fatalf("second run: %+v", r)
	}
	r, _ = jobs.Check(context.Background())
	if r.Notified != 0 || len(sender.inner.Sent()) != 1 {
		t.Errorf("third run sent again: %+v", r)
	}
}

func TestEnglishOwnersGetEnglishAndABusinessWithNoOwnerEmailIsNotRetriedForever(t *testing.T) {
	f := newFx(t)
	m := newMonitor(t, f, "")
	ctx := context.Background()
	old := m.clock.Now().Add(-2 * time.Hour)
	f.pushWithOutbox(ip(2), &old)

	if _, err := f.d.Owner.Exec(ctx, `UPDATE user_account SET locale = 'en'`); err != nil {
		t.Fatal(err)
	}
	m.check()
	sent := m.sender.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Subject, "have not synced") {
		t.Fatalf("english email: %+v", sent)
	}

	// Another incident, but nobody to tell (the owner's address is unverified): marked as handled.
	if _, err := f.d.Owner.Exec(ctx, `UPDATE user_account SET email_verified_at = NULL`); err != nil {
		t.Fatal(err)
	}
	f.pushWithOutbox(ip(0), nil)
	m.check() // resolves
	again := m.clock.Now().Add(-2 * time.Hour)
	f.pushWithOutbox(ip(1), &again)
	if r := m.check(); r.Opened != 1 || r.Notified != 0 {
		t.Errorf("nobody to tell: %+v", r)
	}
	if r := m.check(); r.Opened != 0 || r.Notified != 0 {
		t.Errorf("the next run: %+v", r)
	}
}

func TestADeviceReportsItsOutboxOnPushAndPull(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	oldest := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	f.pushWithOutbox(ip(7), &oldest)
	var n int
	var at, reported *time.Time
	read := func() {
		if err := f.d.Owner.QueryRow(ctx, `SELECT unsynced_events, oldest_unsynced_at, health_reported_at FROM device WHERE id = $1`, f.dev.DeviceID).Scan(&n, &at, &reported); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if n != 7 || at == nil || !at.Equal(oldest) || reported == nil {
		t.Fatalf("after a push: %d %v %v", n, at, reported)
	}

	// A pull reports it too, and a pull that says nothing leaves the last report alone.
	if _, err := f.svc.Pull(ctx, f.dev, syncsrv.PullRequest{Health: syncsrv.DeviceHealth{Unsynced: ip(0)}}); err != nil {
		t.Fatal(err)
	}
	read()
	if n != 0 || at != nil {
		t.Errorf("after a pull saying 0: %d %v", n, at)
	}
	f.pushWithOutbox(ip(2), &oldest)
	if _, err := f.svc.Pull(ctx, f.dev, syncsrv.PullRequest{}); err != nil {
		t.Fatal(err)
	}
	read()
	if n != 2 {
		t.Errorf("a pull with no report changed the count to %d", n)
	}

	// Nonsense is ignored, and the sync it came with still works.
	f.pushWithOutbox(ip(5), nil) // a count with no oldest
	read()
	if n != 2 {
		t.Errorf("an inconsistent report was taken: %d", n)
	}
	f.pushWithOutbox(ip(-1), &oldest)
	read()
	if n != 2 {
		t.Errorf("a negative count was taken: %d", n)
	}
}
