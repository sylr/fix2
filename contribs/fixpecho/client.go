package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/session"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/transport"
)

// clientOptions configures a client run.
type clientOptions struct {
	flow        fixp.FlowType   // the client's flow
	serverFlows []fixp.FlowType // those it accepts, any if empty
	token       string          // the credentials sent, none if empty
	keepalive   time.Duration
	count       int           // messages to send
	window      int           // messages awaiting their echo, at most
	size        int           // of each message, its header included
	timeout     time.Duration // of the whole run
	reconnect   time.Duration // the first wait before connecting again, the engine's default if 0
	logger      *slog.Logger
}

// result is what a client run measured.
type result struct {
	sent, echoed, duplicates int
	notApplied               uint64        // the server never got them (Idempotent)
	establishments           uint64        // connections the session was established on
	elapsed                  time.Duration // from the first message sent to the last echo
	rtts                     []time.Duration
}

// probeState is where a message is.
type probeState uint8

const (
	unsent      probeState = iota
	outstanding            // sent, awaiting its echo
	echoed
	lost // the server never got it (NotApplied)
)

// loader sends the messages and receives their echoes.
type loader struct {
	session.BaseHandler
	count       int
	window      chan struct{} // a slot per message outstanding
	established chan struct{}
	once        sync.Once

	mu    sync.Mutex
	state []probeState // by message number - 1
	seqOf []uint64     // by message number - 1, on a sequenced flow
	bySeq map[uint64]uint64
	// inflight is the message being sent, its sequence number not known
	// yet, 0 if none; inflightNA the NotApplied ranges come meanwhile.
	inflight   uint64
	inflightNA [][2]uint64
	rtts       []time.Duration
	echoes     int
	duplicates int
	notApplied uint64
	finished   bool
	all        chan struct{} // closed once every message is echoed or lost
}

func newLoader(o clientOptions) *loader {
	return &loader{
		count:       o.count,
		window:      make(chan struct{}, o.window),
		established: make(chan struct{}),
		state:       make([]probeState, o.count),
		seqOf:       make([]uint64, o.count),
		bySeq:       make(map[uint64]uint64, o.window),
		rtts:        make([]time.Duration, 0, o.count),
		all:         make(chan struct{}),
	}
}

func (l *loader) OnEstablished(context.Context, *session.Session) {
	l.once.Do(func() { close(l.established) })
}

// sending records message id outstanding, before Send: its echo may come
// before Send returns.
func (l *loader) sending(id uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state[id-1] = outstanding
	l.inflight = id
}

// sent records the sequence number message id took, Send returned, 0 on
// an Unsequenced flow: NotApplied may name it since, or come before Send
// returned.
func (l *loader) sent(id, seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	na := l.inflightNA
	l.inflight, l.inflightNA = 0, nil
	if seq == 0 || l.state[id-1] != outstanding {
		return
	}
	l.seqOf[id-1] = seq
	l.bySeq[seq] = id
	for _, r := range na {
		if inRange(seq, r[0], r[1]) {
			l.notApplied++
			l.settle(id, lost)
			return
		}
	}
}

// inRange tells whether seq is among from to from+count-1, without
// wrapping past the largest sequence number.
func inRange(seq, from, count uint64) bool {
	return seq >= from && seq-from < count
}

// settle records an outstanding message's end, under l.mu, freeing its
// slot of the window, and tells when every message is accounted for.
func (l *loader) settle(id uint64, st probeState) {
	l.state[id-1] = st
	if seq := l.seqOf[id-1]; seq != 0 {
		delete(l.bySeq, seq)
	}
	select {
	case <-l.window:
	default:
	}
	if !l.finished && uint64(l.echoes)+l.notApplied == uint64(l.count) {
		l.finished = true
		close(l.all)
	}
}

// OnMessage takes an echo, timed from its send to its last byte read.
func (l *loader) OnMessage(_ context.Context, _ *session.Session, msg []byte, in session.Inbound) error {
	id, sent, err := readProbe(msg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if id == 0 || id > uint64(l.count) || l.state[id-1] == unsent {
		return fmt.Errorf("echo of message %d, not sent", id)
	}
	if l.state[id-1] != outstanding {
		l.duplicates++
		return nil
	}
	l.echoes++
	l.rtts = append(l.rtts, in.Received.Sub(sent))
	l.settle(id, echoed)
	return nil
}

// OnNotApplied counts the messages the server never got, from to
// from+count-1, those outstanding: an Idempotent flow does not send them
// again, and neither does this client.
func (l *loader) OnNotApplied(_ context.Context, _ *session.Session, from uint64, count uint32, _ session.Inbound) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight != 0 {
		l.inflightNA = append(l.inflightNA, [2]uint64{from, uint64(count)})
	}
	var ids []uint64
	if uint64(count) <= uint64(len(l.bySeq)) {
		for seq := from; inRange(seq, from, uint64(count)); seq++ {
			if id, ok := l.bySeq[seq]; ok {
				ids = append(ids, id)
			}
		}
	} else {
		for seq, id := range l.bySeq {
			if inRange(seq, from, uint64(count)) {
				ids = append(ids, id)
			}
		}
	}
	for _, id := range ids {
		l.notApplied++
		l.settle(id, lost)
	}
	return nil
}

