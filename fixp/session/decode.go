package session

import (
	"fmt"

	"github.com/sylr/fix2/fixp/internal/codec"
)

// sessionMessage is a message of the FIXP schema the engine decodes: a
// session message, Applied or NotApplied.
type sessionMessage interface {
	DecodeBody(h codec.MessageHeader, body []byte) (int, error)
}

// decodeBody decodes m from body, the message after header h, and checks
// it well-formed; an error makes it malformed, which terminates the
// connection (§4.4: "a transport should be terminated if an unrecoverable
// error occurs in message parsing or framing"):
//
//   - a null in the message header, before anything reads it;
//   - what the decoder refuses, codec.ErrNewer among it: a newer version's
//     groups or data it cannot bound, whose meaning it cannot know either;
//   - bytes after what the decoder consumed, its block padding counted,
//     unless TolerateTrailingBytes and the header's version is not above
//     the schema's: after a newer version's message, they may be its
//     groups or data (docs/sbe.md, Decoding);
//   - a null in a required field, or an enumeration value the schema does
//     not list (SBE §4.15), but the flows', which the negotiation answers.
//
// On error, m is not to be used.
func (cfg *Config) decodeBody(m sessionMessage, h codec.MessageHeader, body []byte) error {
	// The header's fields are required uint16s too: a null Version would
	// read as a newer version, a null BlockLength as a block padded to it.
	if err := headerWellFormed(h); err != nil {
		return err
	}
	n, err := m.DecodeBody(h, body)
	if err != nil {
		return err
	}
	if n < len(body) && (!cfg.TolerateTrailingBytes || h.Version > codec.SchemaVersion) {
		return fmt.Errorf("%d bytes after the message", len(body)-n)
	}
	return wellFormed(m)
}

// headerWellFormed reports a null in h, a FIXP message's header, whose
// fields are all required (SBE §4.15).
func headerWellFormed(h codec.MessageHeader) error {
	return first(
		required("header's BlockLength", h.BlockLength), required("header's TemplateId", h.TemplateId),
		required("header's SchemaId", h.SchemaId), required("header's Version", h.Version),
	)
}

// decode is decodeBody of msg, a message with its header.
func (cfg *Config) decode(m sessionMessage, msg []byte) error {
	h, body, err := codec.ReadHeader(msg)
	if err != nil {
		return err
	}
	return cfg.decodeBody(m, h, body)
}

// wellFormed reports a null in a required field of m, or an enumeration
// value its schema does not list (SBE §4.15: "The null value of a datatype
// is invalid for a required field", "A value is invalid if it does not
// match one of the explicitly listed valid values"). Optional fields'
// nulls are their absence, decoded as such. FlowType is left to the
// negotiation, which answers an unknown one as a flow not supported; a
// UUID's validity to the session ID checks.
func wellFormed(m sessionMessage) error {
	switch m := m.(type) {
	case *codec.Negotiate:
		return required("Timestamp", m.Timestamp)
	case *codec.NegotiationResponse:
		return required("RequestTimestamp", m.RequestTimestamp)
	case *codec.NegotiationReject:
		return first(required("RequestTimestamp", m.RequestTimestamp), known("Code", m.Code))
	case *codec.Establish:
		return first(required("Timestamp", m.Timestamp), required("KeepaliveInterval", m.KeepaliveInterval))
	case *codec.EstablishmentAck:
		return first(required("RequestTimestamp", m.RequestTimestamp), required("KeepaliveInterval", m.KeepaliveInterval))
	case *codec.EstablishmentReject:
		return first(required("RequestTimestamp", m.RequestTimestamp), known("Code", m.Code))
	case *codec.Sequence:
		return required("NextSeqNo", m.NextSeqNo)
	case *codec.RetransmitRequest:
		return first(required("Timestamp", m.Timestamp), required("FromSeqNo", m.FromSeqNo), required("Count", m.Count))
	case *codec.Retransmission:
		return first(required("RequestTimestamp", m.RequestTimestamp), required("NextSeqNo", m.NextSeqNo), required("Count", m.Count))
	case *codec.RestransmitReject:
		return first(required("RequestTimestamp", m.RequestTimestamp), known("Code", m.Code))
	case *codec.Terminate:
		return known("Code", m.Code)
	case *codec.Applied:
		return first(required("FromSeqNo", m.FromSeqNo), required("Count", m.Count))
	case *codec.NotApplied:
		return first(required("FromSeqNo", m.FromSeqNo), required("Count", m.Count))
	case *codec.MessageTemplate:
		return required("EncodingType", m.EncodingType)
	}
	// UnsequencedHeartbeat, FinishedSending (LastSeqNo optional),
	// FinishedReceiving: nothing that may be null.
	return nil
}

// required reports v null: all ones, an unsigned type's null (SBE §4.4.2;
// the FIXP schema sets no nullValue).
func required[T uint16 | uint32 | uint64](name string, v T) error {
	if v == ^T(0) {
		return fmt.Errorf("%s null", name)
	}
	return nil
}

// known reports v not a value its enumeration lists.
func known[E interface {
	Known() bool
	String() string
}](name string, v E) error {
	if !v.Known() {
		return fmt.Errorf("%s %s not a valid value", name, v)
	}
	return nil
}

func first(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
