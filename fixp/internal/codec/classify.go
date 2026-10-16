package codec

import (
	"errors"

	"github.com/sylr/fix2/sofh"
)

// Kind is what a frame holds, for a session.
type Kind uint8

const (
	// Session is a session message, to decode by its template id.
	Session Kind = iota + 1
	// Application is an application message, which a flow sequences: a
	// message of another schema, or Applied or NotApplied (§4.8).
	Application
	// Ignored is a MessageTemplate, which "may be sent at any time" (§3.6)
	// and fix2 does not use.
	Ignored
)

func (k Kind) String() string {
	switch k {
	case Session:
		return "session"
	case Application:
		return "application"
	case Ignored:
		return "ignored"
	}
	return "unknown"
}

var (
	// ErrEncoding reports a frame of an encoding other than SBE 1.0
	// little-endian.
	ErrEncoding = errors.New("codec: frame not of SBE 1.0 little-endian")
	// ErrPointToPoint reports a Topic or a Context, which have no place on
	// a point-to-point session without multiplexing.
	ErrPointToPoint = errors.New("codec: Topic or Context on a point-to-point session")
)

// Classify tells what a frame carries, by its SOFH encoding type, then its
// schema id, then its template id, and returns its message header and the
// body after it. An application message's schema must start its header as
// SBE's standard one does, with four uint16: blockLength, templateId,
// schemaId and version. The body follows those 8 bytes: for an application
// schema whose header is longer, it starts with the rest of the header, and
// the application's codec is to read the whole message, msg. Both alias
// msg, as decoded []byte fields do: what outlives the frame is copied.
//
// A frame of another encoding is ErrEncoding, one too short for a header
// ErrShort, a Topic or a Context ErrPointToPoint, a template the FIXP
// schema does not define ErrTemplate.
func Classify(enc sofh.Encoding, msg []byte) (Kind, MessageHeader, []byte, error) {
	if enc != sofh.SBE10LittleEndian {
		return 0, MessageHeader{}, nil, ErrEncoding
	}
	h, body, err := ReadHeader(msg)
	if err != nil {
		return 0, h, nil, err
	}
	if h.SchemaId != SchemaID {
		return Application, h, body, nil
	}
	switch h.TemplateId {
	case NegotiateTemplateID, NegotiationResponseTemplateID, NegotiationRejectTemplateID,
		EstablishTemplateID, EstablishmentAckTemplateID, EstablishmentRejectTemplateID,
		SequenceTemplateID, UnsequencedHeartbeatTemplateID,
		RetransmitRequestTemplateID, RetransmissionTemplateID, RestransmitRejectTemplateID,
		TerminateTemplateID, FinishedSendingTemplateID, FinishedReceivingTemplateID:
		return Session, h, body, nil
	case AppliedTemplateID, NotAppliedTemplateID:
		return Application, h, body, nil
	case MessageTemplateTemplateID:
		return Ignored, h, body, nil
	case TopicTemplateID, ContextTemplateID:
		return 0, h, body, ErrPointToPoint
	}
	return 0, h, body, ErrTemplate
}
