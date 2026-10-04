// Package pricing is the one written bill calculation (BACKEND_PLAN.md section 4.8). The POS
// implements the same algorithm in TypeScript, and both must pass the golden vectors in
// testdata/pricing-vectors, so the server can recompute a sale that arrives from a device and flag
// it when the totals differ.
//
// The algorithm is versioned (Version, stored on each sale as pricing_version): changing any
// result of any bill means a new version, never an edit, so old sales keep their meaning.
//
// Only integer arithmetic is used. Rupiah amounts are whole rupiah, rates are basis points, and
// rounding goes through kernel.ApplyRate, ExtractRate, Allocate and RoundToUnit.
//
// The order, version 1:
//
//  1. line amount = (unit price + modifier deltas) x quantity
//  2. line discount (at most one per line): a percent of the line amount, rounded half up, or a
//     fixed amount that may not exceed the line amount
//  3. bill discount (at most one): a percent of the sum of the discounted lines, rounded half up,
//     or a fixed amount that may not exceed that sum; spread over the lines in proportion to their
//     discounted amounts by largest remainder (ties to the earlier line), so the parts add up
//     exactly
//  4. service charge = rate x the discounted subtotal, rounded half up, once per bill
//  5. tax: on the discounted subtotal plus the service charge when that is taxable. With prices
//     excluding tax it is rate x base rounded half up and is added; with prices including tax it
//     is the part of base that is tax, base x rate / (1 + rate) rounded half up, and is only shown
//  6. total = discounted subtotal + service charge (+ tax when prices exclude it)
//  7. cash rounding moves the total to a multiple of the rounding unit, but only when the whole
//     bill is paid in cash. It is reported as its own rounding_amount, so the total still
//     reconciles. Bills paid by card or QRIS, or by a mix, are not rounded.
package pricing

import (
	"errors"
	"fmt"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
)

// Version is the algorithm version recorded on every sale.
const Version = 1

// Limits keep every intermediate sum far inside int64.
const (
	MaxLines            = 200
	MaxModifiersPerLine = 50
	MaxQuantity         = 10_000
	MaxAmount           = kernel.Rupiah(1_000_000_000) // a unit price, or the size of one modifier delta
	maxRateBasisPoints  = 10_000
	maxCashRoundingUnit = 1_000_000
	percentBasisPoints  = 10_000
)

// RoundMode is how cash rounding moves a total that is not a multiple of the unit.
type RoundMode string

const (
	RoundNearest RoundMode = "nearest" // halves go up
	RoundDown    RoundMode = "down"
	RoundUp      RoundMode = "up"
)

// Tender says how the bill is paid, which decides whether cash rounding applies.
type Tender string

const (
	TenderCash    Tender = "cash"     // every payment is cash: cash rounding applies
	TenderNonCash Tender = "non_cash" // card, QRIS, e-wallet: no rounding
	TenderMixed   Tender = "mixed"    // cash together with something else: no rounding
)

// DiscountKind is a percent (Value in basis points) or a fixed amount (Value in rupiah).
type DiscountKind string

const (
	DiscountPercent DiscountKind = "percent"
	DiscountAmount  DiscountKind = "amount"
)

// Settings are the outlet's tax, service charge and rounding settings (outlet_settings).
type Settings struct {
	PriceIncludesTax     bool
	TaxRate              kernel.BasisPoints
	ServiceChargeRate    kernel.BasisPoints
	ServiceChargeTaxable bool
	CashRoundingUnit     kernel.Rupiah // 0 or 1 means no rounding
	CashRoundingMode     RoundMode
}

// Line is one line of the bill.
type Line struct {
	UnitPrice      kernel.Rupiah
	ModifierDeltas []kernel.Rupiah // each chosen modifier's price delta, which may be negative
	Quantity       int64
}

// Discount applies to one line (Line set to its index) or, with Line nil, to the whole bill.
type Discount struct {
	Line  *int
	Kind  DiscountKind
	Value int64 // basis points for a percent, rupiah for an amount
}

// Bill is everything the calculation needs.
type Bill struct {
	Settings  Settings
	Lines     []Line
	Discounts []Discount
	Tender    Tender
}

// LineResult is what one line comes to.
type LineResult struct {
	Gross                 kernel.Rupiah // (unit price + modifiers) x quantity
	Discount              kernel.Rupiah // its own line discount
	AllocatedBillDiscount kernel.Rupiah // its share of the bill discount
	Total                 kernel.Rupiah // Gross - Discount - AllocatedBillDiscount
}

