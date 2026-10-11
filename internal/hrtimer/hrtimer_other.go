//go:build !linux

package hrtimer

import "time"

// sleepUntil falls back to a Go timer where clock_nanosleep and timer
// slack are not available: the wait is as precise as the runtime's timers.
func sleepUntil(deadline time.Time) {
	time.Sleep(time.Until(deadline))
}
