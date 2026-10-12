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

// The scenarios: the initiator sends requests, at most a window of them
// unanswered, which the acceptor answers. Agents build and read every
// message through the code their engine generates from the dictionary.
const (
	// Orders: NewOrderSingle (35=D) messages, each acknowledged with an
	// ExecutionReport (35=8).
	Orders = "orders"
	// MarketData: MarketDataRequest (35=V) messages, each answered with a
	// MarketDataSnapshotFullRefresh (35=W) of the book.
	MarketData = "marketdata"
)

// The NewOrderSingle the initiator sends, ClOrdID (11) and TransactTime
// (60) apart, with the Parties group (NoPartyIDs, 453) of Parties. The
// acceptor reads its fields and its Parties, and answers it with an
// ExecutionReport acknowledging it: OrderID (37) and ExecID (17) numbered by
// the acceptor, ExecType (150) and OrdStatus (39) New, LeavesQty (151) the
// OrderQty, CumQty (14) and AvgPx (6) zero, ClOrdID, Symbol, Side, OrderQty,
// Price and the Parties echoed, and a TransactTime of its own. The
// initiator reads the ClOrdID and the Parties, which must be those it sent.
const (
	Symbol      = "EURUSD"
	Side        = "1" // Buy
	OrderQty    = 1000000
	OrdType     = "2" // Limit
	Price       = 1.08255
	TimeInForce = "0" // Day
	HandlInst   = "1" // Automated execution, no intervention

	ExecTypeNew  = "0"
	OrdStatusNew = "0"
)

// Checker checks the answers the initiator reads against what its scenario
// expects, reporting any difference as an anomaly.
type Checker struct {
	Scenario string
}

// Answers tells whether an answer of scenario s is one of the checker's,
// reporting it otherwise: a MarketDataSnapshotFullRefresh answers no order,
// an ExecutionReport no MarketDataRequest.
func (c Checker) Answers(s string) bool {
	if s != c.Scenario {
		Anomaly("an answer of the %s scenario in the %s scenario", s, c.Scenario)
		return false
	}
	return true
}

// Party checks the i-th Parties entry of an ExecutionReport.
func (Checker) Party(i int, id, source, role string) {
	if i >= len(Parties) || Parties[i] != (Party{ID: id, Source: source, Role: role}) {
		Anomaly("Parties entry %d: %s %s %s, want %+v", i, id, source, role, Parties[min(i, len(Parties)-1)])
	}
}

// Parties checks the number of Parties entries of an ExecutionReport.
func (Checker) Parties(n int) {
	if n != len(Parties) {
		Anomaly("%d Parties entries, want %d", n, len(Parties))
	}
}

// Entry checks the i-th NoMDEntries entry of a MarketDataSnapshotFullRefresh.
func (Checker) Entry(i int, typ string, px, size float64) {
	if i >= 2*MarketDepth {
		return
	}
	want := EntryTypes[i/MarketDepth]
	if l := Book[i/MarketDepth][i%MarketDepth]; typ != want || px != l.Px || size != l.Size {
		Anomaly("NoMDEntries entry %d: %s %v@%v, want %s %v@%v", i, typ, size, px, want, l.Size, l.Px)
	}
}

// Entries checks the number of NoMDEntries entries of a
// MarketDataSnapshotFullRefresh.
func (Checker) Entries(n int) {
	if n != 2*MarketDepth {
		Anomaly("%d NoMDEntries entries, want %d", n, 2*MarketDepth)
	}
}

// Party is an entry of the Parties group.
type Party struct {
	ID     string // PartyID (448)
	Source string // PartyIDSource (447)
	Role   string // PartyRole (452), an INT the Go engines' code types as a string
}

// Parties are the order's: its executing firm, its client and its trader,
// identified by proprietary codes.
var Parties = []Party{
	{ID: "BROKER-1", Source: "D", Role: "1"},  // Executing Firm
	{ID: "CLIENT-1", Source: "D", Role: "3"},  // Client ID
	{ID: "TRADER-1", Source: "D", Role: "11"}, // Initiating Trader
}

// The MarketDataRequest the initiator sends, MDReqID (262) apart: a
// snapshot (SubscriptionRequestType 263) of MarketDepth (264) levels of the
// bids and offers (NoMDEntryTypes 267, MDEntryType 269) of Symbol
// (NoRelatedSym 146, Symbol 55). The acceptor reads its fields and groups,
// and answers it with a MarketDataSnapshotFullRefresh: the MDReqID and the
// Symbol echoed, and NoMDEntries (268) entries of MDEntryType (269),
// MDEntryPx (270) and MDEntrySize (271), MarketDepth levels of each
// requested type, in the order requested: Book. The initiator reads the
// MDReqID and every entry, which must be the book's.
const (
	SubscriptionSnapshot = "0"
	MarketDepth          = 10
	EntryBid             = "0"
	EntryOffer           = "1"
)

