package session

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/session/schedule"
	"github.com/sylr/fix2/fix/session/store"
)

// Dictionaries are the dictionaries a session validates messages with: one
// immutable set per session, sessions having sets of their own.
type Dictionaries struct {
	// Session is, for FIX 4.x, the dictionary of every message; for FIXT
	// 1.1, the transport dictionary (FIXT11.xml). nil validates nothing
	// beyond what the session layer itself checks.
	Session *dictionary.Dict
	// Apps are, for FIXT 1.1, the application dictionaries by ApplVerID
	// value ("9" for FIX 5.0 SP2...). A message's application version is
	// the ApplVerID (1128) of its standard header, the counterparty's
	// DefaultApplVerID without one. With Apps, a version without a
	// dictionary is rejected (SessionRejectReason 18); without, any is
	// accepted, and delivered as Inbound.ApplVerID.
	Apps map[string]*dictionary.Dict
	// DefaultApplVerID is, for FIXT 1.1, the application version of the
	// messages we send without ApplVerID (1128); we announce it in our Logon
	// (1137). The counterparty's Logon announces the default of its own.
	DefaultApplVerID string
}

// TimestampPrecision is the precision of the timestamps a session writes.
type TimestampPrecision int

const (
	// DefaultTimestamps is milliseconds, seconds before FIX 4.2.
	DefaultTimestamps TimestampPrecision = iota
	SecondTimestamps
	MillisecondTimestamps
	MicrosecondTimestamps
	NanosecondTimestamps
)

// Tolerations depart from the FIX specifications, as some counterparties
// need: a session follows them strictly unless told otherwise, each
// departure its own field, all off by default. docs/conformance.md gives
// the rule each relaxes. QuickFIXTolerations returns those QuickFIX's
// acceptance tests need.
//
// Whatever the tolerations, a Logon with ResetSeqNumFlag (141=Y) received
// while logged on is checked as an acceptor checks a first Logon (CompIDs,
// SendingTime, EncryptMethod, HeartBtInt, DefaultApplVerID), and its
// MsgSeqNum must be 1: an invalid one is rejected and logs out, resetting
// nothing. One received while logging out is ignored: FIX lets the session
// send nothing then but what a ResendRequest asks for. An acceptor refuses a
// first Logon without EncryptMethod (98).
type Tolerations struct {
	// TolerateLogonWithoutEncryptMethod lets an initiator accept a Logon
	// answer without EncryptMethod (98), which FIX requires of every Logon,
	// as venues whose dictionaries make it optional send (Binance's).
	TolerateLogonWithoutEncryptMethod bool
	// TolerateUnsupportedDefaultApplVerID accepts, for FIXT 1.1, a Logon
	// whose DefaultApplVerID (1137) has no dictionary in Dictionaries.Apps:
	// the counterparty's messages without ApplVerID (1128) are then
	// validated with our own default version. Refused otherwise.
	TolerateUnsupportedDefaultApplVerID bool
	// TolerateLogoutBeforeGapRecovery answers at once a Logout received
	// ahead of a gap, as QuickFIX does; FIX first recovers the gap, then
	// answers.
	TolerateLogoutBeforeGapRecovery bool
	// TolerateSequenceResetWithoutOrigSendingTime accepts a PossDup
	// SequenceReset without OrigSendingTime (122), which FIX requires of
	// every PossDup message, as some venues send.
	TolerateSequenceResetWithoutOrigSendingTime bool
	// TolerateResetLogonSeqNumNotOne accepts a first Logon, or Logon answer,
	// with ResetSeqNumFlag (141=Y) whose MsgSeqNum is not 1: the sequence
	// numbers reset, the messages before it are asked for as after any
	// Logon received too high. FIX starts a reset session at 1: refused
	// otherwise, as is, whatever the tolerations, a reset Logon on an
	// established session.
	TolerateResetLogonSeqNumNotOne bool
	// TolerateMalformedNextExpectedMsgSeqNum ignores, with a warning, a
	// NextExpectedMsgSeqNum (789) on Logon that is not a positive integer,
	// when the session honours 789 (see Config.NextExpectedMsgSeqNum), and
	// lets the session dictionary's checks pass it; the Logon is refused
	// otherwise.
	TolerateMalformedNextExpectedMsgSeqNum bool
	// TolerateNoHeartbeatAwaitingTestRequest sends no Heartbeat while our
	// TestRequest awaits its answer, as QuickFIX does and its acceptance
	// tests expect; FIX requires one in every idle interval.
	TolerateNoHeartbeatAwaitingTestRequest bool
	// TolerateTagLeadingZeros parses received messages whose tag numbers
	// have leading zeros, "035=D" read as MsgType (35), as
	// fix.ParseOptions.TolerateTagLeadingZeros does. FIX requires tag
	// numbers without them: such a message is otherwise rejected, as any
	// malformed tag number (SessionRejectReason 0), or disregarded as
	// garbled if received before logon or ahead of its MsgType or
	// MsgSeqNum.
	TolerateTagLeadingZeros bool
	// TolerateValidationFailures turns off validations the dictionaries
	// would run: all run by default (see dictionary.Checks). Tolerating
	// dictionary.CheckUnknownTags and dictionary.CheckUserDefinedTags
	// accepts tags the dictionaries do not define, as QuickFIX/J's
	// AllowUnknownMsgFields=Y and ValidateUserDefinedFields=N do;
	// dictionary.CheckGroupOrder, group entries whose fields are not in
	// their dictionary's order, as some counterparties send them, as
	// QuickFIX/J's ValidateUnorderedGroupFields=N does.
	TolerateValidationFailures dictionary.Checks
}

