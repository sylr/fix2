package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is a line of a node's event log. Fields a kind does not use are
// left out. Times are the node's real clock, T, and its session's clock,
// Now, which a skew moves.
type Event struct {
	T    time.Time `json:"t"`
	N    uint64    `json:"n"` // the event's place in its log, from 1: T may not rise with it
	Inc  string    `json:"inc"`
	Kind string    `json:"kind"`

	// start: Life names the store's lifetime, which a SQLite file keeps
	// across processes and a memstore ends with its process
	Role      string   `json:"role,omitempty"`
	Store     string   `json:"store,omitempty"`
	Life      string   `json:"life,omitempty"`
	ResetMode string   `json:"reset_mode,omitempty"` // "logon", "disconnect" or ""
	Mutant    []string `json:"mutant,omitempty"`

	// deliver, in, out, store, recovery-loss, logon, logout, send
	Gen     uint64 `json:"gen,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	ClOrdID string `json:"clordid,omitempty"`
	PossDup bool   `json:"possdup,omitempty"`
	Digest  string `json:"digest,omitempty"`

	// in, out: Hash identifies the frame's bytes, of every message
	Hash         string     `json:"hash,omitempty"`
	TestReqID    string     `json:"testreqid,omitempty"`
	Epoch        uint64     `json:"epoch,omitempty"`
	MsgType      string     `json:"msgtype,omitempty"`
	SendingTime  *time.Time `json:"sendingtime,omitempty"`
	Now          *time.Time `json:"now,omitempty"`
	GapFill      bool       `json:"gapfill,omitempty"`
	NewSeqNo     uint64     `json:"newseqno,omitempty"`
	Reset        bool       `json:"reset,omitempty"`
	RefSeqNum    uint64     `json:"refseqnum,omitempty"`
	RejectReason string     `json:"rejectreason,omitempty"`
	Resend       bool       `json:"resend,omitempty"`

	// store: Call is the store method, Phase "begin" or "end"
	Call   string `json:"call,omitempty"`
	Phase  string `json:"phase,omitempty"`
	Next   uint64 `json:"next,omitempty"`
	UpTo   uint64 `json:"upto,omitempty"`
	Cutoff uint64 `json:"cutoff,omitempty"` // Sync: the last Append of Gen completed when it began
	Target uint64 `json:"target,omitempty"`
	NewGen uint64 `json:"newgen,omitempty"` // Reset: the generation it started
	// range: Begin to End, and the holes it reported, in runs
	Holes []HoleRun `json:"holes,omitempty"`

	// deliver, admin: when its last byte was read; admin, a session
	// message the session handled (FromAdmin)
	Received *time.Time `json:"received,omitempty"`

	// recovery-loss: Gen its generation, Ours if our messages, else the
	// counterparty's it reset past
	Begin uint64 `json:"begin,omitempty"`
	End   uint64 `json:"end,omitempty"`
	Ours  bool   `json:"ours,omitempty"`

	// send: Level the receipt reached, Type the Jepsen outcome
	Level string `json:"level,omitempty"`
	Type  string `json:"type,omitempty"`

	// halt
	Watchdog bool `json:"watchdog,omitempty"`

	// rejected: counted as received, never to be delivered
	Counted bool `json:"counted,omitempty"`

	// clock: the session clock's offset from T, before and after
	WasMS    int64 `json:"was_ms,omitempty"`
	OffsetMS int64 `json:"offset_ms,omitempty"`

	Err string `json:"err,omitempty"`
}

// HoleRun is a run of positions a Range reported holes of one kind.
type HoleRun struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
	Kind string `json:"kind"`
}

// Events appends events to a file of JSON lines, each with one write(2),
// so a process killed loses none it logged.
type Events struct {
	mu  sync.Mutex
	f   *os.File
	inc string
	n   uint64
}

// OpenEvents opens dir/name for appending, its events stamped inc.
func OpenEvents(dir, name, inc string) (*Events, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Events{f: f, inc: inc}, nil
}

// Log writes e, stamped, under the lock: its time and place rise with the
// file's order. A log that cannot be written ends the process: the checkers
// would read a history with holes.
func (e *Events) Log(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	ev.T, ev.N, ev.Inc = time.Now(), e.n, e.inc
	b, err := json.Marshal(ev)
	if err != nil {
		panic(fmt.Sprintf("event log: %v", err))
	}
	b = append(b, '\n')
	if _, err := e.f.Write(b); err != nil {
		panic(fmt.Sprintf("event log: %v", err))
	}
}

// Close closes the file.
func (e *Events) Close() error { return e.f.Close() }
