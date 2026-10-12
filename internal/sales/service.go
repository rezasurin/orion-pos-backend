package sales

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/sales/db"
)

// Service is the read side of the sales module, for the back office. Sales are written only by the
// Projector.
type Service struct{ pool *pgxpool.Pool }

// NewService returns a Service using pool, whose role must be a member of orion_app.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

const (
	defaultSalesPage = 50
	maxSalesPage     = 200
)

// SaleFilter narrows ListSales. Zero values mean "no filter".
type SaleFilter struct {
	// OutletIDs limits the list to these outlets; nil means every outlet. Callers pass the outlets
	// where the user may see sales.
	OutletIDs []uuid.UUID
	// From and To are business dates, inclusive. Only the date part is used.
	From, To      *time.Time
	Status        string // completed or voided
	ReceiptNumber string // exact
	StaffID       *uuid.UUID
	FlaggedOnly   bool
}

// SaleSummary is one line of the sales list.
type SaleSummary struct {
	ID             uuid.UUID
	OutletID       uuid.UUID
	DeviceID       uuid.UUID
	ShiftID        uuid.UUID
	StaffID        uuid.UUID
	ReceiptNumber  string
	DeviceTime     time.Time
	ReceivedAt     time.Time
	BusinessDate   time.Time
	Status         string
	Subtotal       int64
	DiscountTotal  int64
	ServiceCharge  int64
	Tax            int64
	TaxIncluded    bool
	RoundingAmount int64
	Total          int64
	Refunded       int64 // given back by refunds so far; the amounts above stay as rung up
	Payments       []Payment
	FlagCodes      []string
}

// Payment is one payment of a sale.
type Payment struct {
	Method    string
	Amount    int64
	Tendered  *int64
	Change    *int64
	Reference string
	Status    string
}

// SaleLine is one line of a sale as recorded.
type SaleLine struct {
	LineNo                int
	VariantID             uuid.UUID
	Name                  string
	UnitPrice             int64
	Quantity              int
	Modifiers             []SaleModifier
	Discount              int64
	AllocatedBillDiscount int64
	Total                 int64
}

// SaleModifier is a modifier chosen on a line.
type SaleModifier struct {
	ModifierID uuid.UUID
	Name       string
	PriceDelta int64
}

// SaleDiscount is a discount on a line (LineNo set) or on the whole bill.
type SaleDiscount struct {
	LineNo     *int
	Kind       string
	Value      int64
	Amount     int64
	Reason     string
	ApprovedBy *uuid.UUID
}

// VoidInfo says a sale was voided.
type VoidInfo struct {
	ID           uuid.UUID
	ShiftID      uuid.UUID
	StaffID      uuid.UUID
	ApprovedBy   *uuid.UUID
	Reason       string
	DeviceTime   time.Time
	ReceivedAt   time.Time
	BusinessDate time.Time
}

// RefundInfo is money given back for some of a sale.
type RefundInfo struct {
	ID           uuid.UUID
	ShiftID      uuid.UUID
	StaffID      uuid.UUID
	ApprovedBy   *uuid.UUID
	Method       string
	Amount       int64
	Reason       string
	DeviceTime   time.Time
	ReceivedAt   time.Time
	BusinessDate time.Time
	Lines        []RefundLine
}

// RefundLine is what came back of one sale line.
type RefundLine struct {
	LineNo   int
	Quantity int
	Amount   int64
}

// FlagInfo is a review flag about a sale, its void or one of its refunds.
type FlagInfo struct {
	Code       string
	TargetType string // sale or void
	Detail     json.RawMessage
	CreatedAt  time.Time
}

// SaleDetail is a sale with everything recorded about it.
type SaleDetail struct {
	SaleSummary
	PricingVersion int
	Pricing        json.RawMessage // the calculation settings the device used
	CatalogSeq     int64
	Lines          []SaleLine
	Discounts      []SaleDiscount
	Void           *VoidInfo
	Refunds        []RefundInfo
	Flags          []FlagInfo
}

// SaleCursor continues a list after the last sale of a page.
type SaleCursor struct {
	Date time.Time // business date
	At   time.Time // device time
	ID   uuid.UUID
}

// Encode renders the cursor as the opaque string clients pass back.
func (c SaleCursor) Encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.Date.Format(time.DateOnly) + "/" + c.At.UTC().Format(time.RFC3339Nano) + "/" + c.ID.String()))
}

// ParseSaleCursor reads a cursor made by Encode.
func ParseSaleCursor(s string) (SaleCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		if parts := strings.Split(string(raw), "/"); len(parts) == 3 {
			date, derr := time.Parse(time.DateOnly, parts[0])
			at, terr := time.Parse(time.RFC3339Nano, parts[1])
			uid, uerr := uuid.Parse(parts[2])
			if derr == nil && terr == nil && uerr == nil {
				return SaleCursor{Date: date, At: at, ID: uid}, nil
			}
		}
	}
	return SaleCursor{}, fmt.Errorf("%w: the cursor is not valid", kernel.ErrValidation)
}

// SalePage is one page of the sales list.
type SalePage struct {
	Items []SaleSummary
	// Next continues the list; nil on the last page.
	Next *SaleCursor
}