// QuickFIXTolerations returns the tolerations the session acceptance tests
// of QuickFIX (quickfix-go's _test directory) need, some of them failing
// without each: TolerateUnsupportedDefaultApplVerID,
// TolerateLogoutBeforeGapRecovery and TolerateNoHeartbeatAwaitingTestRequest.
// They also need ServeResendRequestSeqTooLow and UnannouncedResetOnLogon,
// choices the specifications leave open.
func QuickFIXTolerations() Tolerations {
	return Tolerations{
		TolerateUnsupportedDefaultApplVerID:    true,
		TolerateLogoutBeforeGapRecovery:        true,
		TolerateNoHeartbeatAwaitingTestRequest: true,
	}
}

// AckProbe selects when a session probes its counterparty to acknowledge
// sent messages (see Receipt).
type AckProbe int

const (
	// AckProbeLazy sends a TestRequest only while someone waits for an
	// acknowledgement.
	AckProbeLazy AckProbe = iota
	// AckProbeEager sends one after every write, coalesced.
	AckProbeEager
	// AckProbeNever never probes: messages are acknowledged only by
	// sequence progress evidence, if AckEvidence allows it.
	AckProbeNever
)

// Retention bounds the messages a session keeps in its store for resending.
// Every Interval, the session evicts the messages its counterparty
// acknowledged, but the last Keep and those younger than MinAge, and, beyond
// Max, older ones even unacknowledged. Keep and Max count sequence numbers,
// session-level messages included. A message evicted is gap-filled if the
// counterparty asks for it again: it is lost to it, which the session tells
// Handler.OnRecoveryLoss with ErrBeyondRetention.
//
// A message never written, or that a ResendRequest asked for and was not
// completely served, is never evicted, Max or not, until the counterparty
// acknowledges it, asks for messages after it, or a replay resends it: a
// connection that ends before writing what it queued holds evictions back
// until then, as do the messages a persistent store holds when the session
// starts.
//
// The counterparty acknowledges our messages up to a MsgSeqNum with
// LastMsgSeqNumProcessed (369) on the messages it sends, PossDup ones
// excepted, and with NextExpectedMsgSeqNum (789) on its Logon if the session
// honours it; the BeginSeqNo (7) of a ResendRequest moves the
// acknowledgement back, or sets the first. The last the counterparty sent
// counts, even if it is lower than an earlier one: a counterparty that
// restored an older state and says so keeps what it may ask for again. Its acknowledgements say
// it processed our messages, or, with some engines, received them, not that
// it can never ask for them again: Keep and MinAge are the margin for its
// crashes. With a counterparty that sends neither 369 nor 789, only Max
// evicts, past its first ResendRequest.
type Retention struct {
	// Interval is the time between two evictions, 0 to keep every message
	// until the sequence numbers are reset.
	Interval time.Duration
	// Keep is how many of the last sequence numbers are kept, acknowledged
	// or not.
	Keep int
	// MinAge keeps the messages sent less than MinAge ago, acknowledged or
	// not, to within an Interval.
	MinAge time.Duration
	// Max bounds the sequence numbers kept, 0 for no bound: older messages
	// are evicted, acknowledged or not, whatever Keep and MinAge. Up to an
	// eighth more are kept between evictions, which copy what stays.
	Max int
}

