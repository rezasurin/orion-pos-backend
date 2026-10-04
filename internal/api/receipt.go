package api

import (
	"context"
	"fmt"
	"time"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func (s *Server) GetReceiptTest(ctx context.Context, _ openapi.GetReceiptTestRequestObject) (openapi.GetReceiptTestResponseObject, error) {
	p := principalFrom(ctx)
	dev, err := s.Identity.GetDevice(ctx, p)
	if err != nil {
		return nil, err
	}
	outlet, err := s.Tenancy.GetOutlet(ctx, p.TenantID, p.OutletID)
	if err != nil {
		return nil, err
	}
	r, err := tenancy.NewSampleReceipt(outlet.Settings)
	if err != nil {
		return nil, err
	}
	out := openapi.GetReceiptTest200JSONResponse{
		Header: outlet.Settings.ReceiptHeader, Footer: outlet.Settings.ReceiptFooter,
		ReceiptNumber: fmt.Sprintf("%s-%02d-%06d", outlet.Code, dev.Code, 1),
		PrintedAt:     time.Now().UTC(),
		Subtotal:      int64(r.Subtotal), ServiceCharge: int64(r.ServiceCharge), Tax: int64(r.Tax), TaxIncluded: r.TaxIncluded,
		Total: int64(r.Total), RoundingAmount: int64(r.RoundingAmount), CashTotal: int64(r.CashTotal),
		PricingVersion: "sample-0",
	}
	out.Outlet.Code, out.Outlet.Name, out.Outlet.Address, out.Outlet.Timezone = outlet.Code, outlet.Name, outlet.Address, outlet.Settings.Timezone
	for _, l := range r.Lines {
		out.Lines = append(out.Lines, struct {
			Amount    int64  `json:"amount"`
			Name      string `json:"name"`
			Quantity  int    `json:"quantity"`
			UnitPrice int64  `json:"unit_price"`
		}{Amount: int64(l.Amount), Name: l.Name, Quantity: int(l.Quantity), UnitPrice: int64(l.UnitPrice)})
	}
	return out, nil
}
