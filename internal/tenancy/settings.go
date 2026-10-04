package tenancy

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/tenancy/db"
)

// MaxBusinessDayCutoff is the latest a business day may begin: before noon, so a day is never
// shorter than 12 hours.
const MaxBusinessDayCutoff = 12*time.Hour - time.Minute

const (
	maxReceiptText      = 500
	maxCashRoundingUnit = 10_000
)

// BusinessDate is the business day an instant belongs to under these settings: its date in the
// outlet's time zone, moved back a day when it falls before BusinessDayCutoff (section 4.9). It is
// computed once, when a sale is projected, and stored.
func (s OutletSettings) BusinessDate(t time.Time) time.Time {
	loc, ok := kernel.LocationFor(s.Timezone)
	if !ok {
		loc = time.UTC // unreachable: the database only holds supported zones
	}
	return kernel.BusinessDate(t, loc, s.BusinessDayCutoff)
}

// SettingsUpdate changes outlet settings. Nil fields stay as they are.
type SettingsUpdate struct {
	Timezone             *string
	BusinessDayCutoff    *time.Duration // time of day the business day begins, 00:00 to 11:59
	PriceIncludesTax     *bool
	TaxRate              *kernel.BasisPoints
	ServiceChargeRate    *kernel.BasisPoints
	ServiceChargeTaxable *bool
	CashRoundingUnit     *kernel.Rupiah
	CashRoundingMode     *string // nearest, down or up
	ReceiptHeader        *string
	ReceiptFooter        *string
}

// UpdateOutletSettings changes an outlet's tax, service charge, rounding, time and receipt
// settings. The change reaches that outlet's devices through the change log, and is written to the
// audit log with the old and new values. Sales already recorded keep the amounts they were
// calculated with and the business date they were given.
func (s *Service) UpdateOutletSettings(ctx context.Context, tenantID, outletID uuid.UUID, in SettingsUpdate) (Outlet, error) {
	if err := validateSettings(in); err != nil {
		return Outlet{}, err
	}
	var out Outlet
	err := kernel.TenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		if err := kernel.LockTenant(ctx, tx); err != nil {
			return err
		}
		q := db.New(tx)
		cur, err := q.GetOutletSettingsForUpdate(ctx, db.GetOutletSettingsForUpdateParams{TenantID: tenantID, OutletID: outletID})
		if err != nil {
			return mapErr(err)
		}
		p := db.UpdateOutletSettingsParams{
			TenantID: tenantID, OutletID: outletID, Timezone: cur.Timezone, BusinessDayCutoff: cur.BusinessDayCutoff,
			PriceIncludesTax: cur.PriceIncludesTax, TaxRateBp: cur.TaxRateBp, ServiceChargeRateBp: cur.ServiceChargeRateBp,
			ServiceChargeTaxable: cur.ServiceChargeTaxable, CashRoundingUnit: cur.CashRoundingUnit,
			CashRoundingMode: cur.CashRoundingMode, ReceiptHeader: cur.ReceiptHeader, ReceiptFooter: cur.ReceiptFooter,
		}
		changes := map[string]any{}
		set := func(field string, before, after any, apply func()) {
			if before != after {
				changes[field] = map[string]any{"before": before, "after": after}
				apply()
			}
		}
		if in.Timezone != nil {
			set("timezone", p.Timezone, *in.Timezone, func() { p.Timezone = *in.Timezone })
		}
		if in.BusinessDayCutoff != nil {
			micros := int64(*in.BusinessDayCutoff / time.Microsecond)
			set("business_day_cutoff", cutoffText(p.BusinessDayCutoff.Microseconds), cutoffText(micros), func() {
				p.BusinessDayCutoff = pgtype.Time{Microseconds: micros, Valid: true}
			})
		}
		if in.PriceIncludesTax != nil {
			set("price_includes_tax", p.PriceIncludesTax, *in.PriceIncludesTax, func() { p.PriceIncludesTax = *in.PriceIncludesTax })
		}
		if in.TaxRate != nil {
			set("tax_rate_bp", int64(p.TaxRateBp), int64(*in.TaxRate), func() { p.TaxRateBp = int32(*in.TaxRate) }) //nolint:gosec // validated above
		}
		if in.ServiceChargeRate != nil {
			set("service_charge_rate_bp", int64(p.ServiceChargeRateBp), int64(*in.ServiceChargeRate), func() { p.ServiceChargeRateBp = int32(*in.ServiceChargeRate) }) //nolint:gosec // validated above
		}
		if in.ServiceChargeTaxable != nil {
			set("service_charge_taxable", p.ServiceChargeTaxable, *in.ServiceChargeTaxable, func() { p.ServiceChargeTaxable = *in.ServiceChargeTaxable })
		}
		if in.CashRoundingUnit != nil {
			set("cash_rounding_unit", int64(p.CashRoundingUnit), int64(*in.CashRoundingUnit), func() { p.CashRoundingUnit = int32(*in.CashRoundingUnit) }) //nolint:gosec // validated above
		}
		if in.CashRoundingMode != nil {
			set("cash_rounding_mode", string(p.CashRoundingMode), *in.CashRoundingMode, func() { p.CashRoundingMode = db.CashRoundingMode(*in.CashRoundingMode) })
		}
		if in.ReceiptHeader != nil {
			set("receipt_header", p.ReceiptHeader, *in.ReceiptHeader, func() { p.ReceiptHeader = *in.ReceiptHeader })
		}
		if in.ReceiptFooter != nil {
			set("receipt_footer", p.ReceiptFooter, *in.ReceiptFooter, func() { p.ReceiptFooter = *in.ReceiptFooter })
		}

		if len(changes) > 0 {
			if _, err = q.UpdateOutletSettings(ctx, p); err != nil {
				return mapErr(err)
			}
			if _, err := q.RecordChange(ctx, db.RecordChangeParams{
				EntityType: "outlet_settings", EntityID: outletID, Op: "upsert", OutletID: &outletID,
			}); err != nil {
				return err
			}
			if err := kernel.RecordAudit(ctx, tx, kernel.AuditEntry{
				Action: "settings.updated", TargetType: "outlet", TargetID: outletID, Detail: changes,
			}); err != nil {
				return err
			}
		}
		o, err := q.GetOutletWithSettings(ctx, db.GetOutletWithSettingsParams{TenantID: tenantID, ID: outletID})
		if err != nil {
			return mapErr(err)
		}
		out = toOutlet(o.Outlet, o.OutletSetting)
		return nil
	})
	return out, err
}

