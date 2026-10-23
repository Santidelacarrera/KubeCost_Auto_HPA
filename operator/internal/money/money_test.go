package money

import (
	"errors"
	"math"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[string]USD{
		"5": 5_000_000, "4.00": 4_000_000, "0.000001": 1, "0.0000005": 1, "0.0000004": 0,
		"1e-5": 10, " 12.5 ": 12_500_000, "0": 0, "1000000000000": Max,
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q)=%v,%v want %v", in, got, err, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{"", "abc", "NaN", "Inf", "-1", "1,5", "1000000000001", "1e30"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
	if _, err := Parse("-0.01"); !errors.Is(err, ErrNegative) {
		t.Fatalf("got %v", err)
	}
}

func TestFromFloat(t *testing.T) {
	if got, err := FromFloat(0.1 + 0.2); err != nil || got != 300_000 {
		t.Fatalf("0.1+0.2 must round to exactly 0.3: %v %v", got, err)
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.5, 1e18} {
		if _, err := FromFloat(f); err == nil {
			t.Errorf("FromFloat(%v) should fail", f)
		}
	}
}

func TestStrictBoundaryComparison(t *testing.T) {
	// 0.1+0.2 > 0.3 in float64; in micro-dollars it must be equal.
	a, _ := FromFloat(0.1 + 0.2)
	b, _ := Parse("0.3")
	if a != b {
		t.Fatalf("%v != %v", a, b)
	}
}

func TestArithmeticSaturates(t *testing.T) {
	if got := USD(1_000_000).MulInt(3); got != 3_000_000 {
		t.Fatal(got)
	}
	if got := Max.MulInt(1 << 40); got != Max {
		t.Fatalf("must saturate, got %v", got)
	}
	if got := USD(5).MulInt(0); got != 0 {
		t.Fatal(got)
	}
	if got := USD(10).MulRat(3, 2); got != 15 {
		t.Fatal(got)
	}
}

func TestString(t *testing.T) {
	if got := USD(4_000_000).String(); got != "4.000000" {
		t.Fatal(got)
	}
	if got := USD(1).String(); got != "0.000001" {
		t.Fatal(got)
	}
}

func TestRatio(t *testing.T) {
	r := MustRatio("1.15")
	if got := r.Apply(USD(1_000_000)); got != 1_150_000 {
		t.Fatal(got)
	}
	if q, ok := r.Div(USD(1_150_000)); !ok || q != 1_000_000 {
		t.Fatal(q, ok)
	}
	if _, ok := MustRatio("0").Div(5); ok {
		t.Fatal("division by zero must be reported")
	}
	for _, in := range []string{"", "x", "-1", "NaN", "9999999999"} {
		if _, err := ParseRatio(in); err == nil {
			t.Errorf("ParseRatio(%q) should fail", in)
		}
	}
	if One().Cmp(MustRatio("1.0")) != 0 || One().Cmp(MustRatio("2")) >= 0 {
		t.Fatal("Cmp")
	}
}

func TestRatioArithmetic(t *testing.T) {
	a, b := MustRatio("0.5"), MustRatio("1.5")
	if a.Add(b).Cmp(RatioFromInt(2)) != 0 || a.Mul(b).Cmp(MustRatio("0.75")) != 0 {
		t.Fatal("add/mul")
	}
	q, ok := b.Quo(a)
	if !ok || q.Cmp(RatioFromInt(3)) != 0 {
		t.Fatal("quo")
	}
	if _, ok := b.Quo(MustRatio("0")); ok {
		t.Fatal("division by zero")
	}
	if MinRatio(a, b).Cmp(a) != 0 || MinRatio(b, a).Cmp(a) != 0 {
		t.Fatal("min")
	}
	var zero Ratio
	if !zero.IsZero() || zero.Add(a).Cmp(a) != 0 {
		t.Fatal("zero value must behave as 0")
	}
}
