package session

import (
	"time"

	"github.com/sylr/fix2/fix"
)

// Tags the session layer reads or writes.
const (
	tagBeginSeqNo             = 7
	tagEndSeqNo               = 16
	tagMsgSeqNum              = 34
	tagMsgType                = 35
	tagNewSeqNo               = 36
	tagPossDupFlag            = 43
	tagRefSeqNum              = 45
	tagSenderCompID           = 49
	tagSenderSubID            = 50
	tagSendingTime            = 52
	tagTargetCompID           = 56
	tagTargetSubID            = 57
	tagText                   = 58
	tagSecureDataLen          = 90
	tagSecureData             = 91
	tagSignature              = 89
	tagSignatureLength        = 93
	tagPossResend             = 97
	tagEncryptMethod          = 98
	tagHeartBtInt             = 108
	tagTestReqID              = 112
	tagOrigSendingTime        = 122
	tagGapFillFlag            = 123
	tagResetSeqNumFlag        = 141
	tagSenderLocationID       = 142
	tagTargetLocationID       = 143
	tagLastMsgSeqNumProcessed = 369
	tagRefTagID               = 371
	tagRefMsgType             = 372
	tagSessionRejectReason    = 373
	tagBusinessRejectRefID    = 379
	tagBusinessRejectReason   = 380
	tagNextExpectedMsgSeqNum  = 789
	tagApplVerID              = 1128
	tagDefaultApplVerID       = 1137
	tagSessionStatus          = 1409
)

// SessionStatus (1409) values of the Logouts the session sends for a
// sequence number it cannot accept.
const (
	statusSeqTooLow   = "9"  // Received MsgSeqNum too low
	statusNextTooHigh = "10" // Received NextExpectedMsgSeqNum too high
)

// reservedTags are the tags the session writes on every message it sends:
// an application message setting one is refused.
var reservedTags = [...]int{8, 9, 10, 34, 35, 43, 49, 50, 52, 56, 57, 122, 142, 143, 369, 1128}

// sealReserved reports whether a session with an Encrypter writes tag
// itself: a reserved one, or SecureDataLen (90) and SecureData (91), which it
// writes when it seals a message.
func sealReserved(tag int) bool {
	return tag == tagSecureDataLen || tag == tagSecureData || reserved(tag)
}

// reserved reports whether the session writes tag itself.
func reserved(tag int) bool {
	for _, t := range reservedTags {
		if t == tag {
			return true
		}
	}
	return false
}

// header holds the header fields the session writes on a message.
type header struct {
	msgType       string
	seq           uint64
	possDup       bool
	orig          []byte // OrigSendingTime, with possDup
	sendingTime   time.Time
	lastProcessed uint64 // LastMsgSeqNumProcessed, if > 0
	applVerID     string // FIXT ApplVerID, if not the default
	probe         string // TestReqID of an acknowledgement probe
	route         *route // third-party routing of a reply, if any
}

// route holds the third-party routing fields of a reply: the reverse of
// those of the message it answers.
type route struct {
	onBehalfOf, onBehalfOfSub, onBehalfOfLoc []byte // 115, 116, 144
	deliverTo, deliverToSub, deliverToLoc    []byte // 128, 129, 145
}

// reverseRoute returns the routing of a reply to m, nil if m has none:
// OnBehalfOf fields become DeliverTo ones and the reverse. Empty fields are
// left out; the location IDs exist from FIX 4.1 on.
func reverseRoute(m *fix.Message, beginString string) *route {
	r := route{
		deliverTo:     nonEmpty(m.Get(115)),
		deliverToSub:  nonEmpty(m.Get(116)),
		onBehalfOf:    nonEmpty(m.Get(128)),
		onBehalfOfSub: nonEmpty(m.Get(129)),
	}
	if beginString != FIX40 {
		r.deliverToLoc = nonEmpty(m.Get(144))
		r.onBehalfOfLoc = nonEmpty(m.Get(145))
	}
	if r.deliverTo == nil && r.deliverToSub == nil && r.deliverToLoc == nil &&
		r.onBehalfOf == nil && r.onBehalfOfSub == nil && r.onBehalfOfLoc == nil {
		return nil
	}
	return &r
}

func nonEmpty(v []byte) []byte {
	if len(v) == 0 {
		return nil
	}
	return v
}

