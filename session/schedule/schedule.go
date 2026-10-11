// Package schedule tells when FIX sessions run: daily or weekly windows in a
// time zone, with holidays. A session logs on only within a window, logs out
// when it ends, and starts each window afresh, at sequence number 1.
package schedule

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// ClockTime is a wall-clock time of day, without a date or a zone.
type ClockTime struct {
	Hour, Minute, Second int
}

// Clock returns the ClockTime h:m:s.
func Clock(h, m, s int) ClockTime { return ClockTime{h, m, s} }

// Validate reports whether c is a time of day in [00:00:00, 24:00:00).
func (c ClockTime) Validate() error {
	if c.Hour < 0 || c.Hour > 23 || c.Minute < 0 || c.Minute > 59 || c.Second < 0 || c.Second > 59 {
		return fmt.Errorf("schedule: clock time %s out of range 00:00:00-23:59:59", c)
	}
	return nil
}

func (c ClockTime) String() string { return fmt.Sprintf("%02d:%02d:%02d", c.Hour, c.Minute, c.Second) }

func (c ClockTime) seconds() int { return c.Hour*3600 + c.Minute*60 + c.Second }

// Window is a stretch of time a session runs.
type Window struct {
	Start, End time.Time
	// ID identifies the window: its schedule and its start. A session
	// whose store holds another window's ID starts afresh.
	ID string
}

// Contains reports whether t is within w: from Start included to End
// excluded.
func (w Window) Contains(t time.Time) bool { return !t.Before(w.Start) && t.Before(w.End) }

// Schedule tells when a session runs.
type Schedule interface {
	// Window returns the window holding t and true, or the next window to
	// open after t and false. ok is false if no window ever opens.
	Window(t time.Time) (w Window, in, ok bool)
	// String describes the schedule; it is part of its windows' IDs.
	String() string
}

// Daily runs from Start to End every day in Days, or every day if Days is
// empty, in Location (UTC if nil). An End before or equal to Start ends the
// next day: the window belongs to the day it starts.
type Daily struct {
	Start, End ClockTime
	Days       []time.Weekday
	Location   *time.Location
}

// Weekly runs from StartDay at Start to EndDay at End, every week, in
// Location (UTC if nil). An end before or equal to the start, on the same
// day, ends the next week.
type Weekly struct {
	StartDay time.Weekday
	Start    ClockTime
	EndDay   time.Weekday
	End      ClockTime
	Location *time.Location
}

func location(l *time.Location) *time.Location {
	if l == nil {
		return time.UTC
	}
	return l
}

// Validate reports problems with d.
func (d Daily) Validate() error {
	if err := d.Start.Validate(); err != nil {
		return err
	}
	return d.End.Validate()
}

func (d Daily) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "daily %s-%s", d.Start, d.End)
	if len(d.Days) > 0 {
		days := slices.Clone(d.Days)
		slices.Sort(days)
		b.WriteString(" on")
		for _, day := range days {
			b.WriteString(" " + day.String()[:3])
		}
	}
	b.WriteString(" " + location(d.Location).String())
	return b.String()
}

// Window implements Schedule.
func (d Daily) Window(t time.Time) (Window, bool, bool) {
	loc := location(d.Location)
	tl := t.In(loc)
	y, m, day := tl.Date()
	// The window holding t started today or yesterday; the next one opens
	// within a week.
	for offset := -1; offset <= 8; offset++ {
		date := time.Date(y, m, day+offset, 12, 0, 0, 0, loc)
		if len(d.Days) > 0 && !slices.Contains(d.Days, date.Weekday()) {
			continue
		}
		start := ResolveLocal(loc, date.Year(), date.Month(), date.Day(), d.Start)
		endDate := date
		if d.End.seconds() <= d.Start.seconds() {
			endDate = date.AddDate(0, 0, 1)
		}
		end := ResolveLocal(loc, endDate.Year(), endDate.Month(), endDate.Day(), d.End)
		if !t.Before(end) {
			continue
		}
		w := Window{Start: start, End: end, ID: windowID(d, start)}
		return w, !t.Before(start), true
	}
	return Window{}, false, false
}