// ListSales returns a page of sales, newest business day first and newest sale first within a day,
// with their payments and the codes of their flags from two more queries, not one per sale.
func (s *Service) ListSales(ctx context.Context, tenantID uuid.UUID, f SaleFilter, after *SaleCursor, limit int) (SalePage, error) {
	switch {
	case limit <= 0:
		limit = defaultSalesPage
	case limit > maxSalesPage:
		limit = maxSalesPage
	}
	if f.Status != "" && f.Status != "completed" && f.Status != "voided" {
		return SalePage{}, fmt.Errorf("%w: status must be completed or voided", kernel.ErrValidation)
	}

	p := db.ListSalesParams{TenantID: tenantID, OutletIds: f.OutletIDs, StaffID: f.StaffID, FlaggedOnly: f.FlaggedOnly, PageSize: int32(limit + 1)} //nolint:gosec // limit is at most 200
	if f.From != nil {
		p.FromDate = pgtype.Date{Time: *f.From, Valid: true}
	}
	if f.To != nil {
		p.ToDate = pgtype.Date{Time: *f.To, Valid: true}
	}
	if f.Status != "" {
		p.Status = &f.Status
	}
	if f.ReceiptNumber != "" {
		p.ReceiptNumber = &f.ReceiptNumber
	}
	if after != nil {
		p.AfterDate, p.AfterTime, p.AfterID = pgtype.Date{Time: after.Date, Valid: true}, &after.At, &after.ID
	}

	var out SalePage
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListSales(ctx, p)
		if err != nil {
			return err
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		ids := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		pays, err := q.ListPaymentsBySales(ctx, db.ListPaymentsBySalesParams{TenantID: tenantID, SaleIds: ids})
		if err != nil {
			return err
		}
		flags, err := q.ListFlagsBySales(ctx, db.ListFlagsBySalesParams{TenantID: tenantID, SaleIds: ids})
		if err != nil {
			return err
		}
		paysOf := map[uuid.UUID][]Payment{}
		for _, py := range pays {
			paysOf[py.SaleID] = append(paysOf[py.SaleID], toPayment(py.Method, py.Amount, py.Tendered, py.Change, py.Reference, py.Status))
		}
		flagsOf := map[uuid.UUID][]string{}
		for _, fl := range flags {
			flagsOf[fl.SaleID] = append(flagsOf[fl.SaleID], fl.Code)
		}

		out.Items = make([]SaleSummary, len(rows))
		for i, r := range rows {
			out.Items[i] = SaleSummary{
				ID: r.ID, OutletID: r.OutletID, DeviceID: r.DeviceID, ShiftID: r.ShiftID, StaffID: r.StaffID, ReceiptNumber: r.ReceiptNumber,
				DeviceTime: r.DeviceTime, ReceivedAt: r.ReceivedAt, BusinessDate: r.BusinessDate.Time, Status: r.Status,
				Subtotal: r.Subtotal, DiscountTotal: r.DiscountTotal, ServiceCharge: r.ServiceCharge, Tax: r.Tax, TaxIncluded: r.TaxIncluded,
				RoundingAmount: r.RoundingAmount, Total: r.Total, Refunded: r.Refunded, Payments: orEmpty(paysOf[r.ID]), FlagCodes: orEmpty(flagsOf[r.ID]),
			}
		}
		if more {
			last := out.Items[len(out.Items)-1]
			out.Next = &SaleCursor{Date: last.BusinessDate, At: last.DeviceTime, ID: last.ID}
		}
		return nil
	})
	return out, err
}

// SaleOutlet returns the outlet a sale belongs to, for the permission check before showing it.
func (s *Service) SaleOutlet(ctx context.Context, tenantID, saleID uuid.UUID) (uuid.UUID, error) {
	var outlet uuid.UUID
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		r, err := db.New(tx).GetSale(ctx, db.GetSaleParams{TenantID: tenantID, ID: saleID})
		if err != nil {
			return mapNoRows(err)
		}
		outlet = r.OutletID
		return nil
	})
	return outlet, err
}

