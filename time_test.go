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
	for _, in := range []string{"", "09:30", "09:30:47.", "09:30:47.12", "09:30:47.1234", "09:30:47.12345678901x", "09:30:47.123456789012345"} {
		if got, err := ParseUTCTimeOnly([]byte(in)); err == nil {
			t.Errorf("ParseUTCTimeOnly(%q) = %v, want error", in, got)
		}
	}
}