// appendFrame appends a complete message to dst: header h, then body, the
// fields after the header, then the CheckSum. Header fields are in ascending
// tag order after MsgType.
func (s *Session) appendFrame(dst []byte, h *header, body []byte) []byte {
	start := len(dst)
	dst = append(dst, "8="...)
	dst = append(dst, s.id.BeginString...)
	dst = append(dst, 1, '9', '=')
	// BodyLength comes first: reserve its widest form, shrink it after.
	lenPos := len(dst)
	dst = append(dst, "0000000000\x01"...)
	bodyStart := len(dst)
	dst = fix.AppendTagValue(dst, tagMsgType, h.msgType)
	dst = fix.AppendTagInt(dst, tagMsgSeqNum, int64(h.seq))
	if h.possDup {
		dst = fix.AppendTagValue(dst, tagPossDupFlag, "Y")
	}
	dst = fix.AppendTagValue(dst, tagSenderCompID, s.id.SenderCompID)
	if s.id.SenderSubID != "" {
		dst = fix.AppendTagValue(dst, tagSenderSubID, s.id.SenderSubID)
	}
	dst = append(dst, "52="...)
	dst = fix.AppendUTCTimestamp(dst, h.sendingTime, s.prec)
	dst = append(dst, 1)
	dst = fix.AppendTagValue(dst, tagTargetCompID, s.id.TargetCompID)
	if s.id.TargetSubID != "" {
		dst = fix.AppendTagValue(dst, tagTargetSubID, s.id.TargetSubID)
	}
	r := h.route
	if r == nil {
		r = &route{}
	}
	dst = appendIf(dst, 115, r.onBehalfOf)
	dst = appendIf(dst, 116, r.onBehalfOfSub)
	if h.possDup && h.orig != nil {
		dst = fix.AppendTagValue(dst, tagOrigSendingTime, h.orig)
	}
	dst = appendIf(dst, 128, r.deliverTo)
	dst = appendIf(dst, 129, r.deliverToSub)
	if s.id.SenderLocationID != "" {
		dst = fix.AppendTagValue(dst, tagSenderLocationID, s.id.SenderLocationID)
	}
	if s.id.TargetLocationID != "" {
		dst = fix.AppendTagValue(dst, tagTargetLocationID, s.id.TargetLocationID)
	}
	dst = appendIf(dst, 144, r.onBehalfOfLoc)
	dst = appendIf(dst, 145, r.deliverToLoc)
	if h.lastProcessed > 0 {
		dst = fix.AppendTagInt(dst, tagLastMsgSeqNumProcessed, int64(h.lastProcessed))
	}
	if h.applVerID != "" {
		dst = fix.AppendTagValue(dst, tagApplVerID, h.applVerID)
	}
	dst = append(dst, body...)

	n := len(dst) - bodyStart
	var digits [20]byte
	d := appendDecimal(digits[:0], uint64(n))
	copy(dst[lenPos:], d)
	dst[lenPos+len(d)] = 1
	copy(dst[lenPos+len(d)+1:], dst[bodyStart:])
	dst = dst[:len(dst)-(10-len(d))]
	return fix.AppendTrailer(dst, start)
}

// appendIf appends tag=v if v is not nil.
func appendIf(dst []byte, tag int, v []byte) []byte {
	if v == nil {
		return dst
	}
	return fix.AppendTagValue(dst, tag, v)
}

func appendDecimal(dst []byte, v uint64) []byte {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(dst, b[i:]...)
}

// msgTypeOf returns the MsgType of raw, a whole message, without parsing it:
// 35 is the third field.
func msgTypeOf(raw []byte) []byte {
	soh := 0
	for i, c := range raw {
		if c != 1 {
			continue
		}
		soh++
		if soh == 2 {
			rest := raw[i+1:]
			if len(rest) < 4 || rest[0] != '3' || rest[1] != '5' || rest[2] != '=' {
				return nil
			}
			for j := 3; j < len(rest); j++ {
				if rest[j] == 1 {
					return rest[3:j]
				}
			}
			return nil
		}
	}
	return nil
}

// isAdmin reports whether msgType is a session-level message.
func isAdmin(msgType []byte) bool {
	if len(msgType) != 1 {
		return false
	}
	switch msgType[0] {
	case '0', '1', '2', '3', '4', '5', 'A':
		return true
	}
	return false
}

// isReject reports whether msgType is a Reject (3) or a BusinessMessageReject
// (j).
func isReject(msgType []byte) bool {
	return len(msgType) == 1 && (msgType[0] == '3' || msgType[0] == 'j')
}