// Config configures a session. The zero value of each field is a sensible
// default, except ID and Handler, which are required.
type Config struct {
	// ID identifies the session.
	ID SessionID
	// Dictionaries validate messages.
	Dictionaries Dictionaries
	// HeartBtInt is the heartbeat interval an initiator asks for, whole
	// seconds, 30s if 0. An acceptor uses the counterparty's, if within
	// MinHeartBtInt and MaxHeartBtInt.
	HeartBtInt time.Duration
	// MinHeartBtInt and MaxHeartBtInt bound the heartbeat interval an
	// acceptor accepts, 1s and 300s if 0.
	MinHeartBtInt, MaxHeartBtInt time.Duration
	// Schedule tells when the session runs: it logs on only within its
	// windows, logs out when one ends, and starts each afresh, at sequence
	// number 1. Always, if nil.
	Schedule schedule.Schedule
	// Store persists the session; a new memstore.Store if nil. Received
	// messages are processed at least once: after a restart, those
	// processed but not yet recorded, a batch of 64 at most, are delivered
	// again. A failure to record them drops the connection and holds the
	// delivery of those received after them, kept in memory, until
	// recording works again, which the next connection waits for, their
	// generation is reset, or the session stops: a crash meanwhile loses
	// only what the counterparty can send again or report. One that leaves
	// the store in doubt halts the session (see Session.Err).
	Store store.Store
	// Logger receives the session's events; nothing is logged if nil.
	Logger *slog.Logger
	// Observer sees every message sent and received, if set.
	Observer MessageObserver
	// Handler receives the session's events and application messages.
	Handler Handler

	// Timestamps is the precision of SendingTime and OrigSendingTime.
	Timestamps TimestampPrecision
	// MaxLatency is how far SendingTime (52) may be from our clock, 120s if
	// 0; negative disables the check.
	MaxLatency time.Duration
	// ResetOnLogon resets sequence numbers at every logon: an initiator
	// sends ResetSeqNumFlag (141=Y), an acceptor resets on any Logon.
	//
	// An acceptor answers a Logon without ResetSeqNumFlag with one, at
	// MsgSeqNum 1, when the reset moved its sequence numbers, either being
	// past 1: the counterparty, which may expect them to go on, resets in
	// turn, its Logon left behind, and its next message is 1. At the start
	// of its sequence an acceptor has nothing to announce: it answers as to
	// any first Logon, and the Logon counts. FIX 4.0, without
	// ResetSeqNumFlag, never announces.
	ResetOnLogon bool
	// UnannouncedResetOnLogon makes an acceptor with ResetOnLogon answer
	// ResetSeqNumFlag only when the Logon carries it, as QuickFIX's
	// acceptance tests expect: one whose sequence numbers had moved answers
	// at MsgSeqNum 1 unannounced, and a counterparty that did not reset logs
	// out with "MsgSeqNum too low". FIX leaves it to the counterparties'
	// agreement.
	UnannouncedResetOnLogon bool
	// ResetOnLogout and ResetOnDisconnect reset sequence numbers after a
	// logout, or any disconnection.
	ResetOnLogout, ResetOnDisconnect bool
	// NextExpectedMsgSeqNum sends and honours NextExpectedMsgSeqNum (789)
	// on Logon, FIX 4.4 and later, if the counterparty agreed to.
	NextExpectedMsgSeqNum bool
	// LastMsgSeqNumProcessed sends LastMsgSeqNumProcessed (369) on every
	// message, FIX 4.2 and later.
	LastMsgSeqNumProcessed bool
	// ResendChunkSize asks for missing messages this many at a time, all at
	// once if 0.
	ResendChunkSize int
	// ServeResendRequestSeqTooLow serves a ResendRequest whose MsgSeqNum is
	// lower than expected, without PossDupFlag, before logging out, as
	// QuickFIX does and its acceptance tests expect; by default it logs out
	// unserved, as FIX recommends.
	ServeResendRequestSeqTooLow bool
	// Tolerations, all off by default, depart from the FIX specifications
	// as some counterparties need: QuickFIXTolerations for those QuickFIX's
	// acceptance tests need.
	Tolerations

	// LogonTimeout is how long to wait for the counterparty's Logon, 10s if
	// 0. LogoutTimeout is how long to wait for its Logout reply and how long
	// a resend under way may hold back our last Logout, once nothing more is
	// awaited: the resend is then cut short and the connection closed. If 0,
	// it is twice the heartbeat interval in effect, as FIX recommends: the
	// one agreed at logon, HeartBtInt before; 2s when that is 0.
	LogonTimeout, LogoutTimeout time.Duration
	// WriteTimeout bounds each write, 10s if 0: a counterparty not reading
	// is disconnected.
	WriteTimeout time.Duration
	// StoreTimeout is how long a store write made under the session's lock
	// (Append, SetNextTarget, SetNextSender, Reset, Truncate) may take, 30s
	// if 0. One taking longer, between once and one and a half times it,
	// halts the session without waiting for it: State and Err tell it at
	// once, the connection is closed, and Stop returns, the session
	// stopping once the write returns. It stays halted then (see Err). The
	// store's Close, when the session stops, is bounded alike. The store's
	// Open, when the session is created, gets a ctx ending after it: New,
	// and the Add that calls it, fail rather than wait longer, unless the
	// store ignores ctx, or finishes a write already started, as the SQLite
	// store's migration of its tables: New may then return later, and
	// succeed.
	StoreTimeout time.Duration
	// MaxPendingMessages and MaxPendingBytes bound the messages waiting to
	// be written, 10000 and 16 MiB if 0. Send waits for room or fails with
	// ErrSlowConsumer: a message is queued only if the bytes waiting and
	// its body fit, give or take its header, or if nothing is waiting.
	// Session-level messages may exceed them.
	MaxPendingMessages, MaxPendingBytes int
	// FailWhenFull makes Send fail with ErrSlowConsumer rather than wait
	// for room.
	FailWhenFull bool
	// MaxPendingAge disconnects a counterparty that leaves a message
	// unwritten longer than this, 0 for no limit.
	MaxPendingAge time.Duration
	// MaxHeldMessages bounds the messages Send holds back while the session
	// resends messages, 10000 if 0, and MaxPendingBytes their bytes, as it
	// does those waiting: Send fails with ErrReplayInProgress beyond.
	MaxHeldMessages int
	// DeliveryQueue and DeliveryQueueBytes bound the received messages
	// waiting for FromApp, 1024 and 64 MiB if 0, the bytes counted as the
	// buffers holding them: the reader waits for room before reading the
	// next one, which may take the queue past DeliveryQueueBytes by its
	// size.
	DeliveryQueue, DeliveryQueueBytes int
	// MaxDeliveryStall disconnects when FromApp has not returned for this
	// long while the delivery queue is full, max(30s, 2*HeartBtInt) if 0.
	// It measures how long the reader has waited for room, not FromApp's
	// progress: once a gap is filled or a SequenceReset passes it, the
	// messages held ahead of it, up to MaxStash, are queued at once, past
	// DeliveryQueue, and a handler slow to work through them can trip it.
	MaxDeliveryStall time.Duration
	// ResetDrainTimeout bounds how long a sequence reset waits for the
	// messages of the previous sequence to be delivered, 5s if 0.
	ResetDrainTimeout time.Duration
	// MaxStash and MaxStashBytes bound the messages received ahead of a
	// gap kept while the gap is recovered, 10000 and 64 MiB if 0; one
	// message is kept whatever its size. Those beyond are dropped, to be
	// asked for again. They are queued for delivery at once, beyond
	// DeliveryQueue, when the gap is filled or a SequenceReset passes it
	// (see MaxDeliveryStall).
	MaxStash, MaxStashBytes int
	// AckProbe selects when to probe for acknowledgements.
	AckProbe AckProbe
	// AckProbeMinInterval is the least time between two probes.
	AckProbeMinInterval time.Duration
	// MaxTrackedReceipts bounds the messages whose acknowledgement is
	// tracked, 1<<20 if 0. As many again, of the generations sequence
	// resets ended, keep what their receipts reached.
	MaxTrackedReceipts int
	// Retention evicts sent messages from the store, which must then
	// implement store.Truncater: none by default.
	Retention Retention
}

