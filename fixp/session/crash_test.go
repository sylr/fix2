package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/store"
	"github.com/sylr/fix2/fixp/store/storetest"
	"github.com/sylr/fix2/transport"
	"github.com/sylr/fix2/transport/mem"
)

// crashStore is a store whose writes are durable only once synced, as a
// disk's: crash returns the store a restarted process would open, without
// what was not synced. It fails its failAt-th write, the process then
// crashing: a session halts on a failing store.
type crashStore struct {
	mu       sync.Mutex
	sessions map[fixp.SessionID]*crashRecord
	writes   int
	failAt   int
	// commit makes the failing write happen before it reports failure: a
	// store whose answer was lost.
	commit bool
	closed bool
}

type crashImage struct {
	state store.State
	msgs  map[uint64][]byte
	low   uint64
}

func (i crashImage) clone() crashImage {
	return crashImage{state: i.state, msgs: maps.Clone(i.msgs), low: i.low}
}

type crashRecord struct {
	live, durable crashImage
}

var errInjected = errors.New("injected store failure")

func newCrashStore(failAt int) *crashStore {
	return &crashStore{sessions: map[fixp.SessionID]*crashRecord{}, failAt: failAt}
}

// crash returns what a restarted process opens: what was synced.
func (s *crashStore) crash() *crashStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := newCrashStore(0)
	for id, r := range s.sessions {
		c.sessions[id] = &crashRecord{live: r.durable.clone(), durable: r.durable.clone()}
	}
	return c
}

// write counts a write, failing the failAt-th.
func (s *crashStore) write() error {
	s.writes++
	if s.writes == s.failAt {
		return errInjected
	}
	return nil
}

func (s *crashStore) Create(ctx context.Context, meta store.Meta) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[meta.ID]; ok {
		return nil, store.ErrExists
	}
	if err := s.write(); err != nil {
		return nil, err
	}
	img := crashImage{state: store.State{Meta: meta, NextOutbound: 1}, msgs: map[uint64][]byte{}, low: 1}
	r := &crashRecord{live: img, durable: img.clone()} // durable at once
	s.sessions[meta.ID] = r
	return &crashSession{s: s, r: r}, nil
}

func (s *crashStore) Open(ctx context.Context, id fixp.SessionID) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.sessions[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &crashSession{s: s, r: r}, nil
}

func (s *crashStore) Sessions(ctx context.Context) ([]store.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var states []store.State
	for _, r := range s.sessions {
		if r.live.state.FinalizedAt.IsZero() {
			states = append(states, r.live.state)
		}
	}
	return states, nil
}

func (s *crashStore) Forget(ctx context.Context, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.sessions {
		if f := r.live.state.FinalizedAt; !f.IsZero() && f.Before(t) {
			delete(s.sessions, id)
		}
	}
	return nil
}

func (s *crashStore) Close() error { return nil }

type crashSession struct {
	s *crashStore
	r *crashRecord
}

// update applies a write to the live image, unless it fails.
func (cs *crashSession) update(f func(*crashImage) error) error {
	cs.s.mu.Lock()
	defer cs.s.mu.Unlock()
	img := cs.r.live.clone()
	if err := f(&img); err != nil {
		return err
	}
	err := cs.s.write()
	if err == nil || cs.s.commit {
		cs.r.live = img
	}
	return err
}

func (cs *crashSession) State() store.State {
	cs.s.mu.Lock()
	defer cs.s.mu.Unlock()
	return cs.r.live.state
}

func (cs *crashSession) SetEstablished(ctx context.Context, t time.Time) error {
	return cs.update(func(i *crashImage) error {
		if i.state.EstablishedAt.IsZero() {
			i.state.EstablishedAt = t
		}
		return nil
	})
}

func (cs *crashSession) SetNextOutbound(ctx context.Context, next uint64) error {
	return cs.update(func(i *crashImage) error {
		if next < i.state.NextOutbound {
			return store.ErrSequence
		}
		i.state.NextOutbound = next
		return nil
	})
}

func (cs *crashSession) Append(ctx context.Context, seq uint64, msg []byte) error {
	return cs.update(func(i *crashImage) error {
		if seq != i.state.NextOutbound {
			return store.ErrSequence
		}
		i.msgs[seq] = bytes.Clone(msg)
		i.state.NextOutbound = seq + 1
		return nil
	})
}

