package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/sales"
)

func (s *Server) ListSales(ctx context.Context, req openapi.ListSalesRequestObject) (openapi.ListSalesResponseObject, error) {
	p := req.Params
	tenantID := principalFrom(ctx).TenantID
	access := callerFrom(ctx).Access

	f := sales.SaleFilter{FlaggedOnly: flag(p.Flagged), StaffID: p.StaffId}
	switch {
	case p.OutletId != nil:
		// An outlet of another business is "not found"; the permission is needed at this outlet.
		if _, err := s.Tenancy.GetOutlet(ctx, tenantID, *p.OutletId); err != nil {
			return nil, err
		}
		if !access.HasAt(identity.PermReportView, *p.OutletId) {
			return nil, &identity.ForbiddenError{Permission: string(identity.PermReportView), Reason: "report.view is needed at this outlet"}
		}
		f.OutletIDs = []uuid.UUID{*p.OutletId}
	default:
		if all, outlets := access.OutletsWith(identity.PermReportView); !all {
			f.OutletIDs = append([]uuid.UUID{}, outlets...) // an empty list, not nil: nothing visible
		}
	}
	if p.From != nil {
		t := p.From.Time
		f.From = &t
	}
	if p.To != nil {
		t := p.To.Time
		f.To = &t
	}
	if p.Status != nil {
		f.Status = string(*p.Status)
	}
	if p.ReceiptNumber != nil {
		f.ReceiptNumber = *p.ReceiptNumber
	}
	var after *sales.SaleCursor
	if p.Cursor != nil && *p.Cursor != "" {
		c, err := sales.ParseSaleCursor(*p.Cursor)
		if err != nil {
			return nil, err
		}
		after = &c
	}
	limit := 0
	if p.Limit != nil {
		if *p.Limit < 1 || *p.Limit > 200 {
			return nil, fmt.Errorf("%w: limit must be between 1 and 200", kernel.ErrValidation)
		}
		limit = *p.Limit
	}

	page, err := s.Sales.ListSales(ctx, tenantID, f, after, limit)
	if err != nil {
		return nil, err
	}
	out := openapi.ListSales200JSONResponse{Items: make([]openapi.SaleSummary, len(page.Items))}
	for i, sm := range page.Items {
		out.Items[i] = toSaleSummary(sm)
	}
	if page.Next != nil {
		c := page.Next.Encode()
		out.NextCursor = &c
	}
	return out, nil
}

