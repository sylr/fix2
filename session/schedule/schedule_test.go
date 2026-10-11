package schedule

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(loc *time.Location, s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDaily(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	for _, tc := range []struct {
		name       string
		sched      Daily
		now        string
		start, end string
		in         bool
	}{
		{"within", Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0), Location: ny}, "2026-10-12 09:00", "2026-10-12 08:00", "2026-10-12 17:00", true},
		{"before", Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0), Location: ny}, "2026-10-12 07:59", "2026-10-12 08:00", "2026-10-12 17:00", false},
		{"after: next day", Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0), Location: ny}, "2026-10-12 17:00", "2026-10-13 08:00", "2026-10-13 17:00", false},
		{"across midnight, evening", Daily{Start: Clock(17, 0, 0), End: Clock(16, 55, 0), Location: ny}, "2026-10-12 18:00", "2026-10-12 17:00", "2026-10-13 16:55", true},
		{"across midnight, morning", Daily{Start: Clock(17, 0, 0), End: Clock(16, 55, 0), Location: ny}, "2026-10-13 10:00", "2026-10-12 17:00", "2026-10-13 16:55", true},
		{"across midnight, the gap", Daily{Start: Clock(17, 0, 0), End: Clock(16, 55, 0), Location: ny}, "2026-10-13 16:58", "2026-10-13 17:00", "2026-10-14 16:55", false},
		{"weekdays only, on Friday evening", Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0), Days: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}, Location: ny}, "2026-10-16 18:00", "2026-10-19 08:00", "2026-10-19 17:00", false},
		{"24 hours", Daily{Start: Clock(0, 0, 0), End: Clock(0, 0, 0), Location: ny}, "2026-10-12 12:00", "2026-10-12 00:00", "2026-10-13 00:00", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, in, ok := tc.sched.Window(at(ny, tc.now))
			if !ok || in != tc.in || !w.Start.Equal(at(ny, tc.start)) || !w.End.Equal(at(ny, tc.end)) {
				t.Errorf("Window(%s) = %v-%v in %v ok %v, want %s-%s in %v", tc.now, w.Start.In(ny), w.End.In(ny), in, ok, tc.start, tc.end, tc.in)
			}
		})
	}
}

func TestWeekly(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	s := Weekly{StartDay: time.Sunday, Start: Clock(17, 0, 0), EndDay: time.Friday, End: Clock(17, 0, 0), Location: ny}
	for _, tc := range []struct {
		now, start, end string
		in              bool
	}{
		{"2026-10-14 12:00", "2026-10-11 17:00", "2026-10-16 17:00", true},  // Wednesday
		{"2026-10-17 12:00", "2026-10-18 17:00", "2026-10-23 17:00", false}, // Saturday
		{"2026-10-11 16:59", "2026-10-11 17:00", "2026-10-16 17:00", false}, // Sunday before the start
		{"2026-10-16 17:00", "2026-10-18 17:00", "2026-10-23 17:00", false}, // Friday at the end
		// Across the end of daylight saving time (1 November 2026): the
		// window still opens and closes at 17:00 local time.
		{"2026-11-02 12:00", "2026-11-01 17:00", "2026-11-06 17:00", true},
	} {
		w, in, ok := s.Window(at(ny, tc.now))
		if !ok || in != tc.in || !w.Start.Equal(at(ny, tc.start)) || !w.End.Equal(at(ny, tc.end)) {
			t.Errorf("Window(%s) = %v-%v in %v, want %s-%s in %v", tc.now, w.Start.In(ny), w.End.In(ny), in, tc.start, tc.end, tc.in)
		}
	}
}

func TestHolidays(t *testing.T) {
	s := Holidays{
		Schedule: Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0)},
		Dates:    []string{"2026-12-25"},
	}
	w, in, ok := s.Window(at(time.UTC, "2026-12-24 18:00"))
	if !ok || in || !w.Start.Equal(at(time.UTC, "2026-12-26 08:00")) {
		t.Errorf("Window = %v in %v, want 26 December 08:00", w.Start, in)
	}
}

func TestWindowIDs(t *testing.T) {
	s := Daily{Start: Clock(8, 0, 0), End: Clock(17, 0, 0)}
	a, _, _ := s.Window(at(time.UTC, "2026-10-12 09:00"))
	b, _, _ := s.Window(at(time.UTC, "2026-10-12 16:00"))
	c, _, _ := s.Window(at(time.UTC, "2026-10-13 09:00"))
	other, _, _ := Daily{Start: Clock(8, 0, 0), End: Clock(18, 0, 0)}.Window(at(time.UTC, "2026-10-12 09:00"))
	if a.ID != b.ID || a.ID == c.ID || a.ID == other.ID {
		t.Errorf("IDs %q %q %q %q: want the same within a window, different across windows and schedules", a.ID, b.ID, c.ID, other.ID)
	}
}

func TestResolveLocal(t *testing.T) {
	ny := mustLoad(t, "America/New_York")
	// Clocks set forward at 02:00 on 8 March 2026: 02:30 does not exist.
	got := ResolveLocal(ny, 2026, time.March, 8, Clock(2, 30, 0))
	if want := time.Date(2026, time.March, 8, 7, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("skipped 02:30 = %v, want 03:30 EDT (%v)", got, want)
	}
	// Clocks set back at 02:00 on 1 November 2026: 01:30 happens twice.
	got = ResolveLocal(ny, 2026, time.November, 1, Clock(1, 30, 0))
	if want := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ambiguous 01:30 = %v, want the earlier, 01:30 EDT (%v)", got, want)
	}
}

func TestClockValidate(t *testing.T) {
	if Clock(24, 0, 0).Validate() == nil || Clock(23, 59, 59).Validate() != nil {
		t.Error("Validate does not bound clock times to 00:00:00-23:59:59")
	}
}

// A holiday skipped is not open: the next window is, later.
func TestHolidaysNotOpen(t *testing.T) {
	s := Holidays{Schedule: Daily{Start: Clock(0, 0, 0), End: Clock(0, 0, 0)}, Dates: []string{"2026-12-25"}}
	w, in, ok := s.Window(at(time.UTC, "2026-12-25 12:00"))
	if !ok || in || !w.Start.Equal(at(time.UTC, "2026-12-26 00:00")) {
		t.Errorf("Window on the holiday = %v in %v, want 26 December, not open", w.Start, in)
	}
}
