package kernel

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestApplyRate(t *testing.T) {
	tests := []struct {
		name   string
		amount Rupiah
		rate   BasisPoints
		mode   RoundingMode
		want   Rupiah
	}{
		{"10% exact", 25000, 1000, RoundHalfUp, 2500},
		{"10% half rounds up", 12345, 1000, RoundHalfUp, 1235}, // 1234.5
		{"10% below half", 12344, 1000, RoundHalfUp, 1234},     // 1234.4
		{"5% service", 33333, 500, RoundHalfUp, 1667},          // 1666.65
		{"floor", 12349, 1000, RoundFloor, 1234},               // 1234.9
		{"ceil", 12341, 1000, RoundCeil, 1235},                 // 1234.1
		{"negative half away from zero", -12345, 1000, RoundHalfUp, -1235},
		{"negative floor", -12341, 1000, RoundFloor, -1235},
		{"negative ceil", -12349, 1000, RoundCeil, -1234},
		{"zero rate", 50000, 0, RoundHalfUp, 0},
		{"large amount", 900_000_000_000_000, 1000, RoundHalfUp, 90_000_000_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyRate(tt.amount, tt.rate, tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("ApplyRate(%d, %d) = %d, want %d", tt.amount, tt.rate, got, tt.want)
			}
		})
	}
}

func TestApplyRateOverflow(t *testing.T) {
	if _, err := ApplyRate(math.MaxInt64, 20000, RoundHalfUp); !errors.Is(err, ErrOverflow) {
		t.Errorf("err = %v, want ErrOverflow", err)
	}
}

func TestRoundToUnit(t *testing.T) {
	tests := []struct {
		amount, unit Rupiah
		mode         RoundingMode
		want         Rupiah
	}{
		{27_450, 100, RoundHalfUp, 27_500},
		{27_449, 100, RoundHalfUp, 27_400},
		{27_450, 500, RoundHalfUp, 27_500},
		{27_249, 500, RoundHalfUp, 27_000},
		{27_250, 500, RoundHalfUp, 27_500},
		{27_999, 1000, RoundFloor, 27_000},
		{27_001, 1000, RoundCeil, 28_000},
		{27_000, 1000, RoundCeil, 27_000},
		{-27_450, 100, RoundHalfUp, -27_500},
		{27_450, 0, RoundHalfUp, 27_450},
		{27_450, 1, RoundHalfUp, 27_450},
	}
	for _, tt := range tests {
		got, err := RoundToUnit(tt.amount, tt.unit, tt.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.want {
			t.Errorf("RoundToUnit(%d, %d, %d) = %d, want %d", tt.amount, tt.unit, tt.mode, got, tt.want)
		}
	}
	if _, err := RoundToUnit(100, -1, RoundHalfUp); err == nil {
		t.Error("negative unit: want error")
	}
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name    string
		total   Rupiah
		weights []Rupiah
		want    []Rupiah
	}{
		{"even", 300, []Rupiah{100, 100, 100}, []Rupiah{100, 100, 100}},
		{"remainder to first on tie", 100, []Rupiah{1, 1, 1}, []Rupiah{34, 33, 33}},
		{"largest remainder wins", 10000, []Rupiah{18000, 7000, 5000}, []Rupiah{6000, 2333, 1667}},
		{"zero weight gets nothing", 1000, []Rupiah{0, 25000, 25000}, []Rupiah{0, 500, 500}},
		{"zero total", 0, []Rupiah{5, 7}, []Rupiah{0, 0}},
		{"zero total zero weights", 0, []Rupiah{0, 0}, []Rupiah{0, 0}},
		{"empty", 0, nil, []Rupiah{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Allocate(tt.total, tt.weights)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Allocate(%d, %v) = %v, want %v", tt.total, tt.weights, got, tt.want)
			}
		})
	}
}

func TestAllocateAlwaysSumsToTotal(t *testing.T) {
	weights := []Rupiah{18_500, 22_000, 9_900, 1, 35_000, 12_345}
	for total := Rupiah(0); total < 20_000; total += 37 {
		parts, err := Allocate(total, weights)
		if err != nil {
			t.Fatal(err)
		}
		var sum Rupiah
		for _, p := range parts {
			sum += p
		}
		if sum != total {
			t.Fatalf("Allocate(%d) parts %v sum to %d", total, parts, sum)
		}
	}
}

func TestAllocateErrors(t *testing.T) {
	if _, err := Allocate(-1, []Rupiah{1}); err == nil {
		t.Error("negative total: want error")
	}
	if _, err := Allocate(10, []Rupiah{1, -1}); err == nil {
		t.Error("negative weight: want error")
	}
	if _, err := Allocate(10, []Rupiah{0, 0}); err == nil {
		t.Error("non-zero total over zero weights: want error")
	}
}

func TestExtractRate(t *testing.T) {
	tests := []struct {
		gross Rupiah
		rate  BasisPoints
		want  Rupiah
	}{
		{11_100, 1000, 1_009},
		{11_000, 1000, 1_000},
		{111_000, 1100, 11_000},
		{100_000, 0, 0},
		{10_500, 1000, 955}, // 954.545... rounds half up
	}
	for _, tt := range tests {
		got, err := ExtractRate(tt.gross, tt.rate, RoundHalfUp)
		if err != nil || got != tt.want {
			t.Errorf("ExtractRate(%d, %d) = %d, %v; want %d", tt.gross, tt.rate, got, err, tt.want)
		}
	}
}
