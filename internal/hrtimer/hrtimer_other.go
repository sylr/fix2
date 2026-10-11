//go:build !linux && !darwin

package hrtimer

import "time"

// sleepUntil falls back to a Go timer where no high-resolution wait is
// implemented: the wait is as precise as the runtime's timers.
func sleepUntil(deadline time.Time) {
	time.Sleep(time.Until(deadline))
}
