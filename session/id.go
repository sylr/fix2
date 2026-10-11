package session

import "strings"

// SessionID identifies a session: its BeginString and the identities of both
// sides as they appear on the wire, from our point of view. Qualifier tells
// apart sessions whose wire identities are the same; it is never sent.
type SessionID struct {
	BeginString                                 string
	SenderCompID, SenderSubID, SenderLocationID string
	TargetCompID, TargetSubID, TargetLocationID string
	Qualifier                                   string
}

// String returns the session ID as "FIX.4.4:SENDER/SUB/LOC->TARGET/SUB/LOC:QUALIFIER",
// leaving out empty parts.
func (id SessionID) String() string {
	var b strings.Builder
	b.WriteString(id.BeginString)
	b.WriteByte(':')
	writeParty(&b, id.SenderCompID, id.SenderSubID, id.SenderLocationID)
	b.WriteString("->")
	writeParty(&b, id.TargetCompID, id.TargetSubID, id.TargetLocationID)
	if id.Qualifier != "" {
		b.WriteByte(':')
		b.WriteString(id.Qualifier)
	}
	return b.String()
}

func writeParty(b *strings.Builder, comp, sub, loc string) {
	b.WriteString(comp)
	if sub != "" || loc != "" {
		b.WriteByte('/')
		b.WriteString(sub)
	}
	if loc != "" {
		b.WriteByte('/')
		b.WriteString(loc)
	}
}

// wire returns id without its Qualifier: what identifies it on the wire.
func (id SessionID) wire() SessionID {
	id.Qualifier = ""
	return id
}

// reversed returns the ID the counterparty knows the session by.
func (id SessionID) reversed() SessionID {
	return SessionID{
		BeginString:      id.BeginString,
		SenderCompID:     id.TargetCompID,
		SenderSubID:      id.TargetSubID,
		SenderLocationID: id.TargetLocationID,
		TargetCompID:     id.SenderCompID,
		TargetSubID:      id.SenderSubID,
		TargetLocationID: id.SenderLocationID,
	}
}