// Validate reports problems with w.
func (w Weekly) Validate() error {
	if err := w.Start.Validate(); err != nil {
		return err
	}
	return w.End.Validate()
}

func (w Weekly) String() string {
	return fmt.Sprintf("weekly %s %s-%s %s %s", w.StartDay.String()[:3], w.Start, w.EndDay.String()[:3], w.End, location(w.Location))
}

// Window implements Schedule.
func (w Weekly) Window(t time.Time) (Window, bool, bool) {
	loc := location(w.Location)
	tl := t.In(loc)
	y, m, day := tl.Date()
	back := (int(tl.Weekday()) - int(w.StartDay) + 7) % 7
	span := (int(w.EndDay) - int(w.StartDay) + 7) % 7
	if span == 0 && w.End.seconds() <= w.Start.seconds() {
		span = 7
	}
	for _, weeks := range []int{-1, 0, 1} {
		date := time.Date(y, m, day-back+7*weeks, 12, 0, 0, 0, loc)
		start := ResolveLocal(loc, date.Year(), date.Month(), date.Day(), w.Start)
		endDate := date.AddDate(0, 0, span)
		end := ResolveLocal(loc, endDate.Year(), endDate.Month(), endDate.Day(), w.End)
		if !t.Before(end) {
			continue
		}
		win := Window{Start: start, End: end, ID: windowID(w, start)}
		return win, !t.Before(start), true
	}
	return Window{}, false, false
}

// Holidays is Schedule without the windows starting on Dates, given as
// "2006-01-02" in Location (UTC if nil).
type Holidays struct {
	Schedule Schedule
	Dates    []string
	Location *time.Location
}

func (h Holidays) String() string {
	dates := slices.Clone(h.Dates)
	slices.Sort(dates)
	return fmt.Sprintf("%s except %s %s", h.Schedule, strings.Join(dates, ","), location(h.Location))
}

// Window implements Schedule.
func (h Holidays) Window(t time.Time) (Window, bool, bool) {
	loc := location(h.Location)
	asked := t
	for range 1000 {
		w, _, ok := h.Schedule.Window(t)
		if !ok {
			return Window{}, false, false
		}
		if !slices.Contains(h.Dates, w.Start.In(loc).Format(time.DateOnly)) {
			w.ID = windowID(h, w.Start)
			return w, w.Contains(asked), true
		}
		t = w.End
	}
	return Window{}, false, false
}

func windowID(s Schedule, start time.Time) string {
	return s.String() + " @" + start.UTC().Format(time.RFC3339)
}

// ResolveLocal is the instant the wall clock of loc shows clock on the
// given date. Wall-clock times a transition makes ambiguous or skips are
// resolved by rule rather than left to time.Date, whose choice is
// unspecified: an ambiguous time (clocks set back) resolves to the earlier
// instant; a skipped time (clocks set forward) shifts forward by the gap,
// so 02:30 in a 02:00 to 03:00 gap is 03:30. It assumes at most one
// transition within a day of the time asked, as every zone has.
func ResolveLocal(loc *time.Location, year int, month time.Month, day int, clock ClockTime) time.Time {
	naive := time.Date(year, month, day, clock.Hour, clock.Minute, clock.Second, 0, time.UTC)
	before := offsetAt(loc, naive.Add(-24*time.Hour))
	after := offsetAt(loc, naive.Add(24*time.Hour))
	early := naive.Add(-before)
	late := naive.Add(-after)
	earlyValid := offsetAt(loc, early) == before
	lateValid := offsetAt(loc, late) == after
	switch {
	case earlyValid && lateValid:
		if late.Before(early) {
			return late.In(loc)
		}
		return early.In(loc)
	case earlyValid:
		return early.In(loc)
	case lateValid:
		return late.In(loc)
	default:
		return early.In(loc)
	}
}

func offsetAt(loc *time.Location, t time.Time) time.Duration {
	_, offset := t.In(loc).Zone()
	return time.Duration(offset) * time.Second
}