func (s *Server) GetSale(ctx context.Context, req openapi.GetSaleRequestObject) (openapi.GetSaleResponseObject, error) {
	tenantID := principalFrom(ctx).TenantID
	outlet, err := s.Sales.SaleOutlet(ctx, tenantID, req.SaleId)
	if err != nil {
		return nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermReportView, outlet) {
		return nil, &identity.ForbiddenError{Permission: string(identity.PermReportView), Reason: "report.view is needed at this outlet"}
	}
	d, err := s.Sales.GetSale(ctx, tenantID, req.SaleId)
	if err != nil {
		return nil, err
	}

	sm := toSaleSummary(d.SaleSummary)
	out := openapi.SaleDetail{
		BusinessDate: sm.BusinessDate, DeviceId: sm.DeviceId, DeviceTime: sm.DeviceTime, DiscountTotal: sm.DiscountTotal,
		FlagCodes: sm.FlagCodes, Id: sm.Id, OutletId: sm.OutletId, Payments: sm.Payments, ReceiptNumber: sm.ReceiptNumber,
		ReceivedAt: sm.ReceivedAt, RoundingAmount: sm.RoundingAmount, ServiceCharge: sm.ServiceCharge, ShiftId: sm.ShiftId,
		StaffId: sm.StaffId, Status: openapi.SaleDetailStatus(sm.Status), Subtotal: sm.Subtotal, Tax: sm.Tax,
		TaxIncluded: sm.TaxIncluded, Total: sm.Total, Refunded: sm.Refunded,
		PricingVersion: d.PricingVersion, CatalogSeq: d.CatalogSeq, Pricing: rawObject(d.Pricing),
		Lines: make([]openapi.SaleLine, len(d.Lines)),
		Discounts: make([]struct {
			Amount     int64                           `json:"amount"`
			ApprovedBy *openapi_types.UUID             `json:"approved_by,omitempty"`
			Kind       openapi.SaleDetailDiscountsKind `json:"kind"`
			LineNo     *int                            `json:"line_no,omitempty"`
			Reason     string                          `json:"reason"`
			Value      int64                           `json:"value"`
		}, len(d.Discounts)),
		Flags: make([]struct {
			Code       string                            `json:"code"`
			CreatedAt  time.Time                         `json:"created_at"`
			Detail     map[string]interface{}            `json:"detail"`
			TargetType openapi.SaleDetailFlagsTargetType `json:"target_type"`
		}, len(d.Flags)),
	}
	for i, l := range d.Lines {
		line := openapi.SaleLine{
			LineNo: l.LineNo, VariantId: l.VariantID, Name: l.Name, UnitPrice: l.UnitPrice, Quantity: l.Quantity,
			Discount: l.Discount, AllocatedBillDiscount: l.AllocatedBillDiscount, Total: l.Total,
		}
		line.Modifiers = make([]struct {
			ModifierId openapi_types.UUID `json:"modifier_id"`
			Name       string             `json:"name"`
			PriceDelta int64              `json:"price_delta"`
		}, len(l.Modifiers))
		for j, m := range l.Modifiers {
			line.Modifiers[j].ModifierId, line.Modifiers[j].Name, line.Modifiers[j].PriceDelta = m.ModifierID, m.Name, m.PriceDelta
		}
		out.Lines[i] = line
	}
	for i, dc := range d.Discounts {
		out.Discounts[i].Amount, out.Discounts[i].ApprovedBy, out.Discounts[i].Kind = dc.Amount, dc.ApprovedBy, openapi.SaleDetailDiscountsKind(dc.Kind)
		out.Discounts[i].LineNo, out.Discounts[i].Reason, out.Discounts[i].Value = dc.LineNo, dc.Reason, dc.Value
	}
	for i, fl := range d.Flags {
		out.Flags[i].Code, out.Flags[i].CreatedAt, out.Flags[i].Detail = fl.Code, fl.CreatedAt, rawObject(fl.Detail)
		out.Flags[i].TargetType = openapi.SaleDetailFlagsTargetType(fl.TargetType)
	}
	// The refunds' element type is an anonymous struct in the generated code; Grow gives n of them.
	out.Refunds = slices.Grow(out.Refunds, len(d.Refunds))[:len(d.Refunds)]
	for i, rf := range d.Refunds {
		r := &out.Refunds[i]
		r.Id, r.ShiftId, r.StaffId, r.ApprovedBy, r.Method = rf.ID, rf.ShiftID, rf.StaffID, rf.ApprovedBy, openapi.SaleDetailRefundsMethod(rf.Method)
		r.Amount, r.Reason, r.DeviceTime, r.ReceivedAt = rf.Amount, rf.Reason, rf.DeviceTime, rf.ReceivedAt
		r.BusinessDate = openapi_types.Date{Time: rf.BusinessDate}
		r.Lines = slices.Grow(r.Lines, len(rf.Lines))[:len(rf.Lines)]
		for j, l := range rf.Lines {
			r.Lines[j].LineNo, r.Lines[j].Quantity, r.Lines[j].Amount = l.LineNo, l.Quantity, l.Amount
		}
	}
	if v := d.Void; v != nil {
		out.Void = &struct {
			ApprovedBy   *openapi_types.UUID `json:"approved_by,omitempty"`
			BusinessDate openapi_types.Date  `json:"business_date"`
			DeviceTime   time.Time           `json:"device_time"`
			Id           openapi_types.UUID  `json:"id"`
			Reason       string              `json:"reason"`
			ReceivedAt   time.Time           `json:"received_at"`
			ShiftId      openapi_types.UUID  `json:"shift_id"`
			StaffId      openapi_types.UUID  `json:"staff_id"`
		}{
			ApprovedBy: v.ApprovedBy, BusinessDate: openapi_types.Date{Time: v.BusinessDate}, DeviceTime: v.DeviceTime, Id: v.ID,
			Reason: v.Reason, ReceivedAt: v.ReceivedAt, ShiftId: v.ShiftID, StaffId: v.StaffID,
		}
	}
	return openapi.GetSale200JSONResponse(out), nil
}

func toSaleSummary(s sales.SaleSummary) openapi.SaleSummary {
	out := openapi.SaleSummary{
		Id: s.ID, OutletId: s.OutletID, DeviceId: s.DeviceID, ShiftId: s.ShiftID, StaffId: s.StaffID, ReceiptNumber: s.ReceiptNumber,
		DeviceTime: s.DeviceTime, ReceivedAt: s.ReceivedAt, BusinessDate: openapi_types.Date{Time: s.BusinessDate},
		Status: openapi.SaleSummaryStatus(s.Status), Subtotal: s.Subtotal, DiscountTotal: s.DiscountTotal, ServiceCharge: s.ServiceCharge,
		Tax: s.Tax, TaxIncluded: s.TaxIncluded, RoundingAmount: s.RoundingAmount, Total: s.Total, Refunded: s.Refunded, FlagCodes: s.FlagCodes,
		Payments: make([]openapi.SalePayment, len(s.Payments)),
	}
	for i, p := range s.Payments {
		ref := p.Reference
		out.Payments[i] = openapi.SalePayment{
			Method: openapi.SalePaymentMethod(p.Method), Amount: p.Amount, Tendered: p.Tendered, Change: p.Change,
			Reference: &ref, Status: openapi.SalePaymentStatus(p.Status),
		}
	}
	return out
}

// rawObject reads stored JSON into the object the contract promises; anything unreadable becomes an
// empty object rather than an error, since it was written by this server.
func rawObject(raw json.RawMessage) map[string]interface{} {
	out := map[string]interface{}{}
	_ = json.Unmarshal(raw, &out)
	return out
}
