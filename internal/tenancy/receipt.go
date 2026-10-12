package tenancy

import (
	"fmt"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
)

// SampleLine is one line of the sample sale.
type SampleLine struct {
	Name      string
	Quantity  int64
	UnitPrice kernel.Rupiah
	Amount    kernel.Rupiah
}

// SampleReceipt is a made-up sale priced with an outlet's real settings, so the POS hardware spike
// can print something that looks like a real receipt: the outlet's header and footer, its tax and
// service charge, and its cash rounding. It is a preview of BACKEND_PLAN.md section 4.8 without
// discounts or modifiers.
type SampleReceipt struct {
	Lines         []SampleLine
	Subtotal      kernel.Rupiah
	ServiceCharge kernel.Rupiah
	Tax           kernel.Rupiah
	TaxIncluded   bool // the tax is already inside the prices and is shown, not added
	Total         kernel.Rupiah
	// RoundingAmount is the cash rounding, applied to a cash tender only; CashTotal is
	// Total + RoundingAmount.
	RoundingAmount kernel.Rupiah
	CashTotal      kernel.Rupiah
}

var sampleLines = []SampleLine{
	{Name: "Es Kopi Susu", Quantity: 2, UnitPrice: 24_000},
	{Name: "Croissant", Quantity: 1, UnitPrice: 28_500},
	{Name: "Air Mineral", Quantity: 3, UnitPrice: 6_000},
}

// NewSampleReceipt prices the sample sale under the given settings, with the same calculation that
// prices every real sale (internal/pricing), paid in cash so the rounding shows.
func NewSampleReceipt(s OutletSettings) (SampleReceipt, error) {
	bill := pricing.Bill{
		Settings: pricing.Settings{
			PriceIncludesTax:     s.PriceIncludesTax,
			TaxRate:              s.TaxRate,
			ServiceChargeRate:    s.ServiceChargeRate,
			ServiceChargeTaxable: s.ServiceChargeTaxable,
			CashRoundingUnit:     s.CashRoundingUnit,
			CashRoundingMode:     pricing.RoundMode(s.CashRoundingMode),
		},
		Tender: pricing.TenderCash,
	}
	for _, l := range sampleLines {
		bill.Lines = append(bill.Lines, pricing.Line{UnitPrice: l.UnitPrice, Quantity: l.Quantity})
	}
	res, err := pricing.Calculate(bill)
	if err != nil {
		return SampleReceipt{}, fmt.Errorf("pricing the sample sale: %w", err)
	}

	r := SampleReceipt{
		Subtotal: res.Subtotal, ServiceCharge: res.ServiceCharge, Tax: res.Tax, TaxIncluded: res.TaxIncluded,
		Total: res.Total, RoundingAmount: res.RoundingAmount, CashTotal: res.CashTotal,
	}
	for i, l := range sampleLines {
		l.Amount = res.Lines[i].Gross
		r.Lines = append(r.Lines, l)
	}
	return r, nil
}