func (cs *crashSession) Range(ctx context.Context, begin, end uint64, fn func(seq uint64, msg []byte) error) error {
	cs.s.mu.Lock()
	if begin == 0 || end >= cs.r.live.state.NextOutbound {
		cs.s.mu.Unlock()
		return store.ErrSequence
	}
	var msgs [][]byte
	for seq := begin; seq <= end; seq++ {
		m, ok := cs.r.live.msgs[seq]
		if !ok {
			cs.s.mu.Unlock()
			return store.ErrEvicted
		}
		msgs = append(msgs, m)
	}
	cs.s.mu.Unlock()
	for i, m := range msgs {
		if err := fn(begin+uint64(i), m); err != nil {
			return err
		}
	}
	return nil
}

func (cs *crashSession) Evict(ctx context.Context, seq uint64) error {
	return cs.update(func(i *crashImage) error {
		for ; i.low < seq; i.low++ {
			delete(i.msgs, i.low)
		}
		return nil
	})
}

func (cs *crashSession) SetInbound(ctx context.Context, seq uint64) error {
	return cs.update(func(i *crashImage) error {
		if seq < i.state.Inbound {
			return store.ErrSequence
		}
		i.state.Inbound = seq
		return nil
	})
}

func (cs *crashSession) SetFinish(ctx context.Context, out, in store.Finish) error {
	return cs.update(func(i *crashImage) error {
		i.state.OutboundFinish, i.state.InboundFinish = out, in
		return nil
	})
}

func (cs *crashSession) Finalize(ctx context.Context, t time.Time) error {
	return cs.update(func(i *crashImage) error {
		if i.state.FinalizedAt.IsZero() {
			i.state.FinalizedAt = t
		}
		return nil
	})
}

func (cs *crashSession) Retire(ctx context.Context, t time.Time) error {
	return cs.update(func(i *crashImage) error {
		if i.state.FinalizedAt.IsZero() {
			i.state.FinalizedAt, i.state.Retired = t, true
		}
		return nil
	})
}

func (cs *crashSession) Sync(ctx context.Context) error {
	cs.s.mu.Lock()
	defer cs.s.mu.Unlock()
	err := cs.s.write()
	if err == nil || cs.s.commit {
		cs.r.durable = cs.r.live.clone()
	}
	return err
}

func (cs *crashSession) Close() error { return nil }

// crashStore passes the store suite, reopening being a crash.
func TestCrashStoreConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) store.Store { return newCrashStore(0) },
		func(_ *testing.T, s store.Store) store.Store { return s.(*crashStore).crash() })
}

// delivered records what a handler got: sequence number and body, in order.
type delivered struct {
	BaseHandler
	mu   sync.Mutex
	msgs []string
}

func (d *delivered) OnMessage(_ context.Context, _ *Session, msg []byte, in Inbound) error {
	d.mu.Lock()
	d.msgs = append(d.msgs, fmt.Sprintf("%d:%s", in.Seq, msg[8:]))
	d.mu.Unlock()
	return nil
}

func (d *delivered) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.msgs...)
}

// storedBodies returns the bodies a store holds durably, by sequence
// number: what a sender would retransmit after a crash.
func storedBodies(s *crashStore, id fixp.SessionID) map[uint64]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uint64]string{}
	for seq, m := range s.sessions[id].durable.msgs {
		out[seq] = string(m[8:])
	}
	return out
}

// A client crashing at its every store write, then restarting: on a
// Recoverable flow the server receives its messages once each, in order,
// none skipped; on an Idempotent one, at most once, in order.
func TestCrashClientAtEveryWrite(t *testing.T) {
	for _, flow := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/commit=%v", flow, commit), func(t *testing.T) { crashClient(t, flow, commit) })
		}
	}
}

