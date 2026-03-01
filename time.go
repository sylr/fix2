package fix2

import (
	"fmt"
	"time"
)

// TimeFormat is a Go time layout string for formatting FIX time values.
type TimeFormat = string

const (
	TimeFormatTimestamp     TimeFormat = "20060102-15:04:05.000"
	TimeFormatTimestampSec  TimeFormat = "20060102-15:04:05"
	TimeFormatTimestampNano TimeFormat = "20060102-15:04:05.000000000"
	TimeFormatDate          TimeFormat = "20060102"
	TimeFormatTimeOnly      TimeFormat = "15:04:05.000"
	TimeFormatTimeOnlySec   TimeFormat = "15:04:05"
	TimeFormatTimeOnlyNano  TimeFormat = "15:04:05.000000000"
)

// ParseUTCTimestamp parses a FIX UTCTimestamp value (e.g. "20231015-10:00:00.000").
// Supported formats: "YYYYMMDD-HH:MM:SS" (17), "YYYYMMDD-HH:MM:SS.sss" (21),
// "YYYYMMDD-HH:MM:SS.sssssssss" (27).
func ParseUTCTimestamp(v []byte) (time.Time, error) {
	var layout string
	switch len(v) {
	case 17:
		layout = TimeFormatTimestampSec
	case 21:
		layout = TimeFormatTimestamp
	case 27:
		layout = TimeFormatTimestampNano
	default:
		return time.Time{}, fmt.Errorf("ParseUTCTimestamp: cannot parse %q", v)
	}
	return time.Parse(layout, string(v))
}

// ParseUTCDate parses a FIX UTCDate value (e.g. "20231015").
func ParseUTCDate(v []byte) (time.Time, error) {
	return time.Parse(TimeFormatDate, string(v))
}

// ParseUTCTimeOnly parses a FIX UTCTimeOnly value (e.g. "10:00:00.000").
// Supported formats: "HH:MM:SS" (8), "HH:MM:SS.sss" (12),
// "HH:MM:SS.sssssssss" (18).
func ParseUTCTimeOnly(v []byte) (time.Time, error) {
	var layout string
	switch len(v) {
	case 8:
		layout = TimeFormatTimeOnlySec
	case 12:
		layout = TimeFormatTimeOnly
	case 18:
		layout = TimeFormatTimeOnlyNano
	default:
		return time.Time{}, fmt.Errorf("ParseUTCTimeOnly: cannot parse %q", v)
	}
	return time.Parse(layout, string(v))
}
