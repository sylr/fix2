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
	// value ("9" for FIX 5.0 SP2...).
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
type Compatibility int

const (
	// CompatQuickFIX behaves as QuickFIX engines and their acceptance
	// tests expect, the most common counterparty behaviour.
	CompatQuickFIX Compatibility = iota
	// CompatStrict2020 follows the 2020 FIX Session Layer specification
	// where it differs, and refuses what QuickFIX lets through: a
	// ResendRequest for messages already processed, without PossDup, logs
	// out; a Logout received ahead of a gap is answered once the gap is
	// recovered; a Logon whose DefaultApplVerID has no dictionary is
	// refused.
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
	// Store persists the session; a new memstore.Store if nil.
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
	// Compatibility selects behaviours where specifications differ.
	Compatibility Compatibility
	// SkipChecks disables validations the dictionaries would run: all run
	// by default (see dictionary.Checks).
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
	MaxDeliveryStall time.Duration
	// ResetDrainTimeout bounds how long a sequence reset waits for the
	// messages of the previous sequence to be delivered, 5s if 0.
	ResetDrainTimeout time.Duration
	// MaxStash bounds the messages received ahead of a gap kept while the
	// gap is recovered, 10000 if 0.
	MaxStash int
	// AckProbe selects when to probe for acknowledgements.
	AckProbe AckProbe
	// AckProbeMinInterval is the least time between two probes.
	AckProbeMinInterval time.Duration
	// MaxTrackedReceipts bounds the messages whose acknowledgement is
	// tracked, 1<<20 if 0.
	MaxTrackedReceipts int
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

// errConfig wraps a Config.Validate error for New.
var errConfig = errors.New("session: invalid configuration")
