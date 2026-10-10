package pricing_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/rezasurin/orion-pos-backend/internal/kernel"
	"github.com/rezasurin/orion-pos-backend/internal/pricing"
)

// vector is one file of testdata/pricing-vectors. The format is shared with the TypeScript
// implementation, which runs the same files (see that directory's README).
type vector struct {
	Name     string `json:"name"`
	Note     string `json:"note"`
	Settings struct {
		PriceIncludesTax     bool   `json:"price_includes_tax"`
		TaxRateBP            int64  `json:"tax_rate_bp"`
		ServiceChargeRateBP  int64  `json:"service_charge_rate_bp"`
		ServiceChargeTaxable bool   `json:"service_charge_taxable"`
		CashRoundingUnit     int64  `json:"cash_rounding_unit"`
		CashRoundingMode     string `json:"cash_rounding_mode"`
	} `json:"settings"`
	Lines []struct {
		UnitPrice int64   `json:"unit_price"`
		Modifiers []int64 `json:"modifiers"`
		Quantity  int64   `json:"quantity"`
	} `json:"lines"`
	Discounts []struct {
		Line  *int   `json:"line"`
		Kind  string `json:"kind"`
		Value int64  `json:"value"`
	} `json:"discounts"`
	Tender        string    `json:"tender"`
	Expected      *expected `json:"expected"`
	ExpectedError string    `json:"expected_error"`
}

type expected struct {
	Lines []struct {
		Gross                 int64 `json:"gross"`
		Discount              int64 `json:"discount"`
		AllocatedBillDiscount int64 `json:"allocated_bill_discount"`
		Total                 int64 `json:"total"`
	} `json:"lines"`
	Subtotal       int64 `json:"subtotal"`
	DiscountTotal  int64 `json:"discount_total"`
	Net            int64 `json:"net"`
	ServiceCharge  int64 `json:"service_charge"`
	Tax            int64 `json:"tax"`
	TaxIncluded    bool  `json:"tax_included"`
	Total          int64 `json:"total"`
	RoundingAmount int64 `json:"rounding_amount"`
	CashTotal      int64 `json:"cash_total"`
}

func (v vector) bill() pricing.Bill {
	b := pricing.Bill{
		Settings: pricing.Settings{
			PriceIncludesTax:     v.Settings.PriceIncludesTax,
			TaxRate:              kernel.BasisPoints(v.Settings.TaxRateBP),
			ServiceChargeRate:    kernel.BasisPoints(v.Settings.ServiceChargeRateBP),
			ServiceChargeTaxable: v.Settings.ServiceChargeTaxable,
			CashRoundingUnit:     kernel.Rupiah(v.Settings.CashRoundingUnit),
			CashRoundingMode:     pricing.RoundMode(v.Settings.CashRoundingMode),
		},
		Tender: pricing.Tender(v.Tender),
	}
	for _, l := range v.Lines {
		line := pricing.Line{UnitPrice: kernel.Rupiah(l.UnitPrice), Quantity: l.Quantity}
		for _, m := range l.Modifiers {
			line.ModifierDeltas = append(line.ModifierDeltas, kernel.Rupiah(m))
		}
		b.Lines = append(b.Lines, line)
	}
	for _, d := range v.Discounts {
		b.Discounts = append(b.Discounts, pricing.Discount{Line: d.Line, Kind: pricing.DiscountKind(d.Kind), Value: d.Value})
	}
	return b
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	files, err := filepath.Glob("../../testdata/pricing-vectors/*.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []vector
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields() // a vector with a field Go ignores would be a vector Go does not test
		var v vector
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if (v.Expected == nil) == (v.ExpectedError == "") {
			t.Fatalf("%s: exactly one of expected and expected_error is required", f)
		}
		out = append(out, v)
	}
	return out
}

