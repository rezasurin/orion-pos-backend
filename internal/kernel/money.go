package kernel

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Rupiah is an amount of money in whole rupiah (ADR 0006). Money never touches floating point.
type Rupiah int64

// BasisPoints is a rate in hundredths of a percent: 1000 is 10.00%.
type BasisPoints int64

// RoundingMode says which way a result that falls between two whole values goes.
type RoundingMode int

const (
	// RoundHalfUp rounds to the nearest value, with halves going away from zero, so a refund
	// rounds the same way as the sale it reverses.
	RoundHalfUp RoundingMode = iota
	// RoundFloor rounds towards negative infinity.
	RoundFloor
	// RoundCeil rounds towards positive infinity.
	RoundCeil
)

// ErrOverflow is returned when a result does not fit in an int64.
var ErrOverflow = errors.New("kernel: money overflow")

// ApplyRate returns amount × rate / 10000, rounded with mode. Tax and service charge use it.
func ApplyRate(amount Rupiah, rate BasisPoints, mode RoundingMode) (Rupiah, error) {
	n := new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(int64(rate)))
	return divRound(n, big.NewInt(10000), mode)
}

// ExtractRate returns the tax contained in a tax-inclusive amount: gross × rate / (10000 + rate),
// rounded with mode. A gross of 11,100 at 11% contains 1,100.
func ExtractRate(gross Rupiah, rate BasisPoints, mode RoundingMode) (Rupiah, error) {
	n := new(big.Int).Mul(big.NewInt(int64(gross)), big.NewInt(int64(rate)))
	return divRound(n, big.NewInt(10000+int64(rate)), mode)
}

// RoundToUnit rounds amount to a multiple of unit, for cash rounding (pembulatan). A unit of 0 or
// 1 leaves the amount unchanged.
func RoundToUnit(amount, unit Rupiah, mode RoundingMode) (Rupiah, error) {
	if unit < 0 {
		return 0, fmt.Errorf("kernel: negative rounding unit %d", unit)
	}
	if unit <= 1 {
		return amount, nil
	}
	q, err := divRound(big.NewInt(int64(amount)), big.NewInt(int64(unit)), mode)
	if err != nil {
		return 0, err
	}
	n := new(big.Int).Mul(big.NewInt(int64(q)), big.NewInt(int64(unit)))
	if !n.IsInt64() {
		return 0, ErrOverflow
	}
	return Rupiah(n.Int64()), nil
}

// Allocate splits total into parts proportional to weights, using the largest-remainder method so
// the parts add up to total exactly. Ties go to the earlier weight. A bill discount is spread
// over its lines this way.
func Allocate(total Rupiah, weights []Rupiah) ([]Rupiah, error) {
	if total < 0 {
		return nil, fmt.Errorf("kernel: cannot allocate negative total %d", total)
	}
	sum := new(big.Int)
	for i, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("kernel: negative weight %d at index %d", w, i)
		}
		sum.Add(sum, big.NewInt(int64(w)))
	}
	if sum.Sign() == 0 {
		if total == 0 {
			return make([]Rupiah, len(weights)), nil
		}
		return nil, errors.New("kernel: cannot allocate a non-zero total over zero weights")
	}

	parts := make([]Rupiah, len(weights))
	rems := make([]*big.Int, len(weights))
	allocated := Rupiah(0)
	for i, w := range weights {
		n := new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(int64(w)))
		q, r := new(big.Int).QuoRem(n, sum, new(big.Int))
		parts[i] = Rupiah(q.Int64()) // q <= total, so it fits
		rems[i] = r
		allocated += parts[i]
	}

	// Hand out what is left, one rupiah at a time, to the largest remainders.
	for left := total - allocated; left > 0; left-- {
		best := -1
		for i, r := range rems {
			if best == -1 || r.Cmp(rems[best]) > 0 {
				best = i
			}
		}
		parts[best]++
		rems[best] = new(big.Int) // each part gets at most one extra rupiah
	}
	return parts, nil
}

// divRound divides n by d (d > 0) and rounds with mode.
func divRound(n, d *big.Int, mode RoundingMode) (Rupiah, error) {
	if d.Sign() <= 0 {
		return 0, errors.New("kernel: divisor must be positive")
	}
	q, r := new(big.Int).QuoRem(n, d, new(big.Int)) // truncates towards zero
	if r.Sign() != 0 {
		switch mode {
		case RoundHalfUp:
			twice := new(big.Int).Mul(new(big.Int).Abs(r), big.NewInt(2))
			if twice.Cmp(d) >= 0 {
				q.Add(q, big.NewInt(int64(n.Sign())))
			}
		case RoundFloor:
			if n.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			}
		case RoundCeil:
			if n.Sign() > 0 {
				q.Add(q, big.NewInt(1))
			}
		default:
			return 0, fmt.Errorf("kernel: unknown rounding mode %d", mode)
		}
	}
	if !q.IsInt64() || q.Int64() == math.MinInt64 {
		return 0, ErrOverflow
	}
	return Rupiah(q.Int64()), nil
}
