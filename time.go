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
//
// FIX allows second 60 for a UTC leap second, which can only be inserted at
// 23:59:60 on the last day of a month. time.Time cannot represent it: it is
// returned as 23:59:59.999999999 that day, so that it keeps its date and
// never sorts before the second preceding it nor after the one following
// it, at the cost of its fraction.
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
// truncated to nanoseconds. A leap second, 23:59:60, is returned as
// 23:59:59.999999999 (see ParseUTCTimestamp).
func ParseUTCTimeOnly(v []byte) (time.Time, error) {
	return parseFixTime("ParseUTCTimeOnly", v, TimeFormatTimeOnlySec)
}

// parseFixTime parses v as layout, which ends with whole seconds, followed by
// no fraction or a fraction of 3, 6, 9 or 12 digits.
func parseFixTime(name string, v []byte, layout string) (time.Time, error) {
	base := len(layout)                // end of the whole seconds
	digits := len(v) - len(layout) - 1 // fraction digits after the dot
	switch {
	case len(v) == len(layout):
		digits = 0
	case len(v) < len(layout) || v[len(layout)] != '.':
		// time.Parse also takes a comma, FIX does not.
		return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
	case digits == 3 || digits == 6 || digits == 9:
	case digits == 12:
		// Go times stop at nanoseconds: drop the picoseconds.
		for _, c := range v[len(v)-3:] {
			if c < '0' || c > '9' {
				return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
			}
		}
		v = v[:len(v)-3]
		digits = 9
	default:
		return time.Time{}, fmt.Errorf("%s: cannot parse %q", name, v)
	}
	if t, ok := fastTime(v, base); ok {
		return t, nil
	}
	if digits > 0 {
		layout += ".000000000"[:digits+1]
	}
	if v[base-2] == '6' && v[base-1] == '0' {
		return parseLeapSecond(name, v, layout, base)
	}
	return time.Parse(layout, string(v))
}

// fastTime parses v, a timestamp ("YYYYMMDD-HH:MM:SS", base 17) or a time
// ("HH:MM:SS", base 8) with a fraction of 0, 3, 6 or 9 digits, without
// allocating. It reports false for anything time.Parse should judge,
// invalid values included, so that errors keep its wording.
func fastTime(v []byte, base int) (time.Time, bool) {
	num := func(b []byte) (int, bool) {
		n := 0
		for _, c := range b {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	year, month, day := 0, 1, 1
	clock := v
	if base == len(TimeFormatTimestampSec) {
		var ok1, ok2, ok3 bool
		year, ok1 = num(v[0:4])
		month, ok2 = num(v[4:6])
		day, ok3 = num(v[6:8])
		if !ok1 || !ok2 || !ok3 || v[8] != '-' || month < 1 || month > 12 || day < 1 ||
			day > time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day() {
			return time.Time{}, false
		}
		clock = v[9:]
	}
	h, ok1 := num(clock[0:2])
	m, ok2 := num(clock[3:5])
	s, ok3 := num(clock[6:8])
	if !ok1 || !ok2 || !ok3 || clock[2] != ':' || clock[5] != ':' || h > 23 || m > 59 || s > 59 {
		return time.Time{}, false
	}
	ns := 0
	if frac := v[base:]; len(frac) > 0 {
		f, ok := num(frac[1:])
		if !ok {
			return time.Time{}, false
		}
		for range 10 - len(frac) {
			f *= 10
		}
		ns = f
	}
	return time.Date(year, time.Month(month), day, h, m, s, ns, time.UTC), true
}

// parseLeapSecond parses v, whose seconds at v[base-2:base] are 60: only
// 23:59:60 is a leap second, on the last day of a month for a timestamp.
func parseLeapSecond(name string, v []byte, layout string, base int) (time.Time, error) {
	if string(v[base-8:base-3]) != "23:59" {
		return time.Time{}, fmt.Errorf("%s: cannot parse %q: second 60 outside a leap second", name, v)
	}
	// Parse it as second 59, which validates the rest.
	var buf [32]byte
	n := copy(buf[:], v)
	buf[base-2], buf[base-1] = '5', '9'
	t, err := time.Parse(layout, string(buf[:n]))
	if err != nil {
		return time.Time{}, err
	}
	if base == len(TimeFormatTimestampSec) && t.AddDate(0, 0, 1).Day() != 1 {
		return time.Time{}, fmt.Errorf("%s: cannot parse %q: leap second not on the last day of a month", name, v)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 999_999_999, time.UTC), nil
}

// TimePrecision is the number of fraction digits a formatted FIX time has.
type TimePrecision int

const (
	Seconds      TimePrecision = 0 // no fraction
	Milliseconds TimePrecision = 3
	Microseconds TimePrecision = 6
	Nanoseconds  TimePrecision = 9
)

// layout returns the Go layout of a timestamp, time or date-less, with p
// fraction digits; p is clamped to 0-9.
func (p TimePrecision) layout(base string) string {
	n := min(max(int(p), 0), 9)
	if n == 0 {
		return base
	}
	return base + ".000000000"[:n+1]
}

// AppendUTCTimestamp appends t as a FIX UTCTimestamp ("YYYYMMDD-HH:MM:SS"
// and p fraction digits, truncated) to dst without allocating.
func AppendUTCTimestamp(dst []byte, t time.Time, p TimePrecision) []byte {
	t = t.UTC()
	y, mo, d := t.Date()
	if y < 0 || y > 9999 {
		return t.AppendFormat(dst, p.layout(TimeFormatTimestampSec))
	}
	dst = append4(dst, y)
	dst = append2(dst, int(mo))
	dst = append2(dst, d)
	dst = append(dst, '-')
	return appendClock(dst, t, p)
}

// AppendUTCTimeOnly appends t as a FIX UTCTimeOnly ("HH:MM:SS" and p
// fraction digits, truncated) to dst without allocating.
func AppendUTCTimeOnly(dst []byte, t time.Time, p TimePrecision) []byte {
	return appendClock(dst, t.UTC(), p)
}

// AppendUTCDate appends t as a FIX UTCDateOnly ("YYYYMMDD") to dst without
// allocating.
func AppendUTCDate(dst []byte, t time.Time) []byte {
	t = t.UTC()
	y, mo, d := t.Date()
	if y < 0 || y > 9999 {
		return t.AppendFormat(dst, TimeFormatDate)
	}
	dst = append4(dst, y)
	dst = append2(dst, int(mo))
	return append2(dst, d)
}

// appendClock appends "HH:MM:SS" and p fraction digits of t, already UTC.
func appendClock(dst []byte, t time.Time, p TimePrecision) []byte {
	h, mi, s := t.Clock()
	dst = append2(dst, h)
	dst = append(dst, ':')
	dst = append2(dst, mi)
	dst = append(dst, ':')
	dst = append2(dst, s)
	n := min(max(int(p), 0), 9)
	if n == 0 {
		return dst
	}
	dst = append(dst, '.')
	frac := t.Nanosecond()
	var digits [9]byte
	for i := 8; i >= 0; i-- {
		digits[i] = byte('0' + frac%10)
		frac /= 10
	}
	return append(dst, digits[:n]...)
}

func append2(dst []byte, v int) []byte {
	return append(dst, byte('0'+v/10), byte('0'+v%10))
}

func append4(dst []byte, v int) []byte {
	return append(dst, byte('0'+v/1000), byte('0'+v/100%10), byte('0'+v/10%10), byte('0'+v%10))
}