// EntryTypes are the requested entry types.
var EntryTypes = []string{EntryBid, EntryOffer}

// Level is a level of the book.
type Level struct {
	Px   float64 // MDEntryPx (270)
	Size float64 // MDEntrySize (271)
}

// Book is the book the acceptor answers with, by entry type, in the order
// of EntryTypes, best first. No price ends with a zero, so that every engine
// writes it the same.
var Book = [2][MarketDepth]Level{
	{
		{1.08249, 1000000}, {1.08248, 2000000}, {1.08247, 3000000}, {1.08246, 4000000}, {1.08245, 5000000},
		{1.08244, 6000000}, {1.08243, 7000000}, {1.08242, 8000000}, {1.08241, 9000000}, {1.08239, 10000000},
	},
	{
		{1.08251, 1000000}, {1.08252, 2000000}, {1.08253, 3000000}, {1.08254, 4000000}, {1.08255, 5000000},
		{1.08256, 6000000}, {1.08257, 7000000}, {1.08258, 8000000}, {1.08259, 9000000}, {1.08261, 10000000},
	},
}

// Params are an agent's arguments:
//
//	acceptor  -port P [-dict FIX44.xml]
//	initiator -port P [-dict FIX44.xml] -scenario S -messages N -warmup K -window W
//	version
//
// The acceptor answers the requests of every scenario. Every agent validates
// with the dictionary, which the QuickFIX engines need to parse groups.
type Params struct {
	Role     string
	Port     int
	Dict     string
	Scenario string
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
	fs.StringVar(&p.Scenario, "scenario", Orders, "the requests the initiator sends: orders or marketdata")
	fs.IntVar(&p.Messages, "messages", 0, "requests to measure")
	fs.IntVar(&p.Warmup, "warmup", 0, "requests sent before measuring")
	fs.IntVar(&p.Window, "window", 1, "requests awaiting their answer at most")
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
	if p.Dict == "" {
		return p, errors.New("-dict is required")
	}
	if p.Role == "initiator" && (p.Messages <= 0 || p.Warmup < 0 || p.Window <= 0) {
		return p, errors.New("-messages and -window must be > 0, -warmup >= 0")
	}
	if p.Scenario != Orders && p.Scenario != MarketData {
		return p, fmt.Errorf("unknown scenario %q", p.Scenario)
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
	// Elapsed is from the first measured request sent to the last answered.
	ElapsedNs int64 `json:"elapsed_ns"`
	// CPUNs is the CPU time the initiator used meanwhile.
	CPUNs int64 `json:"cpu_ns"`
	// RTTNs are the round trips of the measured requests, in the order sent:
	// from before the request is built to the moment its answer is read.
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

// Tracker runs and measures the initiator's side: it sends requests keeping
// at most Window of them unanswered, and times each round trip. Request i is
// identified by the decimal i, its ClOrdID or MDReqID; the engine's handler
// calls Ack with the identifier of each answer.
type Tracker struct {
	p     Params
	start time.Time
	sent  []atomic.Int64 // when request i was sent, since start, +1; 0 once answered
	rtt   []int64        // round trips of the measured requests
	slots chan struct{}  // one per request awaiting its answer

	acked  atomic.Int64 // requests answered
	target atomic.Int64 // acked count ending the current phase
	end    atomic.Int64 // when the latest answer came
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

// Ack tells request id is answered.
func (t *Tracker) Ack(id string) {
	now := t.now()
	i, err := strconv.Atoi(id)
	if err != nil || i < 0 || i >= len(t.sent) {
		t.fail(fmt.Errorf("answer to unknown request %q", id))
		return
	}
	sent := t.sent[i].Swap(0)
	if sent == 0 {
		t.fail(fmt.Errorf("answer to request %d not awaiting one", i))
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

// Run sends the warm-up requests, tells the driver WARM, waits for its GO,
// sends and measures the requests, and tells the driver its RESULT. send
// sends the request identified by id.
func (t *Tracker) Run(ctl *Control, send func(id string) error) error {
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

// phase sends requests from to to, and waits for their answers.
func (t *Tracker) phase(from, to int, send func(string) error) error {
	if from == to {
		return nil
	}
	t.target.Store(int64(to))
	for i := from; i < to; i++ {
		select {
		case t.slots <- struct{}{}:
		case <-t.failed:
			return t.err
		case <-anomalous:
			return errAnomalous
		}
		id := strconv.Itoa(i)
		t.sent[i].Store(t.now())
		if err := send(id); err != nil {
			return fmt.Errorf("sending request %d: %w", i, err)
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