func crashClient(t *testing.T, flow fixp.FlowType, commit bool) {
	const sends = 6
	for failAt := 1; ; failAt++ {
		passed := t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			l := mem.Listen("server")
			srvH := &delivered{}
			_, cancelSrv, srvDone := newServer(t, l, newCrashStore(0), srvH, fixp.Unsequenced, nil)
			defer func() { cancelSrv(); <-srvDone }()

			cs := newCrashStore(failAt)
			cs.commit = commit
			c := newClient(t, l, cs, BaseHandler{}, flow, nil)
			cancel, done := runClient(c)
			eventually(t, func() bool { st := c.Session().State(); return st == StateEstablished || st == StateStopped })
			for i := 1; i <= sends; i++ {
				if _, err := c.Session().Send(bg, appMsg{1, fmt.Sprintf("a%d", i)}); err != nil {
					break
				}
			}
			crashed := c.Session().State() == StateStopped
			if !crashed {
				eventually(t, func() bool { return len(srvH.got()) == sends || c.Session().State() == StateStopped })
				crashed = c.Session().State() == StateStopped
			}
			cancel()
			<-done
			if !crashed {
				skipRest = true
				return
			}

			// The process restarts on what was synced, and sends again.
			restarted := cs.crash()
			c = newClient(t, l, restarted, BaseHandler{}, flow, nil)
			cancel, done = runClient(c)
			defer func() { cancel(); <-done }()
			if c.Session().State() == StateUnnegotiated {
				return // crashed before its negotiation was recorded: a new session
			}
			id := c.Session().ID()
			eventually(t, func() bool { return c.Session().State() == StateEstablished })
			if _, err := c.Session().Send(bg, appMsg{1, "after"}); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool {
				g := srvH.got()
				return len(g) > 0 && g[len(g)-1] == fmt.Sprintf("%d:after", c.Session().NextSeqNo()-1)
			})
			want := storedBodies(restarted, id)
			got := srvH.got()
			var prev uint64
			for i, g := range got {
				var seq uint64
				var body string
				fmt.Sscanf(g, "%d:%s", &seq, &body)
				if flow == fixp.Recoverable && seq != uint64(i+1) || seq <= prev {
					t.Fatalf("delivered %q: not in sequence", got)
				}
				prev = seq
				// What was delivered before the crash was written, so
				// synced: the restarted store holds the same.
				if w, ok := want[seq]; ok && w != body && body != "after" {
					t.Fatalf("delivered %q, the store holds %q at %d", got, w, seq)
				}
			}
		})
		if !passed || skipRest || failAt > 200 {
			skipRest = false
			return
		}
	}
}

// skipRest ends a crash loop: the scenario made no write at that count.
var skipRest bool

// A Recoverable server crashing at its every store write, then
// restarting: the client receives its messages in order, at least once,
// none skipped.
func TestCrashServerAtEveryWrite(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) { crashServer(t, commit) })
	}
}

func crashServer(t *testing.T, commit bool) {
	const sends = 6
	for failAt := 1; ; failAt++ {
		passed := t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			// The server restarts on a new listener, which the client
			// dials.
			var cur atomic.Pointer[mem.Listener]
			cur.Store(mem.Listen("server"))
			dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) { return cur.Load().Dial(ctx) })
			ss := newCrashStore(failAt)
			ss.commit = commit
			srvH := newRecorder()
			srv, cancelSrv, srvDone := newServer(t, cur.Load(), ss, srvH, fixp.Recoverable, nil)
			cliH := &delivered{}
			c := newClient(t, dialer, newCrashStore(0), cliH, fixp.Unsequenced, nil)
			cancel, done := runClient(c)
			defer func() { cancel(); <-done }()
			// Established, or the server's session halted, or the
			// negotiation refused (the store failed at Create).
			var s *Session
			eventually(t, func() bool {
				s = srv.Session(c.Session().ID())
				if s != nil && (s.State() == StateEstablished || s.State() == StateStopped) {
					return true
				}
				return c.Session().State() == StateStopped
			})
			if s == nil {
				cancelSrv()
				<-srvDone
				return
			}
			for i := 1; i <= sends && s.State() == StateEstablished; i++ {
				if _, err := s.Send(bg, appMsg{2, fmt.Sprintf("b%d", i)}); err != nil {
					break
				}
			}
			crashed := s.State() == StateStopped
			if !crashed {
				eventually(t, func() bool { return len(cliH.got()) == sends || s.State() == StateStopped })
				crashed = s.State() == StateStopped
			}
			cancelSrv()
			<-srvDone
			if !crashed {
				skipRest = true
				return
			}
			restarted := ss.crash()
			cur.Store(mem.Listen("server"))
			srv, cancelSrv, srvDone = newServer(t, cur.Load(), restarted, newRecorder(), fixp.Recoverable, nil)
			defer func() { cancelSrv(); <-srvDone }()
			s = srv.Session(c.Session().ID())
			if s == nil {
				return // never recorded established, or retired
			}
			eventually(t, func() bool { return s.State() == StateEstablished })
			if _, err := s.Send(bg, appMsg{2, "after"}); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool {
				g := cliH.got()
				return len(g) > 0 && g[len(g)-1] == fmt.Sprintf("%d:after", s.NextSeqNo()-1)
			})
			got := cliH.got()
			for i, g := range got {
				var seq uint64
				fmt.Sscanf(g, "%d:", &seq)
				if seq != uint64(i+1) {
					t.Fatalf("delivered %q: not in sequence", got)
				}
			}
		})
		if !passed || skipRest || failAt > 200 {
			skipRest = false
			return
		}
	}
}

