package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"

	openapi_types "github.com/oapi-codegen/runtime/types"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/reporting"
)

func (s *Server) GetShiftReport(ctx context.Context, req openapi.GetShiftReportRequestObject) (openapi.GetShiftReportResponseObject, error) {
	tenantID := principalFrom(ctx).TenantID
	// A shift of another business is "not found"; the permission is needed at the shift's outlet.
	outlet, err := s.Reporting.ShiftOutlet(ctx, tenantID, req.ShiftId)
	if err != nil {
		return nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermReportView, outlet) {
		return nil, &identity.ForbiddenError{Permission: string(identity.PermReportView), Reason: "report.view is needed at this outlet"}
	}
	r, err := s.Reporting.ShiftReport(ctx, tenantID, req.ShiftId)
	if err != nil {
		return nil, err
	}
	return openapi.GetShiftReport200JSONResponse{
		Shift: toShiftInfo(r.Shift), Cash: toReportCash(r.Cash), Sales: toReportTotals(r.Sales),
		PaymentMethods: toReportMethods(r.PaymentMethods), Discounts: openapi.ReportDiscounts{Count: r.Discounts.Count, Amount: r.Discounts.Amount},
		VoidedSales:    openapi.ReportVoided{Count: r.VoidedSales.Count, Total: r.VoidedSales.Total},
		VoidsRecorded:  openapi.ReportVoided{Count: r.VoidsRecorded.Count, Total: r.VoidsRecorded.Total},
		NoSaleOpenings: r.NoSaleOpenings, Flags: r.Flags,
	}, nil
}

func (s *Server) GetDayReport(ctx context.Context, req openapi.GetDayReportRequestObject) (openapi.GetDayReportResponseObject, error) {
	tenantID := principalFrom(ctx).TenantID
	if _, err := s.Tenancy.GetOutlet(ctx, tenantID, req.Params.OutletId); err != nil {
		return nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermReportView, req.Params.OutletId) {
		return nil, &identity.ForbiddenError{Permission: string(identity.PermReportView), Reason: "report.view is needed at this outlet"}
	}
	if req.Date.Year() < 2000 || req.Date.Year() > 2100 {
		return nil, fmt.Errorf("%w: date is out of range", kernel.ErrValidation)
	}
	r, err := s.Reporting.DayReport(ctx, tenantID, req.Params.OutletId, req.Date.Time)
	if err != nil {
		return nil, err
	}
	out := openapi.GetDayReport200JSONResponse{
		OutletId: r.OutletID, Date: openapi_types.Date{Time: r.Date}, Sales: toReportTotals(r.Sales),
		PaymentMethods: toReportMethods(r.PaymentMethods), Discounts: openapi.ReportDiscounts{Count: r.Discounts.Count, Amount: r.Discounts.Amount},
		VoidedSales: openapi.ReportVoided{Count: r.VoidedSales.Count, Total: r.VoidedSales.Total},
		OpenShifts:  r.OpenShifts, Cash: toReportCash(r.Cash), Flags: r.Flags,
	}
	out.CashMovements.PayIn, out.CashMovements.PayInCount = r.CashMovements.PayIn, r.CashMovements.PayIns
	out.CashMovements.PayOut, out.CashMovements.PayOutCount = r.CashMovements.PayOut, r.CashMovements.PayOuts
	out.CashMovements.NoSaleOpenings = r.CashMovements.NoSale
	out.Shifts = make([]struct {
		Cash  openapi.ReportCash      `json:"cash"`
		Shift openapi.ReportShiftInfo `json:"shift"`
	}, len(r.Shifts))
	for i, sh := range r.Shifts {
		out.Shifts[i].Shift, out.Shifts[i].Cash = toShiftInfo(sh.Shift), toReportCash(sh.Cash)
	}
	return out, nil
}