// DiscountResult is the amount a discount came to, in the order the discounts were given.
type DiscountResult struct {
	Amount kernel.Rupiah
}

// Result is the priced bill.
type Result struct {
	Version   int
	Lines     []LineResult
	Discounts []DiscountResult

	Subtotal      kernel.Rupiah // sum of line Gross, before any discount
	DiscountTotal kernel.Rupiah // line discounts plus the bill discount
	Net           kernel.Rupiah // Subtotal - DiscountTotal: what service charge and tax work from
	ServiceCharge kernel.Rupiah
	Tax           kernel.Rupiah
	TaxIncluded   bool          // Tax is inside the prices and shown, not added
	Total         kernel.Rupiah // what the bill comes to, before cash rounding
	// RoundingAmount is the cash rounding, positive or negative, and zero unless the bill is paid
	// in cash. CashTotal is Total + RoundingAmount: what the customer hands over.
	RoundingAmount kernel.Rupiah
	CashTotal      kernel.Rupiah
}

// ErrInvalid is wrapped by every *Error: the bill cannot be priced.
var ErrInvalid = errors.New("pricing: invalid bill")

// Error says why a bill cannot be priced, with a stable code (the golden vectors use them).
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("pricing: %s: %s", e.Code, e.Detail) }
func (e *Error) Unwrap() error { return ErrInvalid }