func (l *loader) result(s *session.Session, sent int, elapsed time.Duration) result {
	l.mu.Lock()
	defer l.mu.Unlock()
	rtts := slices.Clone(l.rtts)
	slices.Sort(rtts)
	return result{
		sent: sent, echoed: l.echoes, duplicates: l.duplicates, notApplied: l.notApplied,
		establishments: s.Stats().Establishments.Load(), elapsed: elapsed, rtts: rtts,
	}
}

// load runs a client: it negotiates a session, sends o.count messages, at
// most o.window of them awaiting their echo, then finishes its flow and
// returns once the session is finalized.
func load(ctx context.Context, o clientOptions, d transport.Dialer) (result, error) {
	if o.count < 1 || o.window < 1 {
		return result{}, errors.New("count and window must be positive")
	}
	if o.flow == fixp.None {
		return result{}, errors.New("an echo client needs a flow to send on, not None")
	}
	if _, err := (probe{size: o.size}).AppendTo(nil); err != nil {
		return result{}, err
	}
	l := newLoader(o)
	token := []byte(o.token)
	c, err := session.NewClient(session.ClientConfig{
		Config: session.Config{
			Handler:           l,
			KeepaliveInterval: o.keepalive,
			Logger:            o.logger,
		},
		Flow:        o.flow,
		ServerFlows: o.serverFlows,
		Store:       memstore.New(),
		Credentials: func(context.Context, session.CredentialsRequest) ([]byte, error) {
			return token, nil
		},
		ReconnectMin: o.reconnect,
		ReconnectMax: 30 * max(o.reconnect, time.Second),
	}, d)
	if err != nil {
		return result{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(rctx) }()
	ran := false
	defer func() {
		if !ran {
			stop()
			<-done
		}
	}()

	select {
	case <-l.established:
	case err := <-done:
		ran = true
		return result{}, fmt.Errorf("session ended before establishment: %w", err)
	case <-tctx.Done():
		return result{}, fmt.Errorf("not established: %w", tctx.Err())
	}

	s := c.Session()
	start := time.Now()
	sent := 0
	// ended reports the session's end, Run having returned.
	ended := func(err error) (result, error) {
		ran = true
		r := l.result(s, sent, time.Since(start))
		return r, fmt.Errorf("session ended, %d of %d echoed: %w", r.echoed, o.count, err)
	}
	for i := 1; i <= o.count; i++ {
		select {
		case l.window <- struct{}{}:
		case err := <-done:
			return ended(err)
		case <-tctx.Done():
			return l.result(s, sent, time.Since(start)), fmt.Errorf("sending: %w", tctx.Err())
		}
		l.sending(uint64(i))
		seq, err := s.Send(tctx, probe{id: uint64(i), sent: time.Now(), size: o.size})
		if err != nil {
			if errors.Is(err, session.ErrStopped) {
				return ended(<-done) // Run's error says why
			}
			return l.result(s, sent, time.Since(start)), fmt.Errorf("sending: %w", err)
		}
		l.sent(uint64(i), seq)
		sent++
	}
	select {
	case <-l.all:
	case err := <-done:
		return ended(err)
	case <-tctx.Done():
		r := l.result(s, sent, time.Since(start))
		return r, fmt.Errorf("%d of %d echoed: %w", r.echoed, o.count, tctx.Err())
	}
	r := l.result(s, sent, time.Since(start))

	// Finish our flow; the server finishes its own; Run returns once the
	// session is finalized.
	if err := s.Finish(tctx); err != nil {
		return r, fmt.Errorf("finishing: %w", err)
	}
	select {
	case err := <-done:
		ran = true
		if err != nil {
			return r, fmt.Errorf("finishing: %w", err)
		}
		return r, nil
	case <-tctx.Done():
		return r, fmt.Errorf("not finalized: %w", tctx.Err())
	}
}

// report writes r.
func report(w io.Writer, r result) {
	rate := 0.0
	if r.elapsed > 0 {
		rate = float64(r.echoed) / r.elapsed.Seconds()
	}
	fmt.Fprintf(w, "sent %d, echoed %d, duplicates %d, not applied %d, connections %d, in %v: %.0f echoes/s\n",
		r.sent, r.echoed, r.duplicates, r.notApplied, r.establishments, r.elapsed.Round(time.Microsecond), rate)
	if len(r.rtts) == 0 {
		return
	}
	q := func(p float64) time.Duration { return r.rtts[min(len(r.rtts)-1, int(p*float64(len(r.rtts))))] }
	fmt.Fprintf(w, "round trip: min %v, p50 %v, p90 %v, p99 %v, p99.9 %v, max %v\n",
		r.rtts[0], q(0.5), q(0.9), q(0.99), q(0.999), r.rtts[len(r.rtts)-1])
}
