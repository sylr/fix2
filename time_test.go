package fix2

import (
	"testing"
	"time"
)

func TestParseUTCTimestamp(t *testing.T) {
	day := func(nsec int) time.Time { return time.Date(2001, 12, 17, 9, 30, 47, nsec, time.UTC) }
	tests := []struct {
		in   string
		want time.Time
	}{
		{"20011217-09:30:47", day(0)},
		{"20011217-09:30:47.123", day(123_000_000)},
		{"20011217-09:30:47.123456", day(123_456_000)},
		{"20011217-09:30:47.123456789", day(123_456_789)},
		{"20011217-09:30:47.123456789012", day(123_456_789)},
	}
	for _, tt := range tests {
		got, err := ParseUTCTimestamp([]byte(tt.in))
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("ParseUTCTimestamp(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{
		"", "20011217", "20011217-09:30:47.", "20011217-09:30:47.1", "20011217-09:30:47.1234",
		"20011217-09:30:47.1234567", "20011217-09:30:47.12345678901", "20011217-09:30:47.12345678901x",
		"20011217-09:30:47.1234567890123", "20011217-09:30:4x.123456",
		"20011217-09:30:47,123", "20011217-09:30:47,123456", "20011217-09:30:47,123456789",
		"20011217-09:30:47,123456789012", "20010230-09:30:47.123456", "20011217-24:30:47.123456",
		"20011217-09:30:60.123456", "20011217-09:30:47123456",
	} {
		if got, err := ParseUTCTimestamp([]byte(in)); err == nil {
			t.Errorf("ParseUTCTimestamp(%q) = %v, want error", in, got)
		}
	}
}

func TestParseUTCTimeOnly(t *testing.T) {
	at := func(nsec int) time.Time { return time.Date(0, 1, 1, 9, 30, 47, nsec, time.UTC) }
	tests := []struct {
		in   string
		want time.Time
	}{
		{"09:30:47", at(0)},
		{"09:30:47.123", at(123_000_000)},
		{"09:30:47.123456", at(123_456_000)},
		{"09:30:47.123456789", at(123_456_789)},
		{"09:30:47.123456789012", at(123_456_789)},
	}
	for _, tt := range tests {
		got, err := ParseUTCTimeOnly([]byte(tt.in))
		if err != nil || !got.Equal(tt.want) {
			t.Errorf("ParseUTCTimeOnly(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"", "09:30", "09:30:47.", "09:30:47.12", "09:30:47.1234", "09:30:47.12345678901x", "09:30:47.123456789012345",
		"09:30:47,123", "09:30:47,123456", "09:30:47,123456789", "09:30:47,123456789012", "25:30:47.123456"} {
		if got, err := ParseUTCTimeOnly([]byte(in)); err == nil {
			t.Errorf("ParseUTCTimeOnly(%q) = %v, want error", in, got)
		}
	}
}

func TestFormatMicro(t *testing.T) {
	ts := time.Date(2001, 12, 17, 9, 30, 47, 123_456_789, time.UTC)
	if got := ts.Format(TimeFormatTimestampMicro); got != "20011217-09:30:47.123456" {
		t.Errorf("TimeFormatTimestampMicro = %q", got)
	}
	if got := ts.Format(TimeFormatTimeOnlyMicro); got != "09:30:47.123456" {
		t.Errorf("TimeFormatTimeOnlyMicro = %q", got)
	}
}

func TestParseLeapSecond(t *testing.T) {
	leap := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 23, 59, 59, 999_999_999, time.UTC)
	}
	for in, want := range map[string]time.Time{
		"20161231-23:59:60":              leap(2016, 12, 31),
		"20161231-23:59:60.500":          leap(2016, 12, 31),
		"20150630-23:59:60.123456":       leap(2015, 6, 30),
		"20150630-23:59:60.123456789":    leap(2015, 6, 30),
		"20150630-23:59:60.123456789012": leap(2015, 6, 30),
		"20160331-23:59:60":              leap(2016, 3, 31), // allowed at the end of any month
		"20160229-23:59:60":              leap(2016, 2, 29),
	} {
		if got, err := ParseUTCTimestamp([]byte(in)); err != nil || !got.Equal(want) {
			t.Errorf("ParseUTCTimestamp(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{
		"20161230-23:59:60",     // not the last day of a month
		"20161231-23:58:60",     // not 23:59
		"20161231-22:59:60",     // nor this
		"20161231-23:59:61",     // no second 61
		"20161231-23:59:60,500", // comma
		"20161232-23:59:60",     // no such day
		"20161231-23:59:6x",
		"20161231-23:59:60.",              // empty fraction
		"20161231-23:59:60.12x",           // bad fraction digit
		"20161231-23:59:60.1234",          // 4 fraction digits
		"20161231-23:59:60.12345678901x",  // bad picosecond digit
		"20161231-23:59:60.1234567890123", // 13 fraction digits
	} {
		if got, err := ParseUTCTimestamp([]byte(in)); err == nil {
			t.Errorf("ParseUTCTimestamp(%q) = %v, want error", in, got)
		}
	}
	// 28 February is the last day of its month in 2015, not in 2016.
	if got, err := ParseUTCTimestamp([]byte("20150228-23:59:60")); err != nil || !got.Equal(leap(2015, 2, 28)) {
		t.Errorf("28 February 2015: %v, %v", got, err)
	}
	if got, err := ParseUTCTimestamp([]byte("20160228-23:59:60")); err == nil {
		t.Errorf("28 February 2016: %v, want error", got)
	}

	// The leap second sorts after the second before it and before the next
	// day.
	before, _ := ParseUTCTimestamp([]byte("20161231-23:59:59.999"))
	during, _ := ParseUTCTimestamp([]byte("20161231-23:59:60.500"))
	after, _ := ParseUTCTimestamp([]byte("20170101-00:00:00.000"))
	if !before.Before(during) || !during.Before(after) || during.Day() != 31 {
		t.Errorf("order: %v, %v, %v", before, during, after)
	}

	at := time.Date(0, 1, 1, 23, 59, 59, 999_999_999, time.UTC)
	for _, in := range []string{"23:59:60", "23:59:60.000", "23:59:60.123456789012"} {
		if got, err := ParseUTCTimeOnly([]byte(in)); err != nil || !got.Equal(at) {
			t.Errorf("ParseUTCTimeOnly(%q) = %v, %v; want %v", in, got, err, at)
		}
	}
	for _, in := range []string{"12:00:60", "23:58:60.000", "23:59:61"} {
		if got, err := ParseUTCTimeOnly([]byte(in)); err == nil {
			t.Errorf("ParseUTCTimeOnly(%q) = %v, want error", in, got)
		}
	}
}
