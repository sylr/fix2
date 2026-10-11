package quickfixgo

import (
	"testing"
	"time"

	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/schedule"
)

// https://github.com/quickfixgo/quickfix/issues/218
//
// A schedule whose StartTime equals its EndTime was never in session, where
// other QuickFIX engines run it non-stop. fix2's Daily documents such a
// window as lasting until the next day's Start: back to back windows, always
// in session (each still starts afresh, as every window does; a nil
// Schedule never resets).
func TestIssue218_DailyStartEqualsEndAlwaysInSession(t *testing.T) {
	d := schedule.Daily{Start: schedule.Clock(7, 0, 0), End: schedule.Clock(7, 0, 0)}
	base := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	for at := base; at.Before(base.Add(72 * time.Hour)); at = at.Add(17 * time.Minute) {
		w, in, ok := d.Window(at)
		if !ok || !in {
			t.Fatalf("Window(%s) = %v, in %v, ok %v: want in session", at, w, in, ok)
		}
		if w.End.Sub(w.Start) != 24*time.Hour {
			t.Fatalf("Window(%s) = %s to %s, want 24h", at, w.Start, w.End)
		}
		if next, in, _ := d.Window(w.End); !in || !next.Start.Equal(w.End) {
			t.Fatalf("window after %s starts %s (in %v), want at once", w.End, next.Start, in)
		}
	}
}

// And a session with such a schedule logs on.
func TestIssue218_SessionWithStartEqualsEndLogsOn(t *testing.T) {
	now := time.Now().UTC().Add(-time.Hour)
	c := schedule.Clock(now.Hour(), now.Minute(), now.Second())
	loggedOn(t, func(cfg *session.Config) { cfg.Schedule = schedule.Daily{Start: c, End: c} })
}
