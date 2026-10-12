// Package scenario holds what the Go agents share: the scenario's session
// and messages, the arguments agents take, the line protocol they speak with
// the driver, and the initiator's measurement. An agent of another language
// implements the same; the README describes it.
package scenario

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The session: FIX 4.4, the initiator INITIATOR logs on to the acceptor
// ACCEPTOR on 127.0.0.1, resetting sequence numbers.
const (
	BeginString     = "FIX.4.4"
	InitiatorCompID = "INITIATOR"
	AcceptorCompID  = "ACCEPTOR"
	Host            = "127.0.0.1"
	HeartBtInt      = 30 // seconds
)

// The NewOrderSingle (35=D) the initiator sends, ClOrdID (11) and
// TransactTime (60) apart. The acceptor answers each with an
// ExecutionReport (35=8) acknowledging it: OrderID (37) and ExecID (17)
// numbered by the acceptor, ExecType (150) and OrdStatus (39) New, LeavesQty
// (151) the OrderQty, CumQty (14) and AvgPx (6) zero, ClOrdID, Symbol,
// Side, OrderQty and Price echoed, and a TransactTime of its own.
const (
	Symbol      = "EURUSD"
	Side        = "1" // Buy
	OrderQty    = "1000000"
	OrdType     = "2" // Limit
	Price       = "1.08255"
	TimeInForce = "0" // Day
	HandlInst   = "1" // Automated execution, no intervention

	ExecTypeNew  = "0"
	OrdStatusNew = "0"
	Zero         = "0"
)

// Params are an agent's arguments:
//
//	acceptor  -port P [-dict FIX44.xml]
//	initiator -port P [-dict FIX44.xml] -messages N -warmup K -window W
//	version
//
// Without -dict, the agent validates nothing beyond what its engine does
// without a dictionary.
type Params struct {
	Role     string
	Port     int
	Dict     string
	Messages int
	Warmup   int
	Window   int
}

// ParseArgs parses an agent's arguments, os.Args[1:].
func ParseArgs(args []string) (Params, error) {
	var p Params
	if len(args) == 0 {
		return p, errors.New("usage: agent acceptor|initiator|version [flags]")
	}
	p.Role = args[0]
	fs := flag.NewFlagSet(p.Role, flag.ContinueOnError)
	fs.IntVar(&p.Port, "port", 0, "port the acceptor listens on")
	fs.StringVar(&p.Dict, "dict", "", "QuickFIX FIX 4.4 dictionary to validate with")
	fs.IntVar(&p.Messages, "messages", 0, "orders to measure")
	fs.IntVar(&p.Warmup, "warmup", 0, "orders sent before measuring")
	fs.IntVar(&p.Window, "window", 1, "orders awaiting their acknowledgement at most")
	if err := fs.Parse(args[1:]); err != nil {
		return p, err
	}
	switch p.Role {
	case "version":
		return p, nil
	case "acceptor", "initiator":
	default:
		return p, fmt.Errorf("unknown role %q", p.Role)
	}
	if p.Port <= 0 {
		return p, errors.New("-port is required")
	}
	if p.Role == "initiator" && (p.Messages <= 0 || p.Warmup < 0 || p.Window <= 0) {
		return p, errors.New("-messages and -window must be > 0, -warmup >= 0")
	}
	return p, nil
}

// Version returns the version of module the agent is built with, for the
// version role. A module replaced by a directory, as fix2 is, is told by the
// repository's revision.
func Version(module string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "unknown", ""
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && len(s.Value) >= 12:
			rev = s.Value[:12]
		case s.Key == "vcs.modified" && s.Value == "true":
			dirty = "-dirty"
		}
	}
	rev += dirty
	for _, d := range bi.Deps {
		if d.Path != module {
			continue
		}
		if r := d.Replace; r != nil && (r.Version == "" || r.Version == "(devel)") {
			return "git-" + rev
		}
		return d.Version
	}
	return "unknown"
}

// Addr returns the acceptor's address.
func (p Params) Addr() string { return Host + ":" + strconv.Itoa(p.Port) }

// Control reads the driver's commands, one per line on stdin.
type Control struct{ lines chan string }

// NewControl starts reading stdin.
func NewControl() *Control {
	c := &Control{lines: make(chan string)}
	go func() {
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			c.lines <- s.Text()
		}
		// A read error ends the commands as the end of stdin does.
		close(c.lines)
	}()
	return c
}

// Next returns the next command, false once stdin is closed.
func (c *Control) Next() (string, bool) {
	l, ok := <-c.lines
	return l, ok
}

