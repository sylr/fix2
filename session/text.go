package session

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// maxText bounds the Text (58) of messages the session sends, in bytes.
const maxText = 512

// cleanText makes s safe for a Text field: control characters, SOH
// included, are escaped as \xNN, and the result is cut, on a rune boundary,
// to maxText bytes.
func cleanText(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if !clean {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if c := s[i]; c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
		}
		s = b.String()
	}
	if len(s) <= maxText {
		return s
	}
	cut := maxText - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

// withhold returns text, a Text or log about m's field tag (noTag for
// none), or fallback if text may show the value of one of m's SensitiveTags
// fields: the session never repeats a credential. It does if tag is one, and
// if text holds the beginning of one's value, as is or quoted (quoteValue,
// %q), however it was cut: its first 4 bytes, to the end of their last rune.
// A value shorter than 4 bytes is not looked for: it would match texts about
// any other field. The texts the session writes about a field quote no
// value of a sensitive one, nor do parse errors quote any: this is a
// backstop, for a credential sent in another field by mistake.
func withhold(m *fix2.Message, tag int, text, fallback string) string {
	if slices.Contains(SensitiveTags, tag) {
		return fallback
	}
	for _, f := range m.Fields() {
		if len(f.Value) < 4 || !slices.Contains(SensitiveTags, f.Tag) {
			continue
		}
		n := 4
		for n < len(f.Value) && !utf8.RuneStart(f.Value[n]) {
			n++
		}
		v := string(f.Value[:n])
		q := strconv.Quote(v)
		if strings.Contains(text, v) || strings.Contains(text, q[1:len(q)-1]) {
			return fallback
		}
	}
	return text
}

// withholdAttrs withholds, in attrs, the values that may show the value of
// one of m's SensitiveTags fields, as withhold does: as a handler prints
// them, a LogValuer's resolved and a group's attributes included. A value of
// kind Any is replaced by the string checked, what is logged being what was
// checked whatever its type prints elsewhere.
func withholdAttrs(m *fix2.Message, attrs []slog.Attr) {
	for i := range attrs {
		v := attrs[i].Value.Resolve()
		switch v.Kind() {
		case slog.KindGroup:
			g := slices.Clone(v.Group())
			withholdAttrs(m, g)
			v = slog.GroupValue(g...)
		default:
			s := v.String()
			switch {
			case withhold(m, noTag, s, "") == "":
				v = slog.StringValue("withheld")
			case v.Kind() == slog.KindAny:
				v = slog.StringValue(s)
			}
		}
		attrs[i].Value = v
	}
}

// withholdLocked is withhold for a text the session logs, or keeps as why it
// logged out: for m parsed only up to a malformed tag (see partial), it
// returns fallback whatever text is, withhold not seeing what follows the
// tag.
func (s *Session) withholdLocked(m *fix2.Message, tag int, text, fallback string) string {
	if m == s.partial {
		return fallback
	}
	return withhold(m, tag, text, fallback)
}

// numAttr logs v, a number of m's field tag, or worked out from one, as key:
// withheld if it may show a credential, as a misplaced one would.
func (s *Session) numAttr(m *fix2.Message, key string, tag int, v int64) slog.Attr {
	return slog.String(key, s.withholdLocked(m, tag, strconv.FormatInt(v, 10), "withheld"))
}

// quoteValue quotes a field value for a Text, escaping control characters
// and cutting long values.
func quoteValue(v []byte) string {
	const maxValue = 64
	if len(v) > maxValue {
		return strconv.Quote(string(v[:maxValue])) + "..."
	}
	return strconv.Quote(string(v))
}

// rejectText returns the Text of a Reject for reason: the reason's name in
// the FIX specification, which counterparties and test suites match, then
// the detail.
func rejectText(reason dictionary.RejectReason, detail string) string {
	if detail == "" {
		return reason.String()
	}
	return reason.String() + ": " + detail
}

// fieldDesc returns "tag (Name)" for a tag the dictionary defines, "tag"
// otherwise.
func fieldDesc(d *dictionary.Dict, tag int) string {
	if d != nil {
		if fd := d.Fields[tag]; fd != nil {
			return strconv.Itoa(tag) + " (" + fd.Name + ")"
		}
	}
	if name, ok := sessionFieldNames[tag]; ok {
		return strconv.Itoa(tag) + " (" + name + ")"
	}
	return strconv.Itoa(tag)
}

// sessionFieldNames names the fields the session layer itself checks, for
// Texts written without a dictionary.
var sessionFieldNames = map[int]string{
	7: "BeginSeqNo", 8: "BeginString", 9: "BodyLength", 10: "CheckSum",
	16: "EndSeqNo", 34: "MsgSeqNum", 35: "MsgType", 36: "NewSeqNo",
	43: "PossDupFlag", 45: "RefSeqNum", 49: "SenderCompID", 50: "SenderSubID",
	52: "SendingTime", 56: "TargetCompID", 57: "TargetSubID", 58: "Text",
	97: "PossResend", 98: "EncryptMethod", 108: "HeartBtInt", 112: "TestReqID",
	122: "OrigSendingTime", 123: "GapFillFlag", 141: "ResetSeqNumFlag",
	142: "SenderLocationID", 143: "TargetLocationID", 369: "LastMsgSeqNumProcessed",
	371: "RefTagID", 372: "RefMsgType", 373: "SessionRejectReason",
	789: "NextExpectedMsgSeqNum", 1128: "ApplVerID", 1137: "DefaultApplVerID",
}
