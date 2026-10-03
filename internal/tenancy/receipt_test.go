package tenancy_test

import (
	"reflect"
	"testing"

	"github.com/rezasurin/orion-pos-backend/internal/tenancy"
)

func TestSampleReceipt(t *testing.T) {
	tests := []struct {
		name string
		s    tenancy.OutletSettings
		want tenancy.SampleReceipt // lines are checked separately
	}{
		{"no tax, no rounding", tenancy.OutletSettings{CashRoundingMode: "nearest"},
			tenancy.SampleReceipt{Subtotal: 94_500, Total: 94_500, CashTotal: 94_500}},
		{"tax and taxable service, cash rounded to 500",
			tenancy.OutletSettings{TaxRate: 1100, ServiceChargeRate: 500, ServiceChargeTaxable: true, CashRoundingUnit: 500, CashRoundingMode: "nearest"},
			// service 4,725; tax on 99,225 = 10,914.75 -> 10,915; total 110,140 -> 110,000
			tenancy.SampleReceipt{Subtotal: 94_500, ServiceCharge: 4_725, Tax: 10_915, Total: 110_140, RoundingAmount: -140, CashTotal: 110_000}},
		{"service not taxable, rounding up",
			tenancy.OutletSettings{TaxRate: 1000, ServiceChargeRate: 1000, CashRoundingUnit: 100, CashRoundingMode: "up"},
			// service 9,450; tax on 94,500 = 9,450; total 113,400
			tenancy.SampleReceipt{Subtotal: 94_500, ServiceCharge: 9_450, Tax: 9_450, Total: 113_400, CashTotal: 113_400}},
		{"tax inclusive: shown, not added",
			tenancy.OutletSettings{PriceIncludesTax: true, TaxRate: 1100, ServiceChargeRate: 500, ServiceChargeTaxable: true, CashRoundingMode: "down", CashRoundingUnit: 1000},
			// base 99,225 contains 99,225*1100/11100 = 9,833.1 -> 9,833; total 99,225 -> 99,000
			tenancy.SampleReceipt{Subtotal: 94_500, ServiceCharge: 4_725, Tax: 9_833, TaxIncluded: true, Total: 99_225, RoundingAmount: -225, CashTotal: 99_000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tenancy.NewSampleReceipt(tt.s)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Lines) != 3 || got.Lines[0].Amount != 48_000 {
				t.Errorf("lines = %+v", got.Lines)
			}
			got.Lines = nil
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got  %+v\nwant %+v", got, tt.want)
			}
			if got.CashTotal != got.Total+got.RoundingAmount {
				t.Error("cash total does not reconcile with the rounding amount")
			}
		})
	}
}