func invalid(code, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// Calculate prices a bill under algorithm version 1.
func Calculate(b Bill) (Result, error) {
	if err := validate(b); err != nil {
		return Result{}, err
	}
	res := Result{Version: Version, Lines: make([]LineResult, len(b.Lines)), Discounts: make([]DiscountResult, len(b.Discounts))}

	// 1. line amounts.
	for i, l := range b.Lines {
		unit := l.UnitPrice
		for _, d := range l.ModifierDeltas {
			unit += d
		}
		if unit < 0 {
			return Result{}, invalid("negative_price", "line %d: modifiers take the price below zero", i)
		}
		res.Lines[i].Gross = unit * kernel.Rupiah(l.Quantity)
		res.Subtotal += res.Lines[i].Gross
	}

	// 2. line discounts.
	var billDiscount *int
	for di, d := range b.Discounts {
		if d.Line == nil {
			billDiscount = &di
			continue
		}
		li := *d.Line
		amount, err := discountAmount(d, res.Lines[li].Gross)
		if err != nil {
			return Result{}, fmt.Errorf("line %d: %w", li, err)
		}
		res.Lines[li].Discount = amount
		res.Discounts[di].Amount = amount
		res.DiscountTotal += amount
	}
	afterLines := make([]kernel.Rupiah, len(b.Lines))
	var linesNet kernel.Rupiah
	for i := range res.Lines {
		afterLines[i] = res.Lines[i].Gross - res.Lines[i].Discount
		linesNet += afterLines[i]
	}

	// 3. bill discount, spread over the lines.
	var billAmount kernel.Rupiah
	if billDiscount != nil {
		var err error
		billAmount, err = discountAmount(b.Discounts[*billDiscount], linesNet)
		if err != nil {
			return Result{}, fmt.Errorf("bill discount: %w", err)
		}
		res.Discounts[*billDiscount].Amount = billAmount
		res.DiscountTotal += billAmount
	}
	shares, err := kernel.Allocate(billAmount, afterLines)
	if err != nil {
		return Result{}, err
	}
	for i := range res.Lines {
		res.Lines[i].AllocatedBillDiscount = shares[i]
		res.Lines[i].Total = afterLines[i] - shares[i]
	}
	res.Net = res.Subtotal - res.DiscountTotal

	// 4. service charge.
	s := b.Settings
	if res.ServiceCharge, err = kernel.ApplyRate(res.Net, s.ServiceChargeRate, kernel.RoundHalfUp); err != nil {
		return Result{}, err
	}

	// 5 and 6. tax and total.
	taxBase := res.Net
	if s.ServiceChargeTaxable {
		taxBase += res.ServiceCharge
	}
	res.TaxIncluded = s.PriceIncludesTax
	if s.PriceIncludesTax {
		res.Tax, err = kernel.ExtractRate(taxBase, s.TaxRate, kernel.RoundHalfUp)
		res.Total = res.Net + res.ServiceCharge
	} else {
		res.Tax, err = kernel.ApplyRate(taxBase, s.TaxRate, kernel.RoundHalfUp)
		res.Total = res.Net + res.ServiceCharge + res.Tax
	}
	if err != nil {
		return Result{}, err
	}

	// 7. cash rounding.
	res.CashTotal = res.Total
	if b.Tender == TenderCash {
		rounded, err := kernel.RoundToUnit(res.Total, s.CashRoundingUnit, roundingMode(s.CashRoundingMode))
		if err != nil {
			return Result{}, err
		}
		res.CashTotal, res.RoundingAmount = rounded, rounded-res.Total
	}
	return res, nil
}

// discountAmount is what a discount comes to on a base amount, and an error if it is out of range.
func discountAmount(d Discount, base kernel.Rupiah) (kernel.Rupiah, error) {
	switch d.Kind {
	case DiscountPercent:
		if d.Value < 0 || d.Value > percentBasisPoints {
			return 0, invalid("invalid_discount", "a percent discount is 0 to 10000 basis points, got %d", d.Value)
		}
		return kernel.ApplyRate(base, kernel.BasisPoints(d.Value), kernel.RoundHalfUp)
	case DiscountAmount:
		if d.Value < 0 {
			return 0, invalid("invalid_discount", "a discount amount cannot be negative, got %d", d.Value)
		}
		if kernel.Rupiah(d.Value) > base {
			return 0, invalid("discount_exceeds_amount", "a discount of %d is more than the %d it applies to", d.Value, base)
		}
		return kernel.Rupiah(d.Value), nil
	}
	return 0, invalid("invalid_discount", "unknown discount kind %q", d.Kind)
}

func roundingMode(m RoundMode) kernel.RoundingMode {
	switch m {
	case RoundDown:
		return kernel.RoundFloor
	case RoundUp:
		return kernel.RoundCeil
	}
	return kernel.RoundHalfUp
}

func validate(b Bill) error {
	s := b.Settings
	switch {
	case s.TaxRate < 0 || s.TaxRate > maxRateBasisPoints:
		return invalid("invalid_settings", "tax rate must be 0 to %d basis points", maxRateBasisPoints)
	case s.ServiceChargeRate < 0 || s.ServiceChargeRate > maxRateBasisPoints:
		return invalid("invalid_settings", "service charge rate must be 0 to %d basis points", maxRateBasisPoints)
	case s.CashRoundingUnit < 0 || s.CashRoundingUnit > maxCashRoundingUnit:
		return invalid("invalid_settings", "cash rounding unit must be 0 to %d", maxCashRoundingUnit)
	case s.CashRoundingMode != "" && s.CashRoundingMode != RoundNearest && s.CashRoundingMode != RoundDown && s.CashRoundingMode != RoundUp:
		return invalid("invalid_settings", "unknown cash rounding mode %q", s.CashRoundingMode)
	}
	switch b.Tender {
	case TenderCash, TenderNonCash, TenderMixed:
	default:
		return invalid("invalid_tender", "unknown tender %q", b.Tender)
	}
	if len(b.Lines) == 0 {
		return invalid("no_lines", "a bill needs at least one line")
	}
	if len(b.Lines) > MaxLines {
		return invalid("too_large", "a bill has at most %d lines", MaxLines)
	}
	for i, l := range b.Lines {
		switch {
		case l.Quantity < 1 || l.Quantity > MaxQuantity:
			return invalid("invalid_quantity", "line %d: quantity must be 1 to %d", i, MaxQuantity)
		case l.UnitPrice < 0 || l.UnitPrice > MaxAmount:
			return invalid("invalid_price", "line %d: unit price must be 0 to %d", i, MaxAmount)
		case len(l.ModifierDeltas) > MaxModifiersPerLine:
			return invalid("too_large", "line %d: at most %d modifiers", i, MaxModifiersPerLine)
		}
		for _, d := range l.ModifierDeltas {
			if d < -MaxAmount || d > MaxAmount {
				return invalid("invalid_price", "line %d: a modifier delta must be %d to %d", i, -MaxAmount, MaxAmount)
			}
		}
	}
	seenLine := map[int]bool{}
	seenBill := false
	for _, d := range b.Discounts {
		if d.Line == nil {
			if seenBill {
				return invalid("duplicate_discount", "a bill has at most one bill discount")
			}
			seenBill = true
			continue
		}
		if *d.Line < 0 || *d.Line >= len(b.Lines) {
			return invalid("invalid_discount", "discount refers to line %d, which does not exist", *d.Line)
		}
		if seenLine[*d.Line] {
			return invalid("duplicate_discount", "line %d has more than one discount", *d.Line)
		}
		seenLine[*d.Line] = true
	}
	return nil
}
