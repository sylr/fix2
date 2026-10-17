package binancefix

import (
	"math"
	"math/big"
	"regexp"
	"strings"
	"testing"
)

func TestFormatDecimal(t *testing.T) {
	for _, tc := range []struct {
		mantissa int64
		exponent int8
		want     string
	}{
		{0, 0, "0"},
		{0, -2, "0.00"},
		{0, 3, "0"},
		{1, 0, "1"},
		{-1, 0, "-1"},
		{150_000_000, -8, "1.50000000"},
		{6_543_210, -2, "65432.10"},
		{-6_543_210, -2, "-65432.10"},
		{5, -3, "0.005"},
		{-5, -3, "-0.005"},
		{12345, -5, "0.12345"},
		{12345, -4, "1.2345"},
		{15, 2, "1500"},
		{-15, 2, "-1500"},
		{1, -1, "0.1"},
		{math.MaxInt64, 0, "9223372036854775807"},
		{math.MaxInt64, -19, "0.9223372036854775807"},
		{math.MaxInt64, -20, "0.09223372036854775807"},
		{math.MinInt64, 0, "-9223372036854775808"},
		{math.MinInt64, -18, "-9.223372036854775808"},
		{math.MinInt64, 2, "-922337203685477580800"},
		{7, -128, "0." + strings.Repeat("0", 127) + "7"},
		{7, 127, "7" + strings.Repeat("0", 127)},
	} {
		if got := FormatDecimal(tc.mantissa, tc.exponent); got != tc.want {
			t.Errorf("FormatDecimal(%d, %d) = %q, want %q", tc.mantissa, tc.exponent, got, tc.want)
		}
	}
	if got := string(AppendDecimal([]byte("px="), 6_543_210, -2)); got != "px=65432.10" {
		t.Errorf("AppendDecimal %q", got)
	}
}

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// FormatDecimal is exact: its text is mantissa × 10^exponent, with as many
// decimals as the exponent tells.
func FuzzFormatDecimal(f *testing.F) {
	for _, s := range []struct {
		m int64
		e int8
	}{{0, 0}, {1, -8}, {-1, 8}, {math.MinInt64, -19}, {math.MaxInt64, 127}, {123, -128}} {
		f.Add(s.m, s.e)
	}
	f.Fuzz(func(t *testing.T, mantissa int64, exponent int8) {
		s := FormatDecimal(mantissa, exponent)
		if !decimalPattern.MatchString(s) {
			t.Fatalf("FormatDecimal(%d, %d) = %q", mantissa, exponent, s)
		}
		if _, frac, ok := strings.Cut(s, "."); exponent < 0 && (!ok || len(frac) != -int(exponent)) || exponent >= 0 && ok {
			t.Fatalf("FormatDecimal(%d, %d) = %q: decimals", mantissa, exponent, s)
		}
		got, ok := new(big.Rat).SetString(s)
		if !ok {
			t.Fatalf("FormatDecimal(%d, %d) = %q: not a number", mantissa, exponent, s)
		}
		want := new(big.Rat).SetInt64(mantissa)
		e := int64(exponent)
		scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(max(e, -e)), nil))
		if exponent >= 0 {
			want.Mul(want, scale)
		} else {
			want.Quo(want, scale)
		}
		if got.Cmp(want) != 0 {
			t.Fatalf("FormatDecimal(%d, %d) = %q, want %s", mantissa, exponent, s, want.RatString())
		}
	})
}
