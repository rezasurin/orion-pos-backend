package api

import (
	"context"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func (s *Server) GetMe(ctx context.Context, _ openapi.GetMeRequestObject) (openapi.GetMeResponseObject, error) {
	p := principalFrom(ctx)
	me, err := s.Identity.GetMe(ctx, p)
	if err != nil {
		return nil, err
	}
	t, err := s.Tenancy.GetTenant(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	access, _ := identity.AccessFrom(ctx)
	perms := access.Permissions()
	permissions := make([]string, len(perms))
	for i, p := range perms {
		permissions[i] = string(p)
	}
	return openapi.GetMe200JSONResponse{
		IsOwner:     me.IsOwner,
		Permissions: permissions,
		User: openapi.User{
			Id: me.User.ID, Email: me.User.Email, EmailVerified: me.User.EmailVerified,
			Locale: openapi.UserLocale(me.User.Locale),
		},
		Tenant: openapi.TenantSummary{
			Id: t.ID, Name: t.Name, Slug: t.Slug,
			SubscriptionStatus: openapi.TenantSummarySubscriptionStatus(t.SubscriptionStatus),
		},
	}, nil
}

func (s *Server) ListOutlets(ctx context.Context, _ openapi.ListOutletsRequestObject) (openapi.ListOutletsResponseObject, error) {
	outlets, err := s.Tenancy.ListOutlets(ctx, principalFrom(ctx).TenantID)
	if err != nil {
		return nil, err
	}
	items := make([]openapi.Outlet, len(outlets))
	for i, o := range outlets {
		items[i] = toOutlet(o)
	}
	return openapi.ListOutlets200JSONResponse{Items: items}, nil
}

func (s *Server) GetOutlet(ctx context.Context, req openapi.GetOutletRequestObject) (openapi.GetOutletResponseObject, error) {
	o, err := s.Tenancy.GetOutlet(ctx, principalFrom(ctx).TenantID, req.OutletId)
	if err != nil {
		return nil, err
	}
	return openapi.GetOutlet200JSONResponse(toOutlet(o)), nil
}

func toOutlet(o tenancy.Outlet) openapi.Outlet {
	st := o.Settings
	return openapi.Outlet{
		Id: o.ID, Code: o.Code, Name: o.Name, Address: o.Address,
		Settings: openapi.OutletSettings{
			Timezone:             openapi.OutletSettingsTimezone(st.Timezone),
			PriceIncludesTax:     st.PriceIncludesTax,
			TaxRateBp:            int(st.TaxRate),
			ServiceChargeRateBp:  int(st.ServiceChargeRate),
			ServiceChargeTaxable: st.ServiceChargeTaxable,
			CashRoundingUnit:     int(st.CashRoundingUnit),
			CashRoundingMode:     openapi.OutletSettingsCashRoundingMode(st.CashRoundingMode),
			ReceiptHeader:        st.ReceiptHeader,
			ReceiptFooter:        st.ReceiptFooter,
		},
	}
}
