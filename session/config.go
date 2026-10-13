package session

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session/schedule"
	"github.com/sylr/fix2/session/store"
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

// Compatibility selects how the session behaves where the FIX specification
// changed or counterparties differ.
//
// In every mode, a Logon with ResetSeqNumFlag (141=Y) received while logged
// on is checked as an acceptor checks a first Logon (CompIDs, SendingTime,
// EncryptMethod, HeartBtInt, DefaultApplVerID), and its MsgSeqNum must be 1:
// an invalid one is rejected and logs out, resetting nothing. One received
// while logging out is ignored: FIX lets the session send nothing then but
// what a ResendRequest asks for. An acceptor refuses a first Logon without
// EncryptMethod (98) in every mode.
type Compatibility int

const (
	// CompatQuickFIX behaves as QuickFIX engines and their acceptance
	// tests expect, the most common counterparty behaviour. An acceptor
	// with ResetOnLogon answers a Logon without ResetSeqNumFlag without one,
	// even when the reset moved its sequence numbers: a counterparty that
	// did not reset logs out with "MsgSeqNum too low". An initiator accepts
	// a Logon answer without EncryptMethod (98), as venues whose
	// dictionaries make it optional send.
	CompatQuickFIX Compatibility = iota
	// CompatStrict2020 follows the 2020 FIX Session Layer specification
	// where it differs, and refuses what QuickFIX lets through: a
	// ResendRequest for messages already processed, without PossDup, logs
	// out; a Logout received ahead of a gap is answered once the gap is
	// recovered; a Logon whose DefaultApplVerID has no dictionary is
	// refused; an acceptor with ResetOnLogon announces a reset that moved
	// its sequence numbers (see Config.ResetOnLogon); an initiator refuses a
	// Logon answer without EncryptMethod (98), which the 2020 Session Layer
	// requires of every Logon.
	CompatStrict2020
)

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

// Leniency relaxes checks some counterparties fail.
type Leniency struct {
	// SequenceResetWithoutOrigSendingTime accepts a PossDup SequenceReset
	// without OrigSendingTime (122), which some venues send.
	SequenceResetWithoutOrigSendingTime bool
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
	// By default (CompatQuickFIX) an acceptor answers ResetSeqNumFlag only
	// when the Logon carries it, as QuickFIX's acceptance tests expect: one
	// whose sequence numbers had moved answers at MsgSeqNum 1 unannounced,
	// and a counterparty that did not reset logs out with "MsgSeqNum too
	// low". Under CompatStrict2020 it answers a Logon without
	// ResetSeqNumFlag with one, at MsgSeqNum 1, when the reset moved its
	// sequence numbers, either being past 1: the counterparty, which may
	// expect them to go on, resets in turn, its Logon left behind, and its
	// next message is 1. At the start of its sequence an acceptor has
	// nothing to announce: it answers as to any first Logon, and the Logon
	// counts. FIX 4.0, without ResetSeqNumFlag, never announces.
	ResetOnLogon bool
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
	// Compatibility selects behaviours where specifications differ or
	// counterparties do: CompatQuickFIX by default, which, among others,
	// accepts a Logon answer without EncryptMethod (98); CompatStrict2020
	// refuses it.
	Compatibility Compatibility
	// SkipChecks disables validations the dictionaries would run: all run
	// by default (see dictionary.Checks). Skipping
	// dictionary.CheckUnknownTags and dictionary.CheckUserDefinedTags
	// accepts tags the dictionaries do not define, as QuickFIX/J's
	// AllowUnknownMsgFields=Y and ValidateUserDefinedFields=N do. Skipping
	// dictionary.CheckGroupOrder accepts group entries whose fields are not
	// in their dictionary's order, as some counterparties send them, as
	// QuickFIX/J's ValidateUnorderedGroupFields=N does.
	SkipChecks dictionary.Checks
	// Leniency relaxes checks.
	Leniency Leniency

	// LogonTimeout is how long to wait for the counterparty's Logon, 10s if
	// 0. LogoutTimeout is how long to wait for its Logout reply, 2s if 0.
	LogonTimeout, LogoutTimeout time.Duration
	// WriteTimeout bounds each write, 10s if 0: a counterparty not reading
	// is disconnected.
	WriteTimeout time.Duration
	// MaxPendingMessages and MaxPendingBytes bound the messages waiting to
	// be written, 10000 and 16 MiB if 0. Send waits for room or fails with
	// ErrSlowConsumer.
	MaxPendingMessages, MaxPendingBytes int
	// FailWhenFull makes Send fail with ErrSlowConsumer rather than wait
	// for room.
	FailWhenFull bool
	// MaxPendingAge disconnects a counterparty that leaves a message
	// unwritten longer than this, 0 for no limit.
	MaxPendingAge time.Duration
	// MaxHeldMessages bounds the messages Send holds back while the session
	// resends messages, 10000 if 0.
	MaxHeldMessages int
	// DeliveryQueue bounds the received messages waiting for FromApp, 1024
	// if 0.
	DeliveryQueue int
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
	// MaxStash bounds the messages received ahead of a gap kept while the
	// gap is recovered, 10000 if 0. They are queued for delivery at once,
	// beyond DeliveryQueue, when the gap is filled or a SequenceReset
	// passes it (see MaxDeliveryStall).
	MaxStash int
	// AckProbe selects when to probe for acknowledgements.
	AckProbe AckProbe
	// AckProbeMinInterval is the least time between two probes.
	AckProbeMinInterval time.Duration
	// MaxTrackedReceipts bounds the messages whose acknowledgement is
	// tracked, 1<<20 if 0.
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
	def(&c.LogoutTimeout, 2*time.Second)
	def(&c.WriteTimeout, 10*time.Second)
	def(&c.ResetDrainTimeout, 5*time.Second)
	defInt(&c.MaxPendingMessages, 10000)
	defInt(&c.MaxPendingBytes, 16<<20)
	defInt(&c.MaxHeldMessages, 10000)
	defInt(&c.DeliveryQueue, 1024)
	defInt(&c.MaxStash, 10000)
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

// precision returns the timestamp precision for version p.
func (c *Config) precision(p *profile) fix2.TimePrecision {
	var want fix2.TimePrecision
	switch c.Timestamps {
	case SecondTimestamps:
		want = fix2.Seconds
	case MicrosecondTimestamps:
		want = fix2.Microseconds
	case NanosecondTimestamps:
		want = fix2.Nanoseconds
	default:
		want = fix2.Milliseconds
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
		{"LogonTimeout", c.LogonTimeout}, {"LogoutTimeout", c.LogoutTimeout}, {"WriteTimeout", c.WriteTimeout},
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
		{"DeliveryQueue", c.DeliveryQueue}, {"MaxStash", c.MaxStash}, {"MaxTrackedReceipts", c.MaxTrackedReceipts},
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
