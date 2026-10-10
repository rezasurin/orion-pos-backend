package sales_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rezasurin/orion-pos-backend/internal/entitlements"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/platform"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func newPlatformFor(t *testing.T, f *fx) *platform.Service {
	t.Helper()
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := kernel.NewBox(base64.RawURLEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := platform.NewService(platform.Deps{
		Pool: f.d.Platform, Box: box, Keys: identity.NewEphemeralKeyring(),
		Entitlements: entitlements.NewAdmin(f.d.Platform), Tenants: tenancy.NewAdmin(f.d.Platform),
		PasswordCost: identity.ArgonParams{Time: 1, Memory: 8 * 1024, Threads: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestTheSupportReportShowsWhatIsWrongWithABusiness(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	plat := newPlatformFor(t, f)
	shift := f.openShift(f.dev, 0)

	// A flagged sale (a kitchen account rang it up), a void for a sale that never arrived, an
	// event of a type nobody knows, and a device saying events are stuck.
	flagged := f.event(f.kitchen, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	f.one(f.dev, flagged)
	ghost := kernel.NewID()
	parked := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": ghost, "reason": "x"}, f.t0.Add(time.Minute))
	f.one(f.dev, parked)
	f.one(f.dev, f.event(f.cashier, "pos.mystery", map[string]any{}, f.t0))
	old := time.Now().UTC().Add(-time.Hour)
	if _, err := f.svc.Push(ctx, f.dev, syncsrv.PushRequest{
		Events: []syncsrv.Event{f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 0}, f.t0)},
		Health: syncsrv.DeviceHealth{Unsynced: ip(5), OldestUnsynced: &old},
	}); err != nil {
		t.Fatal(err)
	}

	r, err := plat.SupportReport(ctx, platform.Actor{}, "kopi", "ticket 123: sales missing")
	if err != nil {
		t.Fatal(err)
	}
	if r.TenantID != f.tenant.ID || r.Suspended || len(r.Devices) != 1 {
		t.Fatalf("report: %+v", r)
	}
	d := r.Devices[0]
	if d.OutletCode != "JKT1" || d.Code != 1 || d.Name != "Kasir 1" || d.UnsyncedEvents != 5 || d.OldestUnsyncedAt == nil || d.LastSyncAt == nil || d.Revoked {
		t.Errorf("device: %+v", d)
	}
	if len(r.Parked) != 1 || r.Parked[0].ID != parked.ID || r.Parked[0].Type != "sale.voided" || r.Parked[0].DependsOn != ghost || r.Parked[0].Device != "Kasir 1" {
		t.Errorf("parked: %+v", r.Parked)
	}
	if fmt.Sprint(r.Rejected) != "[{unknown_type 1}]" {
		t.Errorf("rejected: %+v", r.Rejected)
	}
	if fmt.Sprint(r.Flags) != "[{permission_missing 1}]" {
		t.Errorf("flags: %+v", r.Flags)
	}
	if len(r.FlaggedSales) != 1 || r.FlaggedSales[0].ID != flagged.ID || fmt.Sprint(r.FlaggedSales[0].Codes) != "[permission_missing]" || r.FlaggedSales[0].OutletCode != "JKT1" {
		t.Errorf("flagged sales: %+v", r.FlaggedSales)
	}

	// Reading a business's data is an operator action: it is audited, with the reason.
	var action, reason string
	var tenant *string
	if err := f.d.Owner.QueryRow(ctx, `SELECT action, reason, tenant_id::text FROM platform_audit_log WHERE action = 'support.report_viewed'`).Scan(&action, &reason, &tenant); err != nil {
		t.Fatal(err)
	}
	if reason != "ticket 123: sales missing" || tenant == nil || *tenant != f.tenant.ID.String() {
		t.Errorf("audit: %s %q %v", action, reason, tenant)
	}
	if _, err := plat.SupportReport(ctx, platform.Actor{}, "kopi", "  "); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("no reason: %v", err)
	}
	if _, err := plat.SupportReport(ctx, platform.Actor{}, "nobody", "x"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("unknown business: %v", err)
	}
}

func TestAnOperatorCanAbandonAParkedEvent(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	plat := newPlatformFor(t, f)
	f.openShift(f.dev, 0)

	ghost := kernel.NewID()
	parked := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": ghost, "reason": "x"}, f.t0.Add(time.Minute))
	if r := f.one(f.dev, parked); r.Code != "pending_dependency" {
		t.Fatalf("parked: %+v", r)
	}
	// An event that was accepted cannot be abandoned.
	accepted := f.event(f.cashier, sales.TypeShiftOpened, map[string]any{"opening_cash": 1}, f.t0)
	f.one(f.dev, accepted)
	if err := plat.AbandonParkedEvent(ctx, platform.Actor{}, "kopi", accepted.ID, "x"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("an accepted event: %v", err)
	}
	if err := plat.AbandonParkedEvent(ctx, platform.Actor{}, "kopi", parked.ID, " "); !errors.Is(err, kernel.ErrValidation) {
		t.Errorf("no reason: %v", err)
	}

	if err := plat.AbandonParkedEvent(ctx, platform.Actor{}, "kopi", parked.ID, "the tablet that rang the sale was wiped"); err != nil {
		t.Fatal(err)
	}
	var status, code, detail string
	if err := f.d.Owner.QueryRow(ctx, `SELECT status, code, detail FROM sync_inbox WHERE id = $1`, parked.ID).Scan(&status, &code, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || code != "abandoned" || detail != "abandoned by an operator: the tablet that rang the sale was wiped" {
		t.Errorf("inbox: %s %s %q", status, code, detail)
	}
	// The device resending it is told so, and it is no longer parked.
	if r := f.one(f.dev, parked); r.Status != syncsrv.StatusRejected || r.Code != "abandoned" {
		t.Errorf("resend: %+v", r)
	}
	if err := plat.AbandonParkedEvent(ctx, platform.Actor{}, "kopi", parked.ID, "again"); !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("abandoning twice: %v", err)
	}
	var n int
	if err := f.d.Owner.QueryRow(ctx, `SELECT count(*) FROM platform_audit_log WHERE action = 'sync.event_abandoned'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("audit entries: %d (%v)", n, err)
	}
	// If the sale it waited for does turn up after all, the abandoned void stays abandoned.
	shift := f.openShift(f.dev, 0)
	late := f.eventWithID(ghost, f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	if r := f.one(f.dev, late); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("late sale: %+v", r)
	}
	if got := f.saleStatus(ghost); got != "completed" {
		t.Errorf("the abandoned void was applied anyway: sale is %s", got)
	}
}