func cutoffText(micros int64) string {
	d := time.Duration(micros) * time.Microsecond
	return fmt.Sprintf("%02d:%02d", int(d.Hours()), int(d.Minutes())%60)
}

func validateSettings(in SettingsUpdate) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{kernel.ErrValidation}, args...)...)
	}
	if in.Timezone != nil && !isTimezone(*in.Timezone) {
		return bad("timezone must be one of %v", Timezones)
	}
	if in.BusinessDayCutoff != nil {
		d := *in.BusinessDayCutoff
		if d < 0 || d > MaxBusinessDayCutoff || d%time.Minute != 0 {
			return bad("business_day_cutoff must be a time between 00:00 and 11:59")
		}
	}
	if in.TaxRate != nil && (*in.TaxRate < 0 || *in.TaxRate > 10_000) {
		return bad("tax_rate_bp must be between 0 and 10000")
	}
	if in.ServiceChargeRate != nil && (*in.ServiceChargeRate < 0 || *in.ServiceChargeRate > 10_000) {
		return bad("service_charge_rate_bp must be between 0 and 10000")
	}
	if in.CashRoundingUnit != nil && (*in.CashRoundingUnit < 0 || *in.CashRoundingUnit > maxCashRoundingUnit) {
		return bad("cash_rounding_unit must be between 0 and %d rupiah", maxCashRoundingUnit)
	}
	if in.CashRoundingMode != nil {
		switch *in.CashRoundingMode {
		case "nearest", "down", "up":
		default:
			return bad("cash_rounding_mode must be nearest, down or up")
		}
	}
	for field, v := range map[string]*string{"receipt_header": in.ReceiptHeader, "receipt_footer": in.ReceiptFooter} {
		if v != nil && len([]rune(*v)) > maxReceiptText {
			return bad("%s is at most %d characters", field, maxReceiptText)
		}
	}
	return nil
}
