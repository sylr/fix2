//go:build race

package fix2

// raceEnabled reports whether tests run under the race detector, which makes
// sync.Pool drop some of what is put in it.
const raceEnabled = true
