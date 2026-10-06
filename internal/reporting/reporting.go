// Package reporting answers "how did the day go": the end-of-shift report with its cash
// reconciliation, the end-of-day report of an outlet (BACKEND_PLAN.md section 6.4, task B1.8), and
// an outlet's sales over a range of days by day, item or payment method (B2.6).
//
// Reports are computed live from the rows the sales projectors wrote, in one read-only
// transaction, so they can be rebuilt at any time and there is no stored total to go stale. The
// numbers are the POS's own: a sale is stored with the amounts the device charged, so a day's total
// here equals the sum of the receipts.
//
// Two definitions matter and are stated in the results:
//
//   - Sales and their totals count completed sales by the business date they were rung up on. A void
//     recorded later removes the sale from that day, as on any POS, so a past day's report can
//     change after the fact. Voids are reported alongside.
//   - Expected cash is the opening cash, plus the cash applied to the bills of the shift's sales
//     (net of change, whatever became of the sale), minus cash refunded for voids made in the shift
//     (whichever shift sold the sale), plus pay ins, minus pay outs. A refund therefore leaves the
//     drawer of the shift it was paid from, and counting the drawer at close never changes
//     retroactively.
package reporting

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/reporting/db"
)

// Service builds reports. Its pool's role must be a member of orion_app.
type Service struct{ pool *pgxpool.Pool }

// NewService returns a Service using pool.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Cash is the reconciliation of one shift's drawer, in rupiah.
type Cash struct {
	OpeningCash int64
	Received    int64 // cash applied to the bills of the shift's sales
	Refunded    int64 // cash returned for voids made in the shift
	PayIn       int64
	PayOut      int64
	Expected    int64 // OpeningCash + Received - Refunded + PayIn - PayOut
	Counted     *int64
	// Difference is Counted - Expected: negative means the drawer is short. Nil while the shift is
	// open.
	Difference *int64
}

// Totals are the completed sales of a shift or a day.
type Totals struct {
	Count         int64
	Subtotal      int64 // before discounts
	Discounts     int64
	Net           int64 // Subtotal - Discounts
	ServiceCharge int64
	Tax           int64
	Total         int64 // what the bills came to, before cash rounding
	Rounding      int64 // cash rounding, positive or negative
}

// MethodTotal is what one payment method took.
type MethodTotal struct {
	Method   string
	Payments int64
	Amount   int64
}

// Voided is sales that were voided, and what they came to.
type Voided struct {
	Count int64
	Total int64
}

// DiscountTotals is the manual discounts given.
type DiscountTotals struct {
	Count  int64
	Amount int64
}

// ShiftInfo identifies a shift.
type ShiftInfo struct {
	ID           uuid.UUID
	OutletID     uuid.UUID
	DeviceID     uuid.UUID
	OpenedBy     uuid.UUID
	OpenedAt     time.Time
	BusinessDate time.Time
	ClosedBy     *uuid.UUID
	ClosedAt     *time.Time
}

// ShiftReport is the end-of-shift report.
type ShiftReport struct {
	Shift          ShiftInfo
	Cash           Cash
	Sales          Totals
	PaymentMethods []MethodTotal
	Discounts      DiscountTotals
	// VoidedSales are this shift's sales that were voided (in this shift or later).
	VoidedSales Voided
	// VoidsRecorded are the voids made during this shift, of any shift's sales.
	VoidsRecorded  Voided
	NoSaleOpenings int64
	// Flags counts the review flags raised about the shift and its sales.
	Flags int64
}

// ShiftSummary is one line of the day report's list of shifts.
type ShiftSummary struct {
	Shift ShiftInfo
	Cash  Cash
}

// CashMovements are the day's pay ins, pay outs and drawer openings.
type CashMovements struct {
	PayIn, PayOut           int64
	PayIns, PayOuts, NoSale int64
}

