package session

import (
	"sync"
)

// entryKind tells what a queued frame is.
type entryKind uint8

const (
	// entryApp is an application message: it outlives the connection it
	// was queued under, waiting for the next one if needed.
	entryApp entryKind = iota
	// entrySession is a session message of one connection, dropped with it.
	entrySession
	// entryTerminate is our graceful Terminate: the writer writes nothing
	// after it.
	entryTerminate
	// entryEndsNumbering is a session message ending our numbering, such
	// as a RetransmitRequest (§3.5.4).
	entryEndsNumbering
	// entryRetransmit is a batch to retransmit, read from the store when
	// its turn comes.
	entryRetransmit
)

type entry struct {
	frame []byte
	kind  entryKind
	seq   uint64        // an application message's on a sequenced flow, else 0
	conn  *conn         // a session message's connection
	job   retransmitJob // an entryRetransmit's
	// answer marks an answer to a request of the peer's, counted in its
	// connection's answers until written.
	answer bool
	// finishing marks our FinishedSending; finRecv our FinishedReceiving.
	finishing, finRecv bool
}

// pushFinish queues our FinishedSending (finRecv false) or
// FinishedReceiving (finRecv true) on connection c.
func (q *queue) pushFinish(c *conn, frame []byte, finRecv bool) {
	q.mu.Lock()
	q.entries = append(q.entries, entry{frame: frame, kind: entryEndsNumbering, conn: c, finishing: !finRecv, finRecv: finRecv})
	q.mu.Unlock()
	notify(q.signal)
}

// wakeRoom wakes every Send waiting for room, to look again.
func (q *queue) wakeRoom() {
	q.mu.Lock()
	close(q.room)
	q.room = make(chan struct{})
	q.mu.Unlock()
}

// queue holds the frames a session's writer is to write, in order.
// Application messages count against the session's bounds; session
// messages do not, being few and bounded by the protocol.
type queue struct {
	mu      sync.Mutex
	entries []entry
	head    int
	msgs    int // application messages queued
	bytes   int // their bytes
	// next is the sequence number the next application message sent will
	// take, under mu: assignment and queueing are one step for the writer,
	// which reads the next number to write from here.
	next uint64
	// signal wakes the writer: entries were added.
	signal chan struct{}
	// room is closed, and replaced, when application messages leave the
	// queue: every Send waiting for room wakes and looks again.
	room chan struct{}
}

func newQueue() *queue {
	return &queue{signal: make(chan struct{}, 1), room: make(chan struct{}), next: 1}
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// hasRoom reports whether an application message of n bytes fits, and if
// not returns the channel closed once room may have been made: taken with
// the answer, no wakeup is missed. One longer than maxBytes never fits:
// Send refuses it first.
func (q *queue) hasRoom(n, maxMsgs, maxBytes int) (bool, <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.msgs < maxMsgs && q.bytes+n <= maxBytes, q.room
}

// pending returns the application messages queued.
func (q *queue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.msgs
}

// pushApp queues an application message. seq is its sequence number on a
// sequenced flow, 0 otherwise.
func (q *queue) pushApp(frame []byte, seq uint64) {
	q.mu.Lock()
	q.entries = append(q.entries, entry{frame: frame, kind: entryApp, seq: seq})
	q.msgs++
	q.bytes += len(frame)
	if seq != 0 {
		q.next = seq + 1
	}
	q.mu.Unlock()
	notify(q.signal)
}

// pushSession queues a session message of connection c.
func (q *queue) pushSession(c *conn, frame []byte, kind entryKind) {
	q.mu.Lock()
	q.entries = append(q.entries, entry{frame: frame, kind: kind, conn: c})
	q.mu.Unlock()
	notify(q.signal)
}

// pushRetransmit queues a retransmission for connection c, an answer.
func (q *queue) pushRetransmit(c *conn, j retransmitJob) {
	q.mu.Lock()
	q.entries = append(q.entries, entry{kind: entryRetransmit, conn: c, job: j, answer: true})
	q.mu.Unlock()
	notify(q.signal)
}

// pushAnswer queues a session message answering a request of the peer's on
// connection c; it ends our numbering.
func (q *queue) pushAnswer(c *conn, frame []byte) {
	q.mu.Lock()
	q.entries = append(q.entries, entry{frame: frame, kind: entryEndsNumbering, conn: c, answer: true})
	q.mu.Unlock()
	notify(q.signal)
}

// pop removes and returns the first entry c may write: an application
// message, or a session message of c's. Session messages of other
// connections are dropped on the way.
func (q *queue) pop(c *conn) (entry, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.head < len(q.entries) {
		e := q.entries[q.head]
		q.entries[q.head] = entry{}
		q.head++
		if e.kind != entryApp && e.conn != c {
			continue
		}
		if e.kind == entryApp {
			q.msgs--
			q.bytes -= len(e.frame)
			close(q.room)
			q.room = make(chan struct{})
		}
		q.compact()
		return e, true
	}
	q.compact()
	return entry{}, false
}

// compact reclaims the room of popped entries.
func (q *queue) compact() {
	if q.head == len(q.entries) {
		q.entries, q.head = q.entries[:0], 0
	} else if q.head > 64 && q.head*2 > len(q.entries) {
		n := copy(q.entries, q.entries[q.head:])
		clear(q.entries[n:])
		q.entries, q.head = q.entries[:n], 0
	}
}

// nextToWrite returns the sequence number of the first application
// message queued, or the next one to be assigned if none is: what a
// heartbeat Sequence announces.
func (q *queue) nextToWrite() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.entries[q.head:] {
		if e.kind == entryApp && e.seq != 0 {
			return e.seq
		}
	}
	return q.next
}

// setNext sets the next sequence number to assign, as the store says.
func (q *queue) setNext(next uint64) {
	q.mu.Lock()
	q.next = next
	q.mu.Unlock()
}

// nextSeq returns the next sequence number to assign.
func (q *queue) nextSeq() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.next
}
