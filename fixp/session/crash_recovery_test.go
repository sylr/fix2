package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/transport/mem"
)

// eventLog records what a server delivers, skips and finds missing, in
// order: "d<seq>", "l<from>+<count>", "g<from>+<count>". It skips what
// is lost.
type eventLog struct {
	BaseHandler
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	e.events = append(e.events, s)
	e.mu.Unlock()
}

func (e *eventLog) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func (e *eventLog) has(s string) bool {
	for _, x := range e.get() {
		if x == s {
			return true
		}
	}
	return false
}

func (e *eventLog) OnMessage(_ context.Context, _ *Session, _ []byte, in Inbound) error {
	e.add(fmt.Sprintf("d%d", in.Seq))
	return nil
}

func (e *eventLog) OnLost(_ context.Context, _ *Session, from, count uint64) error {
	e.add(fmt.Sprintf("l%d+%d", from, count))
	return nil
}

func (e *eventLog) OnGap(_ context.Context, _ *Session, from, count uint64) {
	e.add(fmt.Sprintf("g%d+%d", from, count))
}

// rawHandshake negotiates and establishes r, reporting whether both
// succeeded.
func rawHandshake(r *raw, flow fixp.FlowType, next codec.Optional[uint64]) bool {
	if h, _, _ := codec.ReadHeader(r.negotiate(flow, "user:alice")); h.TemplateId != codec.NegotiationResponseTemplateID {
		return false
	}
	h, _, _ := codec.ReadHeader(r.establish(1000, next, "user:alice"))
	return h.TemplateId == codec.EstablishmentAckTemplateID
}

// respond answers a server's RetransmitRequests as a peer that lost its
// messages up to lostTo and holds the others, bodies their numbers, until
// the connection ends.
func respond(r *raw, lostTo uint64) {
	go func() {
		for {
			r.c.SetReadDeadline(time.Now().Add(10 * time.Second))
			enc, msg, err := r.r.Next()
			if err != nil {
				return
			}
			_, h, _, err := codec.Classify(enc, msg)
			if err != nil || h.SchemaId != codec.SchemaID || h.TemplateId != codec.RetransmitRequestTemplateID {
				continue
			}
			var rr codec.RetransmitRequest
			rr.Decode(msg)
			if rr.FromSeqNo <= lostTo {
				f, _ := frame(&codec.RestransmitReject{SessionId: r.id, RequestTimestamp: rr.Timestamp, Code: codec.RetransmitRejectCodeOutOfRange})
				r.c.Write(f)
				continue
			}
			f, _ := frame(&codec.Retransmission{SessionId: r.id, RequestTimestamp: rr.Timestamp, NextSeqNo: rr.FromSeqNo, Count: rr.Count})
			r.c.Write(f)
			for seq := rr.FromSeqNo; seq < rr.FromSeqNo+uint64(rr.Count); seq++ {
				f, _ := frame(appMsg{1, fmt.Sprint(seq)})
				r.c.Write(f)
			}
		}
	}()
}

// crashRecovery runs a server receiving a raw client's flow, crashing at
// its every store write: first is the first instance's script, done tells
// it completed; the restarted server is established on with NextSeqNo
// next, second its script, which ends delivering next; check verifies
// both instances' events.
func crashRecovery(t *testing.T, commit bool, clientFlow, serverFlow fixp.FlowType,
	first func(r *raw), done func(*eventLog) bool, next uint64, second func(r *raw),
	check func(t *testing.T, a, b []string, st *crashStore, id fixp.SessionID)) {
	for failAt := 1; ; failAt++ {
		passed := t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			ss := newCrashStore(failAt)
			ss.commit = commit
			log1 := &eventLog{}
			l := mem.Listen("server")
			srv, cancel, srvDone := newServer(t, l, ss, log1, serverFlow, nil)
			r := dialRaw(t, l)
			halted := func() bool { s := srv.Session(r.id); return s != nil && s.State() == StateStopped }
			established := rawHandshake(r, clientFlow, codec.Some(uint64(1)))
			if established {
				first(r)
				eventually(t, func() bool { return done(log1) || halted() })
			}
			crashed := halted()
			cancel()
			<-srvDone
			if !crashed {
				if established {
					skipRest = true
				}
				return // or the negotiation refused: no session
			}
			l2 := mem.Listen("server")
			log2 := &eventLog{}
			restarted := ss.crash()
			srv2, cancel2, srvDone2 := newServer(t, l2, restarted, log2, serverFlow, nil)
			defer func() { cancel2(); <-srvDone2 }()
			if srv2.Session(r.id) == nil {
				return
			}
			r2 := dialRaw(t, l2)
			r2.id = r.id
			if h, _, _ := codec.ReadHeader(r2.establish(1000, codec.Some(next), "user:alice")); h.TemplateId != codec.EstablishmentAckTemplateID {
				t.Fatalf("restarted: establishment refused")
			}
			second(r2)
			eventually(t, func() bool { return log2.has(fmt.Sprintf("d%d", next)) })
			check(t, log1.get(), log2.get(), restarted, r.id)
		})
		if !passed || skipRest || failAt > 200 {
			skipRest = false
			return
		}
	}
}