// withDefaults returns c with its zero fields set to their defaults.
func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	defInt := func(i *int, v int) {
		if *i == 0 {
			*i = v
		}
	}
	def(&c.HeartBtInt, 30*time.Second)
	def(&c.MinHeartBtInt, time.Second)
	def(&c.MaxHeartBtInt, 300*time.Second)
	def(&c.MaxLatency, 120*time.Second)
	def(&c.LogonTimeout, 10*time.Second)
	def(&c.WriteTimeout, 10*time.Second)
	def(&c.StoreTimeout, 30*time.Second)
	def(&c.ResetDrainTimeout, 5*time.Second)
	defInt(&c.MaxPendingMessages, 10000)
	defInt(&c.MaxPendingBytes, 16<<20)
	defInt(&c.MaxHeldMessages, 10000)
	defInt(&c.DeliveryQueue, 1024)
	defInt(&c.DeliveryQueueBytes, 64<<20)
	defInt(&c.MaxStash, 10000)
	defInt(&c.MaxStashBytes, 64<<20)
	defInt(&c.MaxTrackedReceipts, 1<<20)
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// deliveryStall returns MaxDeliveryStall for heartbeat interval hb.
func (c *Config) deliveryStall(hb time.Duration) time.Duration {
	if c.MaxDeliveryStall > 0 {
		return c.MaxDeliveryStall
	}
	return max(30*time.Second, 2*hb)
}

