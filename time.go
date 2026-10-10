package fix2

import (
	"fmt"
	"time"
)

// TimeFormat is a Go time layout string for formatting FIX time values.
type TimeFormat = string

const (
	TimeFormatTimestamp      TimeFormat = "20060102-15:04:05.000"
	TimeFormatTimestampSec   TimeFormat = "20060102-15:04:05"
	TimeFormatTimestampMicro TimeFormat = "20060102-15:04:05.000000"
	TimeFormatTimestampNano  TimeFormat = "20060102-15:04:05.000000000"
	TimeFormatDate           TimeFormat = "20060102"
	TimeFormatTimeOnly       TimeFormat = "15:04:05.000"
	TimeFormatTimeOnlySec    TimeFormat = "15:04:05"
	TimeFormatTimeOnlyMicro  TimeFormat = "15:04:05.000000"
	TimeFormatTimeOnlyNano   TimeFormat = "15:04:05.000000000"
)

// ParseUTCTimestamp parses a FIX UTCTimestamp value (e.g. "20231015-10:00:00.000").
// Supported formats: "YYYYMMDD-HH:MM:SS" (17) and fractions of seconds in
// milliseconds (21), microseconds (24), nanoseconds (27) or picoseconds (30),
// the latter truncated to nanoseconds.
func ParseUTCTimestamp(v []byte) (time.Time, error) {
	return parseFixTime("ParseUTCTimestamp", v, TimeFormatTimestampSec)
}

// ParseUTCDate parses a FIX UTCDate value (e.g. "20231015").
func ParseUTCDate(v []byte) (time.Time, error) {
	return time.Parse(TimeFormatDate, string(v))
}

// ParseUTCTimeOnly parses a FIX UTCTimeOnly value (e.g. "10:00:00.000").
// Supported formats: "HH:MM:SS" (8) and fractions of seconds in milliseconds
// (12), microseconds (15), nanoseconds (18) or picoseconds (21), the latter
// truncated to nanoseconds.
func ParseUTCTimeOnly(v []byte) (time.Time, error) {
	return parseFixTime("ParseUTCTimeOnly", v, TimeFormatTimeOnlySec)
}

// parseFixTime parses v as layout, which ends with whole seconds, followed by
// no fraction or a fraction of 3, 6, 9 or 12 digits.
func parseFixTime(name string, v []byte, layout string) (time.Time, error) {
	digits := len(v) - len(layout) - 1 // fraction digits after the dot
	switch {
	case len(v) == len(layout):
	case len(v) < len(layout) || v[len(layout)] != '.':
		// time.Parse also takes a comma, FIX does not.
		return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
	case digits == 3 || digits == 6 || digits == 9:
		layout += ".000000000"[:digits+1]
	case digits == 12:
		// Go times stop at nanoseconds: drop the picoseconds.
		for _, c := range v[len(v)-3:] {
			if c < '0' || c > '9' {
				return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
			}
		}
		v = v[:len(v)-3]
		layout += ".000000000"
	default:
		return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
	}
	return time.Parse(layout, string(v))
}