// GetSale returns one sale with its lines, modifiers, discounts, payments, void and flags.
func (s *Service) GetSale(ctx context.Context, tenantID, saleID uuid.UUID) (SaleDetail, error) {
	var out SaleDetail
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q := db.New(tx)
		r, err := q.GetSale(ctx, db.GetSaleParams{TenantID: tenantID, ID: saleID})
		if err != nil {
			return mapNoRows(err)
		}
		lines, err := q.ListSaleLines(ctx, db.ListSaleLinesParams{TenantID: tenantID, SaleID: saleID})
		if err != nil {
			return err
		}
		mods, err := q.ListSaleLineModifiers(ctx, db.ListSaleLineModifiersParams{TenantID: tenantID, SaleID: saleID})
		if err != nil {
			return err
		}
		discounts, err := q.ListSaleDiscounts(ctx, db.ListSaleDiscountsParams{TenantID: tenantID, SaleID: saleID})
		if err != nil {
			return err
		}
		pays, err := q.ListPaymentsBySales(ctx, db.ListPaymentsBySalesParams{TenantID: tenantID, SaleIds: []uuid.UUID{saleID}})
		if err != nil {
			return err
		}
		flags, err := q.ListFlagsBySales(ctx, db.ListFlagsBySalesParams{TenantID: tenantID, SaleIds: []uuid.UUID{saleID}})
		if err != nil {
			return err
		}

		out = SaleDetail{
			SaleSummary: SaleSummary{
				ID: r.ID, OutletID: r.OutletID, DeviceID: r.DeviceID, ShiftID: r.ShiftID, StaffID: r.StaffID, ReceiptNumber: r.ReceiptNumber,
				DeviceTime: r.DeviceTime, ReceivedAt: r.ReceivedAt, BusinessDate: r.BusinessDate.Time, Status: r.Status,
				Subtotal: r.Subtotal, DiscountTotal: r.DiscountTotal, ServiceCharge: r.ServiceCharge, Tax: r.Tax, TaxIncluded: r.TaxIncluded,
				RoundingAmount: r.RoundingAmount, Total: r.Total, Payments: []Payment{}, FlagCodes: []string{},
			},
			PricingVersion: int(r.PricingVersion), Pricing: r.Pricing, CatalogSeq: r.CatalogSeq,
			Lines: make([]SaleLine, len(lines)), Discounts: make([]SaleDiscount, len(discounts)), Flags: make([]FlagInfo, len(flags)),
		}
		modsOf := map[uuid.UUID][]SaleModifier{}
		for _, m := range mods {
			modsOf[m.SaleLineID] = append(modsOf[m.SaleLineID], SaleModifier{ModifierID: m.ModifierID, Name: m.NameSnapshot, PriceDelta: m.PriceDelta})
		}
		for i, l := range lines {
			out.Lines[i] = SaleLine{
				LineNo: int(l.LineNo), VariantID: l.VariantID, Name: l.NameSnapshot, UnitPrice: l.UnitPrice, Quantity: int(l.Quantity),
				Modifiers: orEmpty(modsOf[l.ID]), Discount: l.LineDiscount, AllocatedBillDiscount: l.AllocatedBillDiscount, Total: l.LineTotal,
			}
		}
		for i, d := range discounts {
			sd := SaleDiscount{Kind: d.Kind, Value: d.Value, Amount: d.Amount, Reason: d.Reason, ApprovedBy: d.ApprovedBy}
			if d.LineNo != nil {
				n := int(*d.LineNo)
				sd.LineNo = &n
			}
			out.Discounts[i] = sd
		}
		for _, py := range pays {
			out.Payments = append(out.Payments, toPayment(py.Method, py.Amount, py.Tendered, py.Change, py.Reference, py.Status))
		}
		for i, fl := range flags {
			out.Flags[i] = FlagInfo{Code: fl.Code, TargetType: fl.TargetType, Detail: fl.Detail, CreatedAt: fl.CreatedAt}
			out.FlagCodes = append(out.FlagCodes, fl.Code)
		}
		refunds, err := q.ListRefundsOfSale(ctx, db.ListRefundsOfSaleParams{TenantID: tenantID, SaleID: saleID})
		if err != nil {
			return err
		}
		refundLines, err := q.ListRefundLinesOfSale(ctx, db.ListRefundLinesOfSaleParams{TenantID: tenantID, SaleID: saleID})
		if err != nil {
			return err
		}
		linesOf := map[uuid.UUID][]RefundLine{}
		for _, l := range refundLines {
			linesOf[l.RefundID] = append(linesOf[l.RefundID], RefundLine{LineNo: int(l.LineNo), Quantity: int(l.Quantity), Amount: l.Amount})
		}
		out.Refunds = make([]RefundInfo, len(refunds))
		for i, rf := range refunds {
			out.Refunds[i] = RefundInfo{
				ID: rf.ID, ShiftID: rf.ShiftID, StaffID: rf.StaffID, ApprovedBy: rf.ApprovedBy, Method: rf.Method, Amount: rf.Amount,
				Reason: rf.Reason, DeviceTime: rf.DeviceTime, ReceivedAt: rf.ReceivedAt, BusinessDate: rf.BusinessDate.Time,
				Lines: orEmpty(linesOf[rf.ID]),
			}
			out.Refunded += rf.Amount
		}
		v, err := q.GetVoidOfSale(ctx, db.GetVoidOfSaleParams{TenantID: tenantID, SaleID: saleID})
		switch {
		case err == nil:
			out.Void = &VoidInfo{
				ID: v.ID, ShiftID: v.ShiftID, StaffID: v.StaffID, ApprovedBy: v.ApprovedBy, Reason: v.Reason,
				DeviceTime: v.DeviceTime, ReceivedAt: v.ReceivedAt, BusinessDate: v.BusinessDate.Time,
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		return nil
	})
	return out, err
}

func toPayment(method string, amount int64, tendered, change *int64, reference, status string) Payment {
	return Payment{Method: method, Amount: amount, Tendered: tendered, Change: change, Reference: reference, Status: status}
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return kernel.ErrNotFound
	}
	return err
}