// A server receiving a client's flow, crashing at its every store write,
// its failing write lost or made: on a Recoverable flow it delivers in
// order, none skipped, at least once across the restart; on an Idempotent
// one, at most once.
func TestCrashReceiverAtEveryWrite(t *testing.T) {
	for _, flow := range []fixp.FlowType{fixp.Recoverable, fixp.Idempotent} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/commit=%v", flow, commit), func(t *testing.T) { crashReceiver(t, flow, commit) })
		}
	}
}

func crashReceiver(t *testing.T, flow fixp.FlowType, commit bool) {
	const sends = 6
	for failAt := 1; ; failAt++ {
		passed := t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			var cur atomic.Pointer[mem.Listener]
			cur.Store(mem.Listen("server"))
			dialer := transport.DialerFunc(func(ctx context.Context) (transport.Conn, error) { return cur.Load().Dial(ctx) })
			ss := newCrashStore(failAt)
			ss.commit = commit
			first := &delivered{}
			srv, cancelSrv, srvDone := newServer(t, cur.Load(), ss, first, fixp.Unsequenced, nil)
			c := newClient(t, dialer, newCrashStore(0), BaseHandler{}, flow, nil)
			cancel, done := runClient(c)
			defer func() { cancel(); <-done }()
			halted := func() bool {
				s := srv.Session(c.Session().ID())
				return s != nil && s.State() == StateStopped
			}
			eventually(t, func() bool {
				st := c.Session().State()
				return st == StateEstablished || st == StateStopped || halted()
			})
			if c.Session().State() == StateStopped {
				cancelSrv()
				<-srvDone
				return // the negotiation refused: the store failed at Create
			}
			for i := 1; i <= sends && !halted(); i++ {
				if _, err := c.Session().Send(bg, appMsg{1, fmt.Sprintf("c%d", i)}); err != nil {
					t.Fatal(err)
				}
			}
			eventually(t, func() bool { return len(first.got()) == sends || halted() })
			crashed := halted()
			cancelSrv()
			<-srvDone
			if !crashed {
				skipRest = true
				return
			}

			restarted := ss.crash()
			cur.Store(mem.Listen("server"))
			second := &delivered{}
			srv, cancelSrv, srvDone = newServer(t, cur.Load(), restarted, second, fixp.Unsequenced, nil)
			defer func() { cancelSrv(); <-srvDone }()
			if srv.Session(c.Session().ID()) == nil {
				return // never recorded established: retired, or a new session
			}
			eventually(t, func() bool { return c.Session().State() == StateEstablished })
			if _, err := c.Session().Send(bg, appMsg{1, "after"}); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool {
				g := second.got()
				return len(g) > 0 && g[len(g)-1] == fmt.Sprintf("%d:after", c.Session().NextSeqNo()-1)
			})

			seqs := func(d []string) []uint64 {
				var out []uint64
				for _, g := range d {
					var seq uint64
					fmt.Sscanf(g, "%d:", &seq)
					out = append(out, seq)
				}
				return out
			}
			a, b := seqs(first.got()), seqs(second.got())
			for _, run := range [][]uint64{a, b} {
				for i := 1; i < len(run); i++ {
					if flow == fixp.Recoverable && run[i] != run[i-1]+1 || run[i] <= run[i-1] {
						t.Fatalf("delivered %v then %v: not in sequence", a, b)
					}
				}
			}
			switch flow {
			case fixp.Recoverable:
				// The restarted resumes at its durable frontier: at most
				// where the first stopped, none skipped.
				if len(a) > 0 && a[0] != 1 || len(b) > 0 && (b[0] < 1 || len(a) > 0 && b[0] > a[len(a)-1]+1 || len(a) == 0 && b[0] != 1) {
					t.Fatalf("delivered %v then %v: a message skipped", a, b)
				}
			case fixp.Idempotent:
				seen := map[uint64]bool{}
				for _, seq := range append(a, b...) {
					if seen[seq] {
						t.Fatalf("delivered %v then %v: %d twice", a, b, seq)
					}
					seen[seq] = true
				}
			}
		})
		if !passed || skipRest || failAt > 200 {
			skipRest = false
			return
		}
	}
}
