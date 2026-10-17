package session

import (
	"bytes"
	"fmt"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// BeginStrings of the FIX versions sessions support.
const (
	FIX40  = "FIX.4.0"
	FIX41  = "FIX.4.1"
	FIX42  = "FIX.4.2"
	FIX43  = "FIX.4.3"
	FIX44  = "FIX.4.4"
	FIXT11 = "FIXT.1.1"
)

// profile is what the session layer of a FIX version allows.
type profile struct {
	beginString string
	fixt        bool
	// infinity is the EndSeqNo meaning "up to the last": 999999 before FIX
	// 4.2, 0 after.
	infinity uint64
	// accept999999 tells whether an EndSeqNo of 999999 means infinity when
	// received: up to FIX 4.2, which some counterparties still send it in.
	accept999999 bool
	resetSeqNum  bool // ResetSeqNumFlag (141), from FIX 4.1
	nextExpected bool // NextExpectedMsgSeqNum (789), from FIX 4.4
	lastProc     bool // LastMsgSeqNumProcessed (369), from FIX 4.2
	rejectReason bool // SessionRejectReason (373) and RefTagID (371), from FIX 4.2
	maxReason    dictionary.RejectReason
	businessRej  bool // BusinessMessageReject (35=j), from FIX 4.2
	// precision is the finest timestamps the session may write, and fraction
	// the most fraction digits a received one may have: whole seconds in FIX
	// 4.0 and 4.1, milliseconds in FIX 4.3; picoseconds in the versions the
	// Time Precision Addendum amends, FIX 4.2, 4.4 and, for FIXT 1.1, 5.0 and
	// later, of which a session writes nanoseconds at most, as time.Time.
	precision fix.TimePrecision
	fraction  int
}

var profiles = map[string]*profile{
	FIX40:  {beginString: FIX40, infinity: 999999, accept999999: true, precision: fix.Seconds, fraction: 0},
	FIX41:  {beginString: FIX41, infinity: 999999, accept999999: true, resetSeqNum: true, precision: fix.Seconds, fraction: 0},
	FIX42:  {beginString: FIX42, accept999999: true, resetSeqNum: true, lastProc: true, rejectReason: true, maxReason: dictionary.ReasonInvalidMsgType, businessRej: true, precision: fix.Nanoseconds, fraction: 12},
	FIX43:  {beginString: FIX43, resetSeqNum: true, lastProc: true, rejectReason: true, maxReason: dictionary.ReasonNonDataValueIncludesSOH, businessRej: true, precision: fix.Milliseconds, fraction: 3},
	FIX44:  {beginString: FIX44, resetSeqNum: true, nextExpected: true, lastProc: true, rejectReason: true, maxReason: dictionary.ReasonOther, businessRej: true, precision: fix.Nanoseconds, fraction: 12},
	FIXT11: {beginString: FIXT11, fixt: true, resetSeqNum: true, nextExpected: true, lastProc: true, rejectReason: true, maxReason: dictionary.ReasonOther, businessRej: true, precision: fix.Nanoseconds, fraction: 12},
}

func profileOf(beginString string) (*profile, error) {
	p, ok := profiles[beginString]
	if !ok {
		return nil, fmt.Errorf("unsupported BeginString %q: want one of FIX.4.0, FIX.4.1, FIX.4.2, FIX.4.3, FIX.4.4, FIXT.1.1", beginString)
	}
	return p, nil
}

// reasonAllowed reports whether the version defines SessionRejectReason r.
func (p *profile) reasonAllowed(r dictionary.RejectReason) bool {
	if !p.rejectReason {
		return false
	}
	if r == dictionary.ReasonInvalidApplVerID {
		return p.fixt
	}
	if r == dictionary.ReasonOther {
		return p.maxReason == dictionary.ReasonOther
	}
	if p.maxReason == dictionary.ReasonOther {
		return r <= dictionary.ReasonNonDataValueIncludesSOH
	}
	return r <= p.maxReason
}

// tooFine reports whether v, a received UTCTimestamp, has more fraction
// digits than the version allows.
func (p *profile) tooFine(v []byte) bool {
	if i := bytes.IndexByte(v, '.'); i >= 0 {
		return len(v)-i-1 > p.fraction
	}
	return false
}

// isInfinity reports whether EndSeqNo end means "up to the last".
func (p *profile) isInfinity(end uint64) bool {
	return end == 0 || end == 999999 && p.accept999999
}
