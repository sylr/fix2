package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
)

// Tags the checkers read.
const (
	tagCheckSum        = 10
	tagClOrdID         = 11
	tagGapFillFlag     = 123
	tagNewSeqNo        = 36
	tagPossDupFlag     = 43
	tagRefSeqNum       = 45
	tagResetSeqNumFlag = 141
	tagSendingTime     = 52
	tagTestReqID       = 112
	tagRejectReason    = 373

	tagBusinessRejectReason = 380
)

// envelope holds the tags of the standard header and trailer: a resend
// changes some of them, and is the same message.
var envelope = map[int]bool{
	8: true, 9: true, 35: true, 49: true, 56: true, 115: true, 128: true, 90: true, 91: true,
	34: true, 50: true, 142: true, 57: true, 143: true, 116: true, 144: true, 129: true, 145: true,
	43: true, 97: true, 52: true, 122: true, 212: true, 213: true, 347: true, 369: true, 370: true,
	627: true, 628: true, 629: true, 630: true, 1128: true, 1129: true, 1156: true,
	93: true, 89: true, tagCheckSum: true,
}

// digest identifies a message by its application fields, in order.
func digest(m *fix.Message) string {
	h := sha256.New()
	for _, f := range m.Fields() {
		if envelope[f.Tag] {
			continue
		}
		h.Write(strconv.AppendInt(nil, int64(f.Tag), 10))
		h.Write([]byte{'='})
		h.Write(f.Value)
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// digestRaw is digest of a raw message, "" if it does not parse.
func digestRaw(raw []byte) string {
	m, err := fix.Parse(raw)
	if err != nil {
		return ""
	}
	return digest(&m)
}

// clOrdID returns the ClOrdID of a raw message, "" if none.
func clOrdID(raw []byte) string {
	m, err := fix.Parse(raw)
	if err != nil {
		return ""
	}
	return string(m.Get(tagClOrdID))
}

// order is a NewOrderSingle with ClOrdID id, made at now.
func order(id string, now time.Time) *fix.Builder {
	return fix.NewBuilder(session.FIX44, "D").
		SetString(tagClOrdID, id).
		SetString(55, "FIX2").
		SetString(54, "1").
		SetString(60, string(fix.AppendUTCTimestamp(nil, now, fix.Milliseconds))).
		SetString(38, "100").
		SetString(40, "1")
}

// hash identifies raw bytes, any message's.
func hash(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:12])
}

// frameEvent describes a frame the session read or wrote.
func frameEvent(kind string, f session.Frame, now time.Time) Event {
	ev := Event{Kind: kind, Gen: f.Generation, Seq: f.Seq, Epoch: f.Epoch, Resend: f.Resend, Now: &now, Hash: hash(f.Raw)}
	m, err := fix.Parse(f.Raw)
	if err != nil {
		ev.Err = err.Error()
		return ev
	}
	ev.MsgType = string(m.MsgType())
	ev.PossDup = string(m.Get(tagPossDupFlag)) == "Y"
	if v := m.Get(tagSendingTime); v != nil {
		if t, err := fix.ParseUTCTimestamp(v); err == nil {
			ev.SendingTime = &t
		}
	}
	switch ev.MsgType {
	case "4":
		ev.GapFill = string(m.Get(tagGapFillFlag)) == "Y"
		ev.NewSeqNo = uintField(&m, tagNewSeqNo)
	case "A":
		ev.Reset = string(m.Get(tagResetSeqNumFlag)) == "Y"
	case "3":
		ev.RefSeqNum = uintField(&m, tagRefSeqNum)
		ev.RejectReason = string(m.Get(tagRejectReason))
	case "j":
		ev.RefSeqNum = uintField(&m, tagRefSeqNum)
		ev.RejectReason = string(m.Get(tagBusinessRejectReason))
	case "0", "1":
		ev.TestReqID = string(m.Get(tagTestReqID))
	case "2", "5":
	default:
		ev.ClOrdID = string(m.Get(tagClOrdID))
		ev.Digest = digest(&m)
	}
	return ev
}

func uintField(m *fix.Message, tag int) uint64 {
	v, _ := strconv.ParseUint(string(m.Get(tag)), 10, 64)
	return v
}