func TestGoldenVectors(t *testing.T) {
	vectors := loadVectors(t)
	if len(vectors) < 40 {
		t.Fatalf("only %d vectors found, the plan asks for about 40", len(vectors))
	}
	names := map[string]bool{}
	for _, v := range vectors {
		if names[v.Name] {
			t.Errorf("duplicate vector name %q", v.Name)
		}
		names[v.Name] = true
		t.Run(v.Name, func(t *testing.T) {
			got, err := pricing.Calculate(v.bill())
			if v.ExpectedError != "" {
				var perr *pricing.Error
				if !errors.As(err, &perr) {
					t.Fatalf("err = %v, want error code %q", err, v.ExpectedError)
				}
				if perr.Code != v.ExpectedError {
					t.Errorf("error code = %q, want %q (%v)", perr.Code, v.ExpectedError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			e := v.Expected
			if got.Version != pricing.Version {
				t.Errorf("version = %d", got.Version)
			}
			if len(got.Lines) != len(e.Lines) {
				t.Fatalf("%d lines, want %d", len(got.Lines), len(e.Lines))
			}
			for i, l := range got.Lines {
				w := e.Lines[i]
				if int64(l.Gross) != w.Gross || int64(l.Discount) != w.Discount || int64(l.AllocatedBillDiscount) != w.AllocatedBillDiscount || int64(l.Total) != w.Total {
					t.Errorf("line %d = %+v, want %+v", i, l, w)
				}
			}
			checks := []struct {
				name      string
				got, want int64
			}{
				{"subtotal", int64(got.Subtotal), e.Subtotal}, {"discount_total", int64(got.DiscountTotal), e.DiscountTotal},
				{"net", int64(got.Net), e.Net}, {"service_charge", int64(got.ServiceCharge), e.ServiceCharge},
				{"tax", int64(got.Tax), e.Tax}, {"total", int64(got.Total), e.Total},
				{"rounding_amount", int64(got.RoundingAmount), e.RoundingAmount}, {"cash_total", int64(got.CashTotal), e.CashTotal},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
				}
			}
			if got.TaxIncluded != e.TaxIncluded {
				t.Errorf("tax_included = %v", got.TaxIncluded)
			}
		})
	}
}

// Whatever the bill, the parts add up: the property the vectors cannot cover exhaustively.
func TestRandomBillsReconcile(t *testing.T) {
	rng := rand.New(rand.NewSource(20261004))
	modes := []pricing.RoundMode{pricing.RoundNearest, pricing.RoundDown, pricing.RoundUp}
	tenders := []pricing.Tender{pricing.TenderCash, pricing.TenderNonCash, pricing.TenderMixed}
	units := []kernel.Rupiah{0, 1, 100, 500, 1000}

	for n := 0; n < 3000; n++ {
		b := pricing.Bill{
			Settings: pricing.Settings{
				PriceIncludesTax:     rng.Intn(2) == 0,
				TaxRate:              kernel.BasisPoints(rng.Intn(1201)),
				ServiceChargeRate:    kernel.BasisPoints(rng.Intn(1001)),
				ServiceChargeTaxable: rng.Intn(2) == 0,
				CashRoundingUnit:     units[rng.Intn(len(units))],
				CashRoundingMode:     modes[rng.Intn(len(modes))],
			},
			Tender: tenders[rng.Intn(len(tenders))],
		}
		lines := 1 + rng.Intn(12)
		for i := 0; i < lines; i++ {
			l := pricing.Line{UnitPrice: kernel.Rupiah(rng.Intn(200_000)), Quantity: int64(1 + rng.Intn(9))}
			for m := rng.Intn(3); m > 0; m-- {
				l.ModifierDeltas = append(l.ModifierDeltas, kernel.Rupiah(rng.Intn(10_000)))
			}
			b.Lines = append(b.Lines, l)
			if rng.Intn(4) == 0 {
				i := i
				if rng.Intn(2) == 0 {
					b.Discounts = append(b.Discounts, pricing.Discount{Line: &i, Kind: pricing.DiscountPercent, Value: int64(rng.Intn(10_001))})
				} else {
					b.Discounts = append(b.Discounts, pricing.Discount{Line: &i, Kind: pricing.DiscountAmount, Value: 0})
				}
			}
		}
		if rng.Intn(2) == 0 {
			b.Discounts = append(b.Discounts, pricing.Discount{Kind: pricing.DiscountPercent, Value: int64(rng.Intn(10_001))})
		}

		r, err := pricing.Calculate(b)
		if err != nil {
			t.Fatalf("bill %d: %v", n, err)
		}
		var gross, lineDisc, alloc, lineTotals kernel.Rupiah
		for _, l := range r.Lines {
			gross += l.Gross
			lineDisc += l.Discount
			alloc += l.AllocatedBillDiscount
			lineTotals += l.Total
			if l.Total < 0 || l.Total != l.Gross-l.Discount-l.AllocatedBillDiscount {
				t.Fatalf("bill %d: line %+v does not add up", n, l)
			}
		}
		switch {
		case r.Subtotal != gross:
			t.Fatalf("bill %d: subtotal %d != sum of gross %d", n, r.Subtotal, gross)
		case r.DiscountTotal != lineDisc+alloc:
			t.Fatalf("bill %d: discount_total %d != %d + %d", n, r.DiscountTotal, lineDisc, alloc)
		case r.Net != lineTotals || r.Net != r.Subtotal-r.DiscountTotal:
			t.Fatalf("bill %d: net %d, line totals %d", n, r.Net, lineTotals)
		case b.Settings.PriceIncludesTax && r.Total != r.Net+r.ServiceCharge:
			t.Fatalf("bill %d: inclusive total %d", n, r.Total)
		case !b.Settings.PriceIncludesTax && r.Total != r.Net+r.ServiceCharge+r.Tax:
			t.Fatalf("bill %d: exclusive total %d", n, r.Total)
		case r.CashTotal != r.Total+r.RoundingAmount:
			t.Fatalf("bill %d: cash total %d != %d + %d", n, r.CashTotal, r.Total, r.RoundingAmount)
		case b.Tender != pricing.TenderCash && r.RoundingAmount != 0:
			t.Fatalf("bill %d: a %s bill was rounded", n, b.Tender)
		}
		if u := b.Settings.CashRoundingUnit; b.Tender == pricing.TenderCash && u > 1 {
			if r.CashTotal%u != 0 {
				t.Fatalf("bill %d: cash total %d is not a multiple of %d", n, r.CashTotal, u)
			}
			if d := r.RoundingAmount; d <= -u || d >= u {
				t.Fatalf("bill %d: rounding %d moved the total by a whole unit %d", n, d, u)
			}
		}
	}
}

func TestCalculateIsPure(t *testing.T) {
	line := 0
	b := pricing.Bill{
		Settings:  pricing.Settings{TaxRate: 1100, ServiceChargeRate: 500, ServiceChargeTaxable: true},
		Lines:     []pricing.Line{{UnitPrice: 20000, ModifierDeltas: []kernel.Rupiah{1000}, Quantity: 2}},
		Discounts: []pricing.Discount{{Line: &line, Kind: pricing.DiscountPercent, Value: 1000}, {Kind: pricing.DiscountAmount, Value: 500}},
		Tender:    pricing.TenderNonCash,
	}
	first, err := pricing.Calculate(b)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := pricing.Calculate(b)
	if first.Total != second.Total || first.Lines[0] != second.Lines[0] {
		t.Errorf("two calls disagree: %+v vs %+v", first, second)
	}
	if b.Lines[0].ModifierDeltas[0] != 1000 || *b.Discounts[0].Line != 0 {
		t.Error("Calculate changed its input")
	}
	// The discount amounts come back in the order given.
	if len(first.Discounts) != 2 || first.Discounts[0].Amount != 4200 || first.Discounts[1].Amount != 500 {
		t.Errorf("discount amounts = %+v", first.Discounts)
	}
}

func TestInvalidSettingsAndLimits(t *testing.T) {
	ok := func() pricing.Bill {
		return pricing.Bill{Lines: []pricing.Line{{UnitPrice: 1000, Quantity: 1}}, Tender: pricing.TenderNonCash}
	}
	tooMany := ok()
	tooMany.Lines = make([]pricing.Line, pricing.MaxLines+1)
	for i := range tooMany.Lines {
		tooMany.Lines[i] = pricing.Line{UnitPrice: 1, Quantity: 1}
	}
	cases := map[string]func(*pricing.Bill){
		"tax rate over 100%":      func(b *pricing.Bill) { b.Settings.TaxRate = 10_001 },
		"negative service charge": func(b *pricing.Bill) { b.Settings.ServiceChargeRate = -1 },
		"unknown rounding mode":   func(b *pricing.Bill) { b.Settings.CashRoundingMode = "sideways" },
		"unknown tender":          func(b *pricing.Bill) { b.Tender = "barter" },
		"no lines":                func(b *pricing.Bill) { b.Lines = nil },
		"quantity too large":      func(b *pricing.Bill) { b.Lines[0].Quantity = pricing.MaxQuantity + 1 },
		"price too large":         func(b *pricing.Bill) { b.Lines[0].UnitPrice = pricing.MaxAmount + 1 },
		"negative price":          func(b *pricing.Bill) { b.Lines[0].UnitPrice = -1 },
		"discount on no line": func(b *pricing.Bill) {
			five := 5
			b.Discounts = []pricing.Discount{{Line: &five, Kind: pricing.DiscountAmount, Value: 1}}
		},
		"two bill discounts": func(b *pricing.Bill) {
			b.Discounts = []pricing.Discount{{Kind: pricing.DiscountAmount, Value: 1}, {Kind: pricing.DiscountAmount, Value: 1}}
		},
		"unknown discount kind": func(b *pricing.Bill) { b.Discounts = []pricing.Discount{{Kind: "coupon", Value: 1}} },
		"negative discount":     func(b *pricing.Bill) { b.Discounts = []pricing.Discount{{Kind: pricing.DiscountAmount, Value: -1}} },
		"too many lines":        func(b *pricing.Bill) { *b = tooMany },
	}
	for name, mutate := range cases {
		b := ok()
		mutate(&b)
		if _, err := pricing.Calculate(b); !errors.Is(err, pricing.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// An empty rounding mode means nearest.
	b := ok()
	b.Tender, b.Settings.CashRoundingUnit = pricing.TenderCash, 500
	b.Lines[0].UnitPrice = 1250
	if r, err := pricing.Calculate(b); err != nil || r.CashTotal != 1500 {
		t.Errorf("default rounding mode: %+v, %v", r, err)
	}
}