// notAppliedRanges returns the sequence numbers the NotApplied messages a
// store holds report.
func notAppliedRanges(st *crashStore, id fixp.SessionID) map[uint64]bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[uint64]bool{}
	for _, m := range st.sessions[id].live.msgs {
		var na codec.NotApplied
		if _, err := na.Decode(m); err == nil {
			for seq := na.FromSeqNo; seq < na.FromSeqNo+uint64(na.Count); seq++ {
				out[seq] = true
			}
		}
	}
	return out
}

// An Idempotent flow's gap, NotApplied and the frontier past it, with the
// receiver crashing at every write: nothing delivered twice; on a
// Recoverable return flow, whose messages are stored, a NotApplied
// reports the gap, the frontier being past it.
func TestCrashIdempotentGapAtEveryWrite(t *testing.T) {
	for _, flow := range []fixp.FlowType{fixp.Idempotent, fixp.Recoverable} {
		for _, commit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/commit=%v", flow, commit), func(t *testing.T) { crashGap(t, flow, commit) })
		}
	}
}

func crashGap(t *testing.T, serverFlow fixp.FlowType, commit bool) {
	{
		{
			crashRecovery(t, commit, fixp.Idempotent, serverFlow,
				func(r *raw) {
					r.send(&codec.Sequence{NextSeqNo: 1}) // §4.8
					r.send(appMsg{1, "1"})
					r.send(&codec.Sequence{NextSeqNo: 4})
					r.send(appMsg{1, "4"})
					r.send(appMsg{1, "5"})
				},
				func(e *eventLog) bool { return e.has("d5") },
				6,
				func(r *raw) {
					r.send(&codec.Sequence{NextSeqNo: 6})
					r.send(appMsg{1, "6"})
				},
				func(t *testing.T, a, b []string, st *crashStore, id fixp.SessionID) {
					seen := map[string]bool{}
					for _, e := range append(a, b...) {
						if e[0] == 'd' && seen[e] {
							t.Fatalf("%v then %v: %s twice", a, b, e)
						}
						seen[e] = true
					}
					// Message 6 delivered, the frontier is past the gap: a
					// NotApplied stored reports 2 and 3.
					if serverFlow == fixp.Recoverable {
						if r := notAppliedRanges(st, id); !r[2] || !r[3] {
							t.Fatalf("%v then %v: NotApplied stored for %v, not 2 and 3", a, b, r)
						}
					}
				})
		}
	}
}

// A Recoverable flow's messages lost and skipped with the handler's
// consent, the receiver crashing at every write: in each process,
// deliveries and skips follow each other without a hole, and the
// restarted resumes no later than where the first stopped.
func TestCrashRecoverableLossAtEveryWrite(t *testing.T) {
	// position returns where an event starts and where the next follows.
	position := func(e string) (uint64, uint64) {
		var from, count uint64
		if e[0] == 'd' {
			fmt.Sscanf(e, "d%d", &from)
			return from, from + 1
		}
		fmt.Sscanf(e[1:], "%d+%d", &from, &count)
		return from, from + count
	}
	contiguous := func(t *testing.T, events []string) (uint64, uint64) {
		var start, next uint64
		for i, e := range events {
			from, after := position(e)
			if i == 0 {
				start = from
			} else if from != next {
				t.Fatalf("%v: a hole before %s", events, e)
			}
			next = after
		}
		return start, next
	}
	for _, commit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit=%v", commit), func(t *testing.T) {
			crashRecovery(t, commit, fixp.Recoverable, fixp.Unsequenced,
				// The real-time messages first, then the answers to the
				// server's requests: a peer's writes are in one order.
				func(r *raw) {
					r.send(&codec.Sequence{NextSeqNo: 3})
					r.send(appMsg{1, "3"})
					r.send(appMsg{1, "4"})
					respond(r, 2)
				},
				func(e *eventLog) bool { return e.has("d4") },
				5,
				func(r *raw) {
					r.send(&codec.Sequence{NextSeqNo: 5})
					r.send(appMsg{1, "5"})
					respond(r, 2)
				},
				func(t *testing.T, a, b []string, _ *crashStore, _ fixp.SessionID) {
					_, end := contiguous(t, a)
					start, _ := contiguous(t, b)
					if len(a) == 0 {
						end = 1
					}
					if start > end || start == 0 {
						t.Fatalf("%v then %v: the restarted skipped from %d to %d", a, b, end, start)
					}
				})
		})
	}
}
