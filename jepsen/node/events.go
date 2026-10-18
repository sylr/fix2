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
	Inc  string    `json:"inc"`
	Kind string    `json:"kind"`

	// start
	Role   string   `json:"role,omitempty"`
	Store  string   `json:"store,omitempty"`
	Mutant []string `json:"mutant,omitempty"`

	// deliver, in, out, store, recovery-loss, logon, logout, send
	Gen     uint64 `json:"gen,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
	ClOrdID string `json:"clordid,omitempty"`
	PossDup bool   `json:"possdup,omitempty"`
	Digest  string `json:"digest,omitempty"`

	// in, out
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

	// recovery-loss
	Begin uint64 `json:"begin,omitempty"`
	End   uint64 `json:"end,omitempty"`

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

// Events appends events to a file of JSON lines, each with one write(2),
// so a process killed loses none it logged.
type Events struct {
	mu  sync.Mutex
	f   *os.File
	inc string
}

// OpenEvents opens dir/events-<inc>.jsonl for appending.
func OpenEvents(dir, inc string) (*Events, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events-"+inc+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Events{f: f, inc: inc}, nil
}

// Log writes e, stamped. A log that cannot be written ends the process: the
// checkers would read a history with holes.
func (e *Events) Log(ev Event) {
	ev.T, ev.Inc = time.Now(), e.inc
	b, err := json.Marshal(ev)
	if err != nil {
		panic(fmt.Sprintf("event log: %v", err))
	}
	b = append(b, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.f.Write(b); err != nil {
		panic(fmt.Sprintf("event log: %v", err))
	}
}

// Close closes the file.
func (e *Events) Close() error { return e.f.Close() }
