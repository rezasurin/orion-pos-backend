package tenancy

import (
	"fmt"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
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
// discounts or modifiers; the real bill calculation arrives with sales in Phase 1.
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

// NewSampleReceipt prices the sample sale under the given settings.
func NewSampleReceipt(s OutletSettings) (SampleReceipt, error) {
	r := SampleReceipt{TaxIncluded: s.PriceIncludesTax}
	for _, l := range sampleLines {
		l.Amount = l.UnitPrice * kernel.Rupiah(l.Quantity)
		r.Lines = append(r.Lines, l)
		r.Subtotal += l.Amount
	}

	var err error
	if r.ServiceCharge, err = kernel.ApplyRate(r.Subtotal, s.ServiceChargeRate, kernel.RoundHalfUp); err != nil {
		return SampleReceipt{}, err
	}
	taxBase := r.Subtotal
	if s.ServiceChargeTaxable {
		taxBase += r.ServiceCharge
	}
	if s.PriceIncludesTax {
		r.Tax, err = kernel.ExtractRate(taxBase, s.TaxRate, kernel.RoundHalfUp)
		r.Total = r.Subtotal + r.ServiceCharge
	} else {
		r.Tax, err = kernel.ApplyRate(taxBase, s.TaxRate, kernel.RoundHalfUp)
		r.Total = r.Subtotal + r.ServiceCharge + r.Tax
	}
	if err != nil {
		return SampleReceipt{}, err
	}

	mode := map[string]kernel.RoundingMode{"nearest": kernel.RoundHalfUp, "down": kernel.RoundFloor, "up": kernel.RoundCeil}[s.CashRoundingMode]
	rounded, err := kernel.RoundToUnit(r.Total, s.CashRoundingUnit, mode)
	if err != nil {
		return SampleReceipt{}, fmt.Errorf("cash rounding: %w", err)
	}
	r.CashTotal, r.RoundingAmount = rounded, rounded-r.Total
	return r, nil
}
