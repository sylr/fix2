package binancefix

import "time"

// rateBudget counts a market data connection's messages over a sliding
// window, as Binance does: those written, at the time their write
// returned, and those Binance may read later, the requests queued and the
// session messages being written. A message is counted from when it is
// queued (a request) or handed to the connection (a session message), and
// its window runs from when its write returned, at which Binance had read
// it or never will: never too early. Requests are admitted while they
// leave the reserve for session messages, which are never refused. The
// connection's lock guards it.
type rateBudget struct {
	limit, reserve int
	window         time.Duration
	times          []time.Time // messages written within the window, oldest first
	pending        int         // requests queued, or being written
	inflight       int         // session messages being written
}

func newRateBudget(limit, reserve int, window time.Duration) *rateBudget {
	return &rateBudget{limit: limit, reserve: reserve, window: window}
}

// prune forgets the messages written a window before now or earlier.
func (b *rateBudget) prune(now time.Time) {
	i := 0
	for i < len(b.times) && now.Sub(b.times[i]) >= b.window {
		i++
	}
	b.times = b.times[i:]
}

// admit reports whether a request may be queued at now, and counts it if
// so.
func (b *rateBudget) admit(now time.Time) bool {
	b.prune(now)
	if len(b.times)+b.pending+b.inflight+1 > b.limit-b.reserve {
		return false
	}
	b.pending++
	return true
}

// sending counts session messages handed to the connection.
func (b *rateBudget) sending(session int) { b.inflight += session }

// wrote counts requests and session messages whose write returned at now.
func (b *rateBudget) wrote(now time.Time, requests, session int) {
	b.pending -= requests
	b.inflight -= session
	for range requests + session {
		b.times = append(b.times, now)
	}
	b.prune(now)
}

// dropped forgets n requests queued and never to be written.
func (b *rateBudget) dropped(n int) { b.pending -= n }