func (s *Server) GetSalesReport(ctx context.Context, req openapi.GetSalesReportRequestObject) (openapi.GetSalesReportResponseObject, error) {
	tenantID, p := principalFrom(ctx).TenantID, req.Params
	if _, err := s.Tenancy.GetOutlet(ctx, tenantID, p.OutletId); err != nil {
		return nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermReportView, p.OutletId) {
		return nil, &identity.ForbiddenError{Permission: string(identity.PermReportView), Reason: "report.view is needed at this outlet"}
	}
	r, err := s.Reporting.SalesReport(ctx, tenantID, p.OutletId, p.From.Time, p.To.Time, string(p.GroupBy))
	if err != nil {
		return nil, err
	}
	if p.Format != nil && *p.Format == openapi.Csv {
		body, err := salesReportCSV(r)
		if err != nil {
			return nil, err
		}
		return openapi.GetSalesReport200TextcsvResponse{Body: bytes.NewReader(body), ContentLength: int64(len(body))}, nil
	}
	out := openapi.GetSalesReport200JSONResponse{
		OutletId: r.OutletID, From: openapi_types.Date{Time: r.From}, To: openapi_types.Date{Time: r.To},
		GroupBy: openapi.SalesReportGroupBy(r.GroupBy),
	}
	switch r.GroupBy {
	case reporting.ByDay:
		days := make([]openapi.SalesReportDay, len(r.Days))
		for i, d := range r.Days {
			days[i] = openapi.SalesReportDay{Date: openapi_types.Date{Time: d.Date}, Sales: toReportTotals(d.Sales)}
		}
		out.Days = &days
	case reporting.ByItem:
		items := make([]openapi.SalesReportItem, len(r.Items))
		for i, it := range r.Items {
			items[i] = openapi.SalesReportItem{
				ItemId: it.ItemID, VariantId: it.VariantID, ItemName: it.ItemName, VariantName: it.VariantName,
				Quantity: it.Quantity, Gross: it.Gross, Discounts: it.Discounts, Net: it.Net,
			}
		}
		out.Items = &items
	case reporting.ByPaymentMethod:
		methods := toReportMethods(r.PaymentMethods)
		out.PaymentMethods = &methods
	}
	return out, nil
}

// salesReportCSV writes the report's rows with a header, amounts in whole rupiah.
func salesReportCSV(r reporting.SalesReport) ([]byte, error) {
	var rows [][]string
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	switch r.GroupBy {
	case reporting.ByDay:
		rows = append(rows, []string{"date", "sales", "subtotal", "discounts", "net", "service_charge", "tax", "total", "rounding"})
		for _, d := range r.Days {
			t := d.Sales
			rows = append(rows, []string{d.Date.Format("2006-01-02"), n(t.Count), n(t.Subtotal), n(t.Discounts), n(t.Net), n(t.ServiceCharge), n(t.Tax), n(t.Total), n(t.Rounding)})
		}
	case reporting.ByItem:
		rows = append(rows, []string{"item_name", "variant_name", "quantity", "gross", "discounts", "net", "item_id", "variant_id"})
		for _, it := range r.Items {
			rows = append(rows, []string{csvText(it.ItemName), csvText(it.VariantName), n(it.Quantity), n(it.Gross), n(it.Discounts), n(it.Net), it.ItemID.String(), it.VariantID.String()})
		}
	case reporting.ByPaymentMethod:
		rows = append(rows, []string{"method", "payments", "amount"})
		for _, m := range r.PaymentMethods {
			rows = append(rows, []string{m.Method, n(m.Payments), n(m.Amount)})
		}
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.WriteAll(rows); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// csvText keeps a spreadsheet from running a name as a formula: an item named "=HYPERLINK(...)"
// by someone with catalog access would otherwise run when the owner opens the file.
func csvText(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func toShiftInfo(s reporting.ShiftInfo) openapi.ReportShiftInfo {
	return openapi.ReportShiftInfo{
		Id: s.ID, OutletId: s.OutletID, DeviceId: s.DeviceID, OpenedBy: s.OpenedBy, OpenedAt: s.OpenedAt,
		BusinessDate: openapi_types.Date{Time: s.BusinessDate}, ClosedBy: s.ClosedBy, ClosedAt: s.ClosedAt,
	}
}

func toReportCash(c reporting.Cash) openapi.ReportCash {
	return openapi.ReportCash{
		OpeningCash: c.OpeningCash, Received: c.Received, Refunded: c.Refunded, PayIn: c.PayIn, PayOut: c.PayOut,
		Expected: c.Expected, Counted: c.Counted, Difference: c.Difference,
	}
}

func toReportTotals(t reporting.Totals) openapi.ReportTotals {
	return openapi.ReportTotals{
		Count: t.Count, Subtotal: t.Subtotal, Discounts: t.Discounts, Net: t.Net, ServiceCharge: t.ServiceCharge,
		Tax: t.Tax, Total: t.Total, Rounding: t.Rounding,
	}
}

func toReportMethods(ms []reporting.MethodTotal) []openapi.ReportMethod {
	out := make([]openapi.ReportMethod, len(ms))
	for i, m := range ms {
		out[i] = openapi.ReportMethod{Method: openapi.ReportMethodMethod(m.Method), Payments: m.Payments, Amount: m.Amount}
	}
	return out
}