// DayReport is the end-of-day report of one outlet and business date.
type DayReport struct {
	OutletID       uuid.UUID
	Date           time.Time
	Sales          Totals
	PaymentMethods []MethodTotal
	Discounts      DiscountTotals
	// VoidedSales are the day's sales that were voided, whenever the void was made.
	VoidedSales   Voided
	CashMovements CashMovements
	// Shifts are the shifts opened on this business date, with their cash reconciliation, in the
	// order they opened. OpenShifts counts those not yet closed.
	Shifts     []ShiftSummary
	OpenShifts int
	// Cash adds up the reconciliation of the closed shifts: expected and counted cash, and how far
	// the drawers were over or short in all.
	Cash Cash
	// Flags counts review flags by code for the day's sales, voids, shifts and cash movements.
	Flags map[string]int64
}

// ShiftOutlet returns the outlet a shift belongs to, for the permission check before a report.
func (s *Service) ShiftOutlet(ctx context.Context, tenantID, shiftID uuid.UUID) (uuid.UUID, error) {
	var outlet uuid.UUID
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		sh, err := db.New(tx).GetShiftForReport(ctx, db.GetShiftForReportParams{TenantID: tenantID, ID: shiftID})
		if err != nil {
			return mapNoRows(err)
		}
		outlet = sh.OutletID
		return nil
	})
	return outlet, err
}

// ShiftReport builds the end-of-shift report.
func (s *Service) ShiftReport(ctx context.Context, tenantID, shiftID uuid.UUID) (ShiftReport, error) {
	var out ShiftReport
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		sh, err := q.GetShiftForReport(ctx, db.GetShiftForReportParams{TenantID: tenantID, ID: shiftID})
		if err != nil {
			return mapNoRows(err)
		}
		cash, err := q.ShiftCash(ctx, db.ShiftCashParams{TenantID: tenantID, ShiftIds: []uuid.UUID{shiftID}})
		if err != nil {
			return err
		}
		sales, err := q.ShiftSales(ctx, db.ShiftSalesParams{TenantID: tenantID, ShiftID: shiftID})
		if err != nil {
			return err
		}
		methods, err := q.ShiftPaymentMethods(ctx, db.ShiftPaymentMethodsParams{TenantID: tenantID, ShiftID: shiftID})
		if err != nil {
			return err
		}
		discounts, err := q.ShiftDiscounts(ctx, db.ShiftDiscountsParams{TenantID: tenantID, ShiftID: shiftID})
		if err != nil {
			return err
		}
		voids, err := q.ShiftVoidsRecorded(ctx, db.ShiftVoidsRecordedParams{TenantID: tenantID, ShiftID: shiftID})
		if err != nil {
			return err
		}
		flags, err := q.ShiftFlagCount(ctx, db.ShiftFlagCountParams{TenantID: tenantID, ShiftID: shiftID})
		if err != nil {
			return err
		}

		out = ShiftReport{
			Shift: shiftInfo(sh.ID, sh.OutletID, sh.DeviceID, sh.OpenedBy, sh.OpenedAt, sh.BusinessDate, sh.ClosedBy, sh.ClosedAt),
			Sales: Totals{
				Count: sales.Sales, Subtotal: sales.Subtotal, Discounts: sales.DiscountTotal, Net: sales.Subtotal - sales.DiscountTotal,
				ServiceCharge: sales.ServiceCharge, Tax: sales.Tax, Total: sales.Total, Rounding: sales.Rounding,
			},
			PaymentMethods: methodTotals(len(methods), func(i int) (string, int64, int64) { return methods[i].Method, methods[i].Payments, methods[i].Amount }),
			Discounts:      DiscountTotals{Count: discounts.Discounts, Amount: discounts.Amount},
			VoidedSales:    Voided{Count: sales.Voided, Total: sales.VoidedTotal},
			VoidsRecorded:  Voided{Count: voids.Voids, Total: voids.Total},
			Flags:          flags,
		}
		if len(cash) == 1 {
			out.Cash = reconcile(sh.OpeningCash, sh.CountedCash, cash[0].Received, cash[0].Refunded, cash[0].PayIn, cash[0].PayOut)
			out.NoSaleOpenings = cash[0].NoSales
		}
		return nil
	})
	return out, err
}

