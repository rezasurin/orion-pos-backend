package api

import (
	"context"
	"fmt"

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
