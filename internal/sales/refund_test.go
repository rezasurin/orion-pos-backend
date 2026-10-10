package sales_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rezasurin/orion-pos-backend/internal/sales"
	syncsrv "github.com/rezasurin/orion-pos-backend/internal/sync"
)

func refundPayload(sale, shift uuid.UUID, method string, lines ...[3]int64) map[string]any {
	ls := make([]map[string]any, len(lines))
	for i, l := range lines {
		ls[i] = map[string]any{"line_no": l[0], "quantity": l[1], "amount": l[2]}
	}
	return map[string]any{"sale_id": sale, "shift_id": shift, "method": method, "reason": "cold coffee", "lines": ls}
}

// A refund gives back part of a sale on the day it happens. Each refund is recorded as the tablet
// sent it; going beyond what was sold or paid, or doing it without sale.refund, is flagged.
func TestRefundsAreRecordedAndCheckedAgainstTheSale(t *testing.T) {
	f := newFx(t)
	ctx := context.Background()
	shift := f.openShift(f.dev, 0)
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(),
		Lines: []lineSpec{f.espressoLine(2), f.latte(1)}}), f.t0.Add(time.Minute))
	if r := f.one(f.dev, sale); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("sale: %+v", r)
	}
	paid := int64(f.lastRes.CashTotal)

	// A manager refunds one espresso in cash: no flag.
	first := f.event(f.manager, sales.TypeRefundIssued, refundPayload(sale.ID, shift, "cash", [3]int64{0, 1, 22000}), f.t0.Add(2*time.Minute))
	if r := f.one(f.dev, first); r.Status != syncsrv.StatusAccepted || r.Code != "" || len(f.flags(first.ID)) != 0 {
		t.Fatalf("refund: %+v, flags %v", r, f.flags(first.ID))
	}
	var amount int64
	var method string
	var day time.Time
	if err := f.d.Owner.QueryRow(ctx, `SELECT amount, method, business_date FROM refund WHERE id = $1`, first.ID).Scan(&amount, &method, &day); err != nil {
		t.Fatal(err)
	}
	if amount != 22000 || method != "cash" || day.IsZero() {
		t.Errorf("refund row: %d %s %v", amount, method, day)
	}
	var status string
	_ = f.d.Owner.QueryRow(ctx, `SELECT status FROM sale WHERE id = $1`, sale.ID).Scan(&status)
	if status != "completed" {
		t.Errorf("a refunded sale is %s, want completed", status)
	}

	// A cashier refunds two more espressos (one too many) and more money than is left, alone.
	second := f.event(f.cashier, sales.TypeRefundIssued, refundPayload(sale.ID, shift, "card_manual", [3]int64{0, 2, paid}, [3]int64{1, 1, 1}), f.t0.Add(3*time.Minute))
	if r := f.one(f.dev, second); r.Status != syncsrv.StatusAccepted {
		t.Fatalf("second refund: %+v", r)
	}
	if got := fmt.Sprint(f.flags(second.ID)); got != "[permission_missing refund_over_paid refund_over_quantity]" {
		t.Errorf("flags = %s", got)
	}
	// With a manager's approval the permission is there.
	approved := refundPayload(sale.ID, shift, "cash", [3]int64{1, 1, 0})
	approved["approved_by"] = f.manager
	third := f.event(f.cashier, sales.TypeRefundIssued, approved, f.t0.Add(4*time.Minute))
	f.one(f.dev, third)
	if got := fmt.Sprint(f.flags(third.ID)); got != "[refund_over_quantity]" {
		t.Errorf("approved refund flags = %s", got)
	}

	// A refunded sale cannot be voided, and a voided one cannot be refunded.
	void := f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": sale.ID, "reason": "x"}, f.t0.Add(5*time.Minute))
	if r := f.one(f.dev, void); r.Status != syncsrv.StatusRejected || r.Code != "already_refunded" {
		t.Errorf("void after refund: %+v", r)
	}
	other := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(6*time.Minute))
	f.one(f.dev, other)
	f.one(f.dev, f.event(f.manager, sales.TypeSaleVoided, map[string]any{"sale_id": other.ID, "reason": "x"}, f.t0.Add(7*time.Minute)))
	if r := f.one(f.dev, f.event(f.manager, sales.TypeRefundIssued, refundPayload(other.ID, shift, "cash", [3]int64{0, 1, 1}), f.t0.Add(8*time.Minute))); r.Code != "already_voided" {
		t.Errorf("refund after void: %+v", r)
	}
}

func TestMalformedRefundsAreRejected(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	f.one(f.dev, sale)
	for name, p := range map[string]map[string]any{
		"unknown line": refundPayload(sale.ID, shift, "cash", [3]int64{5, 1, 1}),
		"line twice":   refundPayload(sale.ID, shift, "cash", [3]int64{0, 1, 1}, [3]int64{0, 1, 1}),
		"no lines":     refundPayload(sale.ID, shift, "cash"),
		"bad method":   refundPayload(sale.ID, shift, "barter", [3]int64{0, 1, 1}),
		"zero units":   refundPayload(sale.ID, shift, "cash", [3]int64{0, 0, 1}),
		"no shift":     refundPayload(sale.ID, uuid.Nil, "cash", [3]int64{0, 1, 1}),
	} {
		if r := f.one(f.dev, f.event(f.manager, sales.TypeRefundIssued, p, f.t0.Add(2*time.Minute))); r.Status != syncsrv.StatusRejected || r.Code != "invalid_payload" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if n := f.count("refund"); n != 0 {
		t.Errorf("%d refunds recorded", n)
	}
}

// A refund that arrives before its sale waits for it, and is applied when the sale comes.
func TestARefundWaitsForItsSale(t *testing.T) {
	f := newFx(t)
	shift := f.openShift(f.dev, 0)
	sale := f.event(f.cashier, sales.TypeSaleCompleted, f.salePayload(saleSpec{Shift: shift, CatalogSeq: f.seq(), Lines: []lineSpec{f.espressoLine(1)}}), f.t0.Add(time.Minute))
	refund := f.event(f.manager, sales.TypeRefundIssued, refundPayload(sale.ID, shift, "cash", [3]int64{0, 1, 5000}), f.t0.Add(2*time.Minute))
	if r := f.one(f.dev, refund); r.Code != "pending_dependency" {
		t.Fatalf("refund first: %+v", r)
	}
	f.one(f.dev, sale)
	if s := f.inboxStatus(refund.ID); s != "accepted" || f.count("refund") != 1 {
		t.Errorf("after the sale arrived the refund is %s (%d refunds)", s, f.count("refund"))
	}
}
