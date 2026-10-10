package fix2

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

// ---------------------------------------------------------------------------
// ParseInt
// ---------------------------------------------------------------------------

func TestParseInt(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"0", 0},
		{"1", 1},
		{"9", 9},
		{"42", 42},
		{"123456789", 123456789},
		{"-1", -1},
		{"-42", -42},
		{"-123456789", -123456789},
	}
	for _, tt := range tests {
		got, err := ParseInt([]byte(tt.in))
		if err != nil {
			t.Errorf("ParseInt(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseInt(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestParseIntErrors(t *testing.T) {
	tests := []string{
		"",
		"-",
		"abc",
		"12.34",
		"12 34",
		" 1",
		"1 ",
		"1a",
		"-abc",
	}
	for _, in := range tests {
		_, err := ParseInt([]byte(in))
		if err == nil {
			t.Errorf("ParseInt(%q) expected error, got nil", in)
		}
	}
}

func FuzzParseInt(f *testing.F) {
	f.Add([]byte("0"))
	f.Add([]byte("1"))
	f.Add([]byte("42"))
	f.Add([]byte("-1"))
	f.Add([]byte("-42"))
	f.Add([]byte("123456789"))
	f.Add([]byte(""))
	f.Add([]byte("-"))
	f.Add([]byte("abc"))
	f.Add([]byte("9223372036854775807"))
	f.Add([]byte("9223372036854775808"))
	f.Add([]byte("-9223372036854775808"))
	f.Add([]byte("18446744073709551621"))

	f.Fuzz(func(t *testing.T, v []byte) {
		got, err := ParseInt(v)

		// For plain decimal input, strconv.Atoi is the reference: same value,
		// and an error exactly when it reports one (e.g. out of range).
		if plainDecimal(v) {
			want, werr := strconv.Atoi(string(v))
			if (err != nil) != (werr != nil) || (err == nil && got != want) {
				t.Fatalf("ParseInt(%q) = (%d, %v), strconv.Atoi = (%d, %v)", v, got, err, want, werr)
			}
		}
		if err != nil {
			return
		}
		// Round-trip: format back and re-parse.
		s := strconv.Itoa(got)
		got2, err := ParseInt([]byte(s))
		if err != nil {
			t.Fatalf("ParseInt(%q) round-trip parse error: %v", s, err)
		}
		if got != got2 {
			t.Fatalf("ParseInt round-trip: %d != %d (input %q)", got, got2, v)
		}
	})
}

// ---------------------------------------------------------------------------
// ParseFloat
// ---------------------------------------------------------------------------

func TestParseFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"0", 0},
		{"1", 1},
		{"1.5", 1.5},
		{"-1.5", -1.5},
		{"150.25", 150.25},
		{"0.001", 0.001},
		{"123456.789", 123456.789},
		{"-0", 0},
		{"1e2", 100},
		{"-3.14", -3.14},
	}
	for _, tt := range tests {
		got, err := ParseFloat([]byte(tt.in))
		if err != nil {
			t.Errorf("ParseFloat(%q) error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseFloat(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseFloatErrors(t *testing.T) {
	tests := []string{
		"",
		"abc",
		"12.34.56",
		"--1",
	}
	for _, in := range tests {
		_, err := ParseFloat([]byte(in))
		if err == nil {
			t.Errorf("ParseFloat(%q) expected error, got nil", in)
		}
	}
}

func FuzzParseFloat(f *testing.F) {
	f.Add([]byte("0"))
	f.Add([]byte("1.5"))
	f.Add([]byte("-1.5"))
	f.Add([]byte("150.25"))
	f.Add([]byte(""))
	f.Add([]byte("abc"))

	f.Fuzz(func(t *testing.T, v []byte) {
		got, err := ParseFloat(v)
		if err != nil {
			return
		}
		if math.IsNaN(got) || math.IsInf(got, 0) {
			return
		}
		// Round-trip: format back and re-parse.
		s := strconv.FormatFloat(got, 'f', -1, 64)
		got2, err := ParseFloat([]byte(s))
		if err != nil {
			t.Fatalf("ParseFloat(%q) round-trip parse error: %v", s, err)
		}
		if got != got2 {
			t.Fatalf("ParseFloat round-trip: %v != %v (input %q)", got, got2, v)
		}
	})
}

func TestParseIntRange(t *testing.T) {
	maxInt := strconv.Itoa(math.MaxInt)
	minInt := strconv.Itoa(math.MinInt)
	for _, tt := range []struct {
		in   string
		want int
	}{
		{maxInt, math.MaxInt},
		{minInt, math.MinInt},
		{"000" + maxInt, math.MaxInt},
	} {
		if got, err := ParseInt([]byte(tt.in)); err != nil || got != tt.want {
			t.Errorf("ParseInt(%q) = (%d, %v), want %d", tt.in, got, err, tt.want)
		}
	}

	for _, in := range []string{
		"9223372036854775808",  // MaxInt64 + 1
		"-9223372036854775809", // MinInt64 - 1
		"18446744073709551621", // 2^64 + 5, wraps to 5 without the check
		"99999999999999999999999",
	} {
		got, err := ParseInt([]byte(in))
		if !errors.Is(err, strconv.ErrRange) {
			t.Errorf("ParseInt(%q) = (%d, %v), want strconv.ErrRange", in, got, err)
		}
	}
}

func TestParseUintRange(t *testing.T) {
	maxInt := strconv.Itoa(math.MaxInt)
	if got, ok := parseUint([]byte(maxInt)); !ok || got != math.MaxInt {
		t.Errorf("parseUint(%q) = (%d, %v), want (%d, true)", maxInt, got, ok, math.MaxInt)
	}
	for _, in := range []string{"9223372036854775808", "18446744073709551621", "18446744073709551651"} {
		if got, ok := parseUint([]byte(in)); ok {
			t.Errorf("parseUint(%q) = (%d, true), want overflow", in, got)
		}
	}
}

// plainDecimal reports whether v is an optional '-' followed by one or more digits.
func plainDecimal(v []byte) bool {
	if len(v) > 0 && v[0] == '-' {
		v = v[1:]
	}
	if len(v) == 0 {
		return false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