// Say writes a line to the driver.
func Say(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

// ServeAcceptor tells the driver the acceptor is ready, then answers its
// commands until stdin is closed: USAGE with the CPU time the process
// used.
func ServeAcceptor(ctl *Control) error {
	Say("READY")
	for {
		cmd, ok := ctl.Next()
		if !ok {
			return nil
		}
		switch cmd {
		case "USAGE":
			Say("USAGE %d", CPUTime().Nanoseconds())
		default:
			return fmt.Errorf("unknown command %q", cmd)
		}
	}
}

// CPUTime returns the user and system CPU time the process used.
func CPUTime() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// Result is what the initiator measured.
type Result struct {
	// Elapsed is from the first measured order sent to the last
	// acknowledged.
	ElapsedNs int64 `json:"elapsed_ns"`
	// CPUNs is the CPU time the initiator used meanwhile.
	CPUNs int64 `json:"cpu_ns"`
	// RTTNs are the round trips of the measured orders, in the order sent:
	// from before the order is handed to the engine to the moment its
	// ExecutionReport is delivered to the application.
	RTTNs []int64 `json:"rtt_ns"`
}

// What invalidates a run: Rejects, unexpected messages.
var (
	anomalies    atomic.Int64
	anomalyOnce  sync.Once
	anomalous    = make(chan struct{}) // closed at the first anomaly
	errAnomalous = errors.New("anomaly: the run is invalid")
)

// Anomaly reports something invalidating the run: the agent will exit with
// status 3, an initiator's Tracker at once.
func Anomaly(format string, args ...any) {
	anomalies.Add(1)
	fmt.Fprintf(os.Stderr, "anomaly: "+format+"\n", args...)
	anomalyOnce.Do(func() { close(anomalous) })
}

// Exit exits with status 3 after an anomaly, 1 if err is set, 0 otherwise.
func Exit(err error) {
	if err != nil && !errors.Is(err, errAnomalous) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if n := anomalies.Load(); n > 0 {
		fmt.Fprintf(os.Stderr, "%d anomalies\n", n)
		os.Exit(3)
	}
	os.Exit(0)
}

// Tracker runs and measures the initiator's side: it sends orders keeping at
// most Window of them unacknowledged, and times each round trip. The
// engine's handler calls Ack with the ClOrdID of each ExecutionReport.
type Tracker struct {
	p     Params
	start time.Time
	sent  []atomic.Int64 // when order i was sent, since start, +1; 0 once acknowledged
	rtt   []int64        // round trips of the measured orders
	slots chan struct{}  // one per order awaiting its acknowledgement

	acked  atomic.Int64 // orders acknowledged
	target atomic.Int64 // acked count ending the current phase
	end    atomic.Int64 // when the phase's last order was acknowledged
	done   chan struct{}

	errOnce sync.Once
	err     error
	failed  chan struct{}
}

// NewTracker returns a tracker for the initiator of p.
func NewTracker(p Params) *Tracker {
	return &Tracker{
		p:      p,
		start:  time.Now(),
		sent:   make([]atomic.Int64, p.Warmup+p.Messages),
		rtt:    make([]int64, p.Messages),
		slots:  make(chan struct{}, p.Window),
		done:   make(chan struct{}, 1),
		failed: make(chan struct{}),
	}
}

func (t *Tracker) now() int64 { return int64(time.Since(t.start)) + 1 }

// fail fails the run, even once measured: the agent exits with status 3.
func (t *Tracker) fail(err error) {
	Anomaly("%v", err)
	t.errOnce.Do(func() {
		t.err = fmt.Errorf("%w: %v", errAnomalous, err)
		close(t.failed)
	})
}

// Ack tells order clOrdID is acknowledged.
func (t *Tracker) Ack(clOrdID []byte) {
	now := t.now()
	i, err := strconv.Atoi(string(clOrdID))
	if err != nil || i < 0 || i >= len(t.sent) {
		t.fail(fmt.Errorf("ExecutionReport for unknown ClOrdID %q", clOrdID))
		return
	}
	sent := t.sent[i].Swap(0)
	if sent == 0 {
		t.fail(fmt.Errorf("ExecutionReport for ClOrdID %d not awaiting one", i))
		return
	}
	if j := i - t.p.Warmup; j >= 0 {
		t.rtt[j] = now - sent
	}
	// Engines deliver on one goroutine, but the last acknowledgement counted
	// need not be the latest if one did not.
	for end := t.end.Load(); now > end && !t.end.CompareAndSwap(end, now); end = t.end.Load() {
	}
	<-t.slots
	if t.acked.Add(1) == t.target.Load() {
		t.done <- struct{}{}
	}
}

// Run sends the warm-up orders, tells the driver WARM, waits for its GO,
// sends and measures the orders, and tells the driver its RESULT. send sends
// the order of ClOrdID clOrdID.
func (t *Tracker) Run(ctl *Control, send func(clOrdID []byte) error) error {
	if err := t.phase(0, t.p.Warmup, send); err != nil {
		return fmt.Errorf("warm-up: %w", err)
	}
	Say("WARM")
	if cmd, ok := ctl.Next(); !ok || cmd != "GO" {
		return fmt.Errorf("expected GO, got %q", cmd)
	}
	cpu := CPUTime()
	begin := t.now()
	if err := t.phase(t.p.Warmup, t.p.Warmup+t.p.Messages, send); err != nil {
		return err
	}
	res := Result{ElapsedNs: t.end.Load() - begin, CPUNs: (CPUTime() - cpu).Nanoseconds(), RTTNs: t.rtt}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	Say("RESULT %s", b)
	return nil
}

// phase sends orders from to to, and waits for their acknowledgements.
func (t *Tracker) phase(from, to int, send func([]byte) error) error {
	if from == to {
		return nil
	}
	t.target.Store(int64(to))
	var id []byte
	for i := from; i < to; i++ {
		select {
		case t.slots <- struct{}{}:
		case <-t.failed:
			return t.err
		case <-anomalous:
			return errAnomalous
		}
		id = strconv.AppendInt(id[:0], int64(i), 10)
		t.sent[i].Store(t.now())
		if err := send(id); err != nil {
			return fmt.Errorf("sending order %d: %w", i, err)
		}
	}
	select {
	case <-t.done:
		return nil
	case <-t.failed:
		return t.err
	case <-anomalous:
		return errAnomalous
	}
}
