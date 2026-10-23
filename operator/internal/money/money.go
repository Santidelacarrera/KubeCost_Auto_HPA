// Package money provides exact fixed-point USD arithmetic for budget checks.
//
// Amounts are stored as an integer number of micro-dollars (1e-6 USD) so that
// comparisons against a configured budget never depend on floating-point
// rounding. Conversion from untrusted input (JSON numbers, user strings) goes
// through math/big, rounds half up to micro-dollars, and rejects negatives,
// non-finite values and amounts above MaxUSD.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// USD is an amount in micro-dollars.
type USD int64

const (
	microPerUSD = 1_000_000
	// MaxUSD bounds accepted amounts (1e12 USD) so sums of a few thousand
	// replicas can never overflow int64 micro-dollars.
	MaxUSD = 1_000_000_000_000
	// Max is the largest representable accepted amount.
	Max = USD(MaxUSD * microPerUSD)
)

var (
	ErrInvalid  = errors.New("invalid amount")
	ErrNegative = errors.New("amount must not be negative")
	ErrTooLarge = errors.New("amount too large")
)

// Parse converts a decimal string ("5", "4.00", "1e-5") to USD, rounding half
// up at the 6th decimal.
func Parse(s string) (USD, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrInvalid)
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
		return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	return fromRat(r)
}

// FromFloat converts a float64 coming from a JSON payload, rejecting NaN,
// infinities and negatives.
func FromFloat(f float64) (USD, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: not finite", ErrInvalid)
	}
	r := new(big.Rat)
	if r.SetFloat64(f) == nil {
		return 0, fmt.Errorf("%w: not finite", ErrInvalid)
	}
	return fromRat(r)
}

func fromRat(r *big.Rat) (USD, error) {
	if r.Sign() < 0 {
		return 0, ErrNegative
	}
	// round half up at micro-dollars: floor(r*1e6 + 1/2)
	scaled := new(big.Rat).Mul(r, big.NewRat(microPerUSD, 1))
	scaled.Add(scaled, big.NewRat(1, 2))
	n := new(big.Int).Div(scaled.Num(), scaled.Denom())
	if !n.IsInt64() || n.Int64() > int64(Max) {
		return 0, ErrTooLarge
	}
	return USD(n.Int64()), nil
}

// String renders the amount with exactly six decimals ("4.000000").
func (u USD) String() string {
	neg := u < 0
	v := int64(u)
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%d.%06d", v/microPerUSD, v%microPerUSD)
	if neg {
		return "-" + s
	}
	return s
}

// Float returns an approximate float for metrics/logging only; never use it
// for budget comparisons.
func (u USD) Float() float64 { return float64(u) / microPerUSD }

// MulInt multiplies by a non-negative integer, saturating at Max.
func (u USD) MulInt(n int64) USD {
	if n <= 0 || u <= 0 {
		return 0
	}
	if int64(u) > int64(Max)/n {
		return Max
	}
	return USD(int64(u) * n)
}

// MulRat multiplies by a non-negative rational factor num/den, rounding half
// up and saturating at Max. den must be positive.
func (u USD) MulRat(num, den int64) USD {
	if num <= 0 || den <= 0 || u <= 0 {
		return 0
	}
	r := new(big.Rat).Mul(big.NewRat(int64(u), 1), big.NewRat(num, den))
	r.Add(r, big.NewRat(1, 2))
	n := new(big.Int).Div(r.Num(), r.Denom())
	if !n.IsInt64() || n.Int64() > int64(Max) {
		return Max
	}
	return USD(n.Int64())
}

// Ratio is an exact non-negative decimal factor (e.g. a price multiplier).
type Ratio struct{ rat *big.Rat }

// ParseRatio parses a non-negative decimal factor such as "1.15".
func ParseRatio(s string) (Ratio, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if s == "" || strings.Contains(lower, "nan") || strings.Contains(lower, "inf") {
		return Ratio{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return Ratio{}, fmt.Errorf("%w: %q", ErrInvalid, s)
	}
	if r.Sign() < 0 {
		return Ratio{}, ErrNegative
	}
	if r.Cmp(big.NewRat(1_000_000, 1)) > 0 {
		return Ratio{}, ErrTooLarge
	}
	return Ratio{rat: r}, nil
}

// MustRatio is ParseRatio for constants; it panics on invalid input.
func MustRatio(s string) Ratio {
	r, err := ParseRatio(s)
	if err != nil {
		panic(err)
	}
	return r
}

// One is the neutral factor.
func One() Ratio { return Ratio{rat: big.NewRat(1, 1)} }

// IsZero reports whether the factor is zero (or unset).
func (r Ratio) IsZero() bool { return r.rat == nil || r.rat.Sign() == 0 }

// Apply multiplies an amount by the factor (rounding half up, saturating).
func (r Ratio) Apply(u USD) USD {
	if r.rat == nil || u <= 0 {
		return 0
	}
	x := new(big.Rat).Mul(big.NewRat(int64(u), 1), r.rat)
	x.Add(x, big.NewRat(1, 2))
	n := new(big.Int).Div(x.Num(), x.Denom())
	if !n.IsInt64() || n.Int64() > int64(Max) {
		return Max
	}
	return USD(n.Int64())
}

// Div returns u / r (rounding half up), or ok=false when r is zero.
func (r Ratio) Div(u USD) (USD, bool) {
	if r.IsZero() {
		return 0, false
	}
	x := new(big.Rat).Quo(big.NewRat(int64(u), 1), r.rat)
	x.Add(x, big.NewRat(1, 2))
	n := new(big.Int).Div(x.Num(), x.Denom())
	if !n.IsInt64() || n.Int64() > int64(Max) {
		return Max, true
	}
	return USD(n.Int64()), true
}

// Cmp compares two factors.
func (r Ratio) Cmp(o Ratio) int {
	a, b := r.rat, o.rat
	if a == nil {
		a = new(big.Rat)
	}
	if b == nil {
		b = new(big.Rat)
	}
	return a.Cmp(b)
}

// Float returns an approximation for logs only.
func (r Ratio) Float() float64 {
	if r.rat == nil {
		return 0
	}
	f, _ := r.rat.Float64()
	return f
}

// RatioFromInt builds an integer factor.
func RatioFromInt(n int64) Ratio { return Ratio{rat: new(big.Rat).SetInt64(n)} }

func (r Ratio) val() *big.Rat {
	if r.rat == nil {
		return new(big.Rat)
	}
	return r.rat
}

// Add returns r+o.
func (r Ratio) Add(o Ratio) Ratio { return Ratio{rat: new(big.Rat).Add(r.val(), o.val())} }

// Mul returns r*o.
func (r Ratio) Mul(o Ratio) Ratio { return Ratio{rat: new(big.Rat).Mul(r.val(), o.val())} }

// Quo returns r/o, or ok=false when o is zero.
func (r Ratio) Quo(o Ratio) (Ratio, bool) {
	if o.IsZero() {
		return Ratio{}, false
	}
	return Ratio{rat: new(big.Rat).Quo(r.val(), o.val())}, true
}

// MinRatio returns the smaller factor.
func MinRatio(a, b Ratio) Ratio {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