// logoutTimeout returns LogoutTimeout for heartbeat interval hb: twice hb if
// 0, 2s if both are.
func (c *Config) logoutTimeout(hb time.Duration) time.Duration {
	switch {
	case c.LogoutTimeout > 0:
		return c.LogoutTimeout
	case hb > 0:
		return 2 * hb
	}
	return 2 * time.Second
}

// parseOptions returns the options the session parses messages with, those
// received and those it stored.
func (c *Config) parseOptions() fix.ParseOptions {
	return fix.ParseOptions{TolerateTagLeadingZeros: c.TolerateTagLeadingZeros}
}

// precision returns the timestamp precision for version p.
func (c *Config) precision(p *profile) fix.TimePrecision {
	var want fix.TimePrecision
	switch c.Timestamps {
	case SecondTimestamps:
		want = fix.Seconds
	case MicrosecondTimestamps:
		want = fix.Microseconds
	case NanosecondTimestamps:
		want = fix.Nanoseconds
	default:
		want = fix.Milliseconds
	}
	return min(want, p.precision)
}

// ConfigError lists everything wrong with a Config.
type ConfigError struct {
	Session string   // the session ID, as String formats it
	Errs    []string // one per problem: "Field: what is wrong"
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("session %s: invalid configuration: %s", e.Session, strings.Join(e.Errs, "; "))
}

