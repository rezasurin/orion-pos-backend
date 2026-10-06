package api

import (
	"context"
	"fmt"
	"time"

	openapi "github.com/rezasurin/orion-pos-backend/gen/openapi"
	"github.com/rezasurin/orion-pos-backend/internal/identity"
	"github.com/rezasurin/orion-pos-backend/internal/kernel"
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
			SuspendedAt:        t.SuspendedAt,
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
			BusinessDayCutoff:    cutoffString(st.BusinessDayCutoff),
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

func (s *Server) UpdateOutletSettings(ctx context.Context, req openapi.UpdateOutletSettingsRequestObject) (openapi.UpdateOutletSettingsResponseObject, error) {
	b := req.Body
	if b == nil {
		return nil, errBodyRequired
	}
	tenantID := principalFrom(ctx).TenantID
	// An outlet of another business is "not found"; the permission is needed at this outlet.
	if _, err := s.Tenancy.GetOutlet(ctx, tenantID, req.OutletId); err != nil {
		return nil, err
	}
	if !callerFrom(ctx).Access.HasAt(identity.PermSettingsManage, req.OutletId) {
		return nil, &identity.ForbiddenError{Permission: string(identity.PermSettingsManage), Reason: "settings.manage is needed at this outlet"}
	}
	in := tenancy.SettingsUpdate{
		PriceIncludesTax: b.PriceIncludesTax, ServiceChargeTaxable: b.ServiceChargeTaxable,
		ReceiptHeader: b.ReceiptHeader, ReceiptFooter: b.ReceiptFooter,
	}
	if b.Timezone != nil {
		tz := string(*b.Timezone)
		in.Timezone = &tz
	}
	if b.CashRoundingMode != nil {
		m := string(*b.CashRoundingMode)
		in.CashRoundingMode = &m
	}
	if b.BusinessDayCutoff != nil {
		d, err := parseCutoff(*b.BusinessDayCutoff)
		if err != nil {
			return nil, err
		}
		in.BusinessDayCutoff = &d
	}
	if b.TaxRateBp != nil {
		r := kernel.BasisPoints(*b.TaxRateBp)
		in.TaxRate = &r
	}
	if b.ServiceChargeRateBp != nil {
		r := kernel.BasisPoints(*b.ServiceChargeRateBp)
		in.ServiceChargeRate = &r
	}
	if b.CashRoundingUnit != nil {
		u := kernel.Rupiah(*b.CashRoundingUnit)
		in.CashRoundingUnit = &u
	}
	o, err := s.Tenancy.UpdateOutletSettings(ctx, tenantID, req.OutletId, in)
	if err != nil {
		return nil, err
	}
	return openapi.UpdateOutletSettings200JSONResponse(toOutlet(o)), nil
}

// cutoffString renders a time of day as HH:MM.
func cutoffString(d time.Duration) string {
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

func parseCutoff(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%w: business_day_cutoff must look like 04:00", kernel.ErrValidation)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