// DayReport builds the end-of-day report for an outlet and a business date.
func (s *Service) DayReport(ctx context.Context, tenantID, outletID uuid.UUID, date time.Time) (DayReport, error) {
	date = dateOnly(date)
	out := DayReport{OutletID: outletID, Date: date, Flags: map[string]int64{}}
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		day := pgtype.Date{Time: date, Valid: true}
		sales, err := q.DaySales(ctx, db.DaySalesParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		methods, err := q.DayPaymentMethods(ctx, db.DayPaymentMethodsParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		discounts, err := q.DayDiscounts(ctx, db.DayDiscountsParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		moves, err := q.DayCashMovements(ctx, db.DayCashMovementsParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		flags, err := q.DayFlags(ctx, db.DayFlagsParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		shifts, err := q.ShiftsOfDay(ctx, db.ShiftsOfDayParams{TenantID: tenantID, OutletID: outletID, BusinessDate: day})
		if err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(shifts))
		for i, sh := range shifts {
			ids[i] = sh.ID
		}
		cash, err := q.ShiftCash(ctx, db.ShiftCashParams{TenantID: tenantID, ShiftIds: ids})
		if err != nil {
			return err
		}
		cashOf := map[uuid.UUID]db.ShiftCashRow{}
		for _, c := range cash {
			cashOf[c.ShiftID] = c
		}

		out.Sales = Totals{
			Count: sales.Sales, Subtotal: sales.Subtotal, Discounts: sales.DiscountTotal, Net: sales.Subtotal - sales.DiscountTotal,
			ServiceCharge: sales.ServiceCharge, Tax: sales.Tax, Total: sales.Total, Rounding: sales.Rounding,
		}
		out.PaymentMethods = methodTotals(len(methods), func(i int) (string, int64, int64) { return methods[i].Method, methods[i].Payments, methods[i].Amount })
		out.Discounts = DiscountTotals{Count: discounts.Discounts, Amount: discounts.Amount}
		out.VoidedSales = Voided{Count: sales.Voided, Total: sales.VoidedTotal}
		for _, m := range moves {
			switch m.Kind {
			case "pay_in":
				out.CashMovements.PayIns, out.CashMovements.PayIn = m.Movements, m.Amount
			case "pay_out":
				out.CashMovements.PayOuts, out.CashMovements.PayOut = m.Movements, m.Amount
			case "no_sale":
				out.CashMovements.NoSale = m.Movements
			}
		}
		for _, f := range flags {
			out.Flags[f.Code] = f.Flags
		}

		out.Shifts = make([]ShiftSummary, len(shifts))
		var opening, received, refunded, payIn, payOut, expected, counted int64
		for i, sh := range shifts {
			c := cashOf[sh.ID]
			rec := reconcile(sh.OpeningCash, sh.CountedCash, c.Received, c.Refunded, c.PayIn, c.PayOut)
			out.Shifts[i] = ShiftSummary{Shift: shiftInfo(sh.ID, sh.OutletID, sh.DeviceID, sh.OpenedBy, sh.OpenedAt, sh.BusinessDate, sh.ClosedBy, sh.ClosedAt), Cash: rec}
			if sh.ClosedAt == nil {
				out.OpenShifts++
				continue
			}
			opening += rec.OpeningCash
			received += rec.Received
			refunded += rec.Refunded
			payIn += rec.PayIn
			payOut += rec.PayOut
			expected += rec.Expected
			counted += *rec.Counted
		}
		out.Cash = Cash{OpeningCash: opening, Received: received, Refunded: refunded, PayIn: payIn, PayOut: payOut, Expected: expected}
		if len(shifts) > out.OpenShifts {
			diff := counted - expected
			out.Cash.Counted, out.Cash.Difference = &counted, &diff
		}
		return nil
	})
	return out, err
}

// Ways to group the sales report.
const (
	ByDay           = "day"
	ByItem          = "item"
	ByPaymentMethod = "payment_method"
)

// MaxReportDays bounds a sales report's range.
const MaxReportDays = 366

// DaySales is one business date of the sales report.
type DaySales struct {
	Date  time.Time
	Sales Totals
}

// ItemSales is one variant of the sales report. Discounts include the lines' share of bill
// discounts, so Net over all rows is the range's net sales.
type ItemSales struct {
	ItemID, VariantID     uuid.UUID
	ItemName, VariantName string
	Quantity              int64
	Gross, Discounts, Net int64
}

// SalesReport is an outlet's completed sales from From to To (business dates, inclusive). Only the
// list for GroupBy is filled.
type SalesReport struct {
	OutletID       uuid.UUID
	From, To       time.Time
	GroupBy        string
	Days           []DaySales
	Items          []ItemSales
	PaymentMethods []MethodTotal
}

// SalesReport builds the sales report of an outlet over a range of business dates.
func (s *Service) SalesReport(ctx context.Context, tenantID, outletID uuid.UUID, from, to time.Time, groupBy string) (SalesReport, error) {
	from, to = dateOnly(from), dateOnly(to)
	switch {
	case from.Year() < 2000 || to.Year() > 2100:
		return SalesReport{}, fmt.Errorf("%w: dates are out of range", kernel.ErrValidation)
	case to.Before(from):
		return SalesReport{}, fmt.Errorf("%w: to is before from", kernel.ErrValidation)
	case to.Sub(from) >= MaxReportDays*24*time.Hour:
		return SalesReport{}, fmt.Errorf("%w: a report covers at most %d days", kernel.ErrValidation, MaxReportDays)
	}
	out := SalesReport{OutletID: outletID, From: from, To: to, GroupBy: groupBy}
	f, t := pgtype.Date{Time: from, Valid: true}, pgtype.Date{Time: to, Valid: true}
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		switch groupBy {
		case ByDay:
			rows, err := q.SalesByDay(ctx, db.SalesByDayParams{TenantID: tenantID, OutletID: outletID, FromDate: f, ToDate: t})
			if err != nil {
				return err
			}
			out.Days = make([]DaySales, len(rows))
			for i, r := range rows {
				out.Days[i] = DaySales{Date: r.BusinessDate.Time, Sales: Totals{
					Count: r.Sales, Subtotal: r.Subtotal, Discounts: r.DiscountTotal, Net: r.Subtotal - r.DiscountTotal,
					ServiceCharge: r.ServiceCharge, Tax: r.Tax, Total: r.Total, Rounding: r.Rounding,
				}}
			}
		case ByItem:
			rows, err := q.SalesByItem(ctx, db.SalesByItemParams{TenantID: tenantID, OutletID: outletID, FromDate: f, ToDate: t})
			if err != nil {
				return err
			}
			out.Items = make([]ItemSales, len(rows))
			for i, r := range rows {
				out.Items[i] = ItemSales{
					ItemID: r.ItemID, VariantID: r.VariantID, ItemName: r.ItemName, VariantName: r.VariantName,
					Quantity: r.Quantity, Gross: r.Gross, Discounts: r.Discounts, Net: r.Net,
				}
			}
		case ByPaymentMethod:
			rows, err := q.SalesByPaymentMethod(ctx, db.SalesByPaymentMethodParams{TenantID: tenantID, OutletID: outletID, FromDate: f, ToDate: t})
			if err != nil {
				return err
			}
			out.PaymentMethods = methodTotals(len(rows), func(i int) (string, int64, int64) { return rows[i].Method, rows[i].Payments, rows[i].Amount })
		default:
			return fmt.Errorf("%w: group_by must be day, item or payment_method", kernel.ErrValidation)
		}
		return nil
	})
	return out, err
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func reconcile(opening int64, counted *int64, received, refunded, payIn, payOut int64) Cash {
	c := Cash{OpeningCash: opening, Received: received, Refunded: refunded, PayIn: payIn, PayOut: payOut}
	c.Expected = opening + received - refunded + payIn - payOut
	if counted != nil {
		v := *counted
		diff := v - c.Expected
		c.Counted, c.Difference = &v, &diff
	}
	return c
}

func shiftInfo(id, outlet, device, openedBy uuid.UUID, openedAt time.Time, date pgtype.Date, closedBy *uuid.UUID, closedAt *time.Time) ShiftInfo {
	return ShiftInfo{ID: id, OutletID: outlet, DeviceID: device, OpenedBy: openedBy, OpenedAt: openedAt, BusinessDate: date.Time, ClosedBy: closedBy, ClosedAt: closedAt}
}

func methodTotals(n int, at func(i int) (method string, payments, amount int64)) []MethodTotal {
	out := make([]MethodTotal, n)
	for i := range out {
		out[i].Method, out[i].Payments, out[i].Amount = at(i)
	}
	return out
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	return err
}