// Validate checks c and returns a *ConfigError listing every problem, or nil.
func (c *Config) Validate() error {
	var errs []string
	add := func(field, format string, args ...any) {
		errs = append(errs, field+": "+fmt.Sprintf(format, args...))
	}
	p, err := profileOf(c.ID.BeginString)
	if err != nil {
		add("ID.BeginString", "%v", err)
	}
	if c.ID.SenderCompID == "" {
		add("ID.SenderCompID", "required")
	}
	if c.ID.TargetCompID == "" {
		add("ID.TargetCompID", "required")
	}
	if c.Handler == nil {
		add("Handler", "required (embed session.BaseHandler for defaults)")
	}
	if v, ok := c.Schedule.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			add("Schedule", "%v", err)
		}
	}
	for _, d := range []struct {
		field string
		v     time.Duration
	}{{"HeartBtInt", c.HeartBtInt}, {"MinHeartBtInt", c.MinHeartBtInt}, {"MaxHeartBtInt", c.MaxHeartBtInt}} {
		if d.v < 0 || d.v%time.Second != 0 {
			add(d.field, "must be whole seconds >= 0, got %v", d.v)
		}
	}
	if c.MinHeartBtInt > 0 && c.MaxHeartBtInt > 0 && c.MinHeartBtInt > c.MaxHeartBtInt {
		add("MinHeartBtInt", "%v is above MaxHeartBtInt %v", c.MinHeartBtInt, c.MaxHeartBtInt)
	}
	for _, d := range []struct {
		field string
		v     time.Duration
	}{
		{"LogonTimeout", c.LogonTimeout}, {"LogoutTimeout", c.LogoutTimeout}, {"WriteTimeout", c.WriteTimeout}, {"StoreTimeout", c.StoreTimeout},
		{"MaxPendingAge", c.MaxPendingAge}, {"MaxDeliveryStall", c.MaxDeliveryStall},
		{"ResetDrainTimeout", c.ResetDrainTimeout}, {"AckProbeMinInterval", c.AckProbeMinInterval},
	} {
		if d.v < 0 {
			add(d.field, "must be >= 0, got %v", d.v)
		}
	}
	for _, n := range []struct {
		field string
		v     int
	}{
		{"ResendChunkSize", c.ResendChunkSize}, {"MaxPendingMessages", c.MaxPendingMessages},
		{"MaxPendingBytes", c.MaxPendingBytes}, {"MaxHeldMessages", c.MaxHeldMessages},
		{"DeliveryQueue", c.DeliveryQueue}, {"DeliveryQueueBytes", c.DeliveryQueueBytes}, {"MaxStash", c.MaxStash}, {"MaxStashBytes", c.MaxStashBytes}, {"MaxTrackedReceipts", c.MaxTrackedReceipts},
	} {
		if n.v < 0 {
			add(n.field, "must be >= 0, got %d", n.v)
		}
	}
	c.validateRetention(add)
	if p != nil {
		d := c.Dictionaries
		if p.fixt {
			if d.DefaultApplVerID == "" {
				add("Dictionaries.DefaultApplVerID", "required for FIXT.1.1")
			} else if len(d.Apps) > 0 && d.Apps[d.DefaultApplVerID] == nil {
				add("Dictionaries.DefaultApplVerID", "%q has no dictionary in Apps (have %s)", d.DefaultApplVerID, strings.Join(slices.Sorted(maps.Keys(d.Apps)), ", "))
			}
		} else if len(d.Apps) > 0 || d.DefaultApplVerID != "" {
			add("Dictionaries", "Apps and DefaultApplVerID are for FIXT.1.1, not %s", c.ID.BeginString)
		}
		if c.NextExpectedMsgSeqNum && !p.nextExpected {
			add("NextExpectedMsgSeqNum", "%s has no NextExpectedMsgSeqNum (789): FIX.4.4 or FIXT.1.1 required", c.ID.BeginString)
		}
		if c.LastMsgSeqNumProcessed && !p.lastProc {
			add("LastMsgSeqNumProcessed", "%s has no LastMsgSeqNumProcessed (369): FIX.4.2 or later required", c.ID.BeginString)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &ConfigError{Session: c.ID.String(), Errs: errs}
}

// validateRetention checks c.Retention.
func (c *Config) validateRetention(add func(field, format string, args ...any)) {
	r := c.Retention
	if r.Interval < 0 {
		add("Retention.Interval", "must be >= 0, got %v", r.Interval)
	}
	if r.MinAge < 0 {
		add("Retention.MinAge", "must be >= 0, got %v", r.MinAge)
	}
	if r.Keep < 0 {
		add("Retention.Keep", "must be >= 0, got %d", r.Keep)
	}
	if r.Max < 0 {
		add("Retention.Max", "must be >= 0, got %d", r.Max)
	}
	if r.Interval == 0 && (r.Keep != 0 || r.MinAge != 0 || r.Max != 0) {
		add("Retention.Interval", "required to evict messages: 0 keeps every message")
	}
	if r.Max > 0 && r.Keep > r.Max {
		add("Retention.Keep", "%d is above Retention.Max %d", r.Keep, r.Max)
	}
	if _, ok := c.Store.(store.Truncater); r.Interval > 0 && c.Store != nil && !ok {
		add("Retention", "the store, %T, cannot evict messages: it does not implement store.Truncater", c.Store)
	}
}

// errConfig wraps a Config.Validate error for New.
var errConfig = errors.New("session: invalid configuration")
