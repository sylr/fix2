package codec

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sylr/fix2/sofh"
)

type message interface {
	AppendTo([]byte) ([]byte, error)
	Decode([]byte) error
}

// newMessage returns a new message of a template of the FIXP schema, nil
// for an unknown one.
func newMessage(template uint16) message {
	switch template {
	case NegotiateTemplateID:
		return &Negotiate{}
	case NegotiationResponseTemplateID:
		return &NegotiationResponse{}
	case NegotiationRejectTemplateID:
		return &NegotiationReject{}
	case TopicTemplateID:
		return &Topic{}
	case EstablishTemplateID:
		return &Establish{}
	case EstablishmentAckTemplateID:
		return &EstablishmentAck{}
	case EstablishmentRejectTemplateID:
		return &EstablishmentReject{}
	case SequenceTemplateID:
		return &Sequence{}
	case ContextTemplateID:
		return &Context{}
	case UnsequencedHeartbeatTemplateID:
		return &UnsequencedHeartbeat{}
	case RetransmitRequestTemplateID:
		return &RetransmitRequest{}
	case RetransmissionTemplateID:
		return &Retransmission{}
	case RestransmitRejectTemplateID:
		return &RestransmitReject{}
	case TerminateTemplateID:
		return &Terminate{}
	case FinishedSendingTemplateID:
		return &FinishedSending{}
	case FinishedReceivingTemplateID:
		return &FinishedReceiving{}
	case AppliedTemplateID:
		return &Applied{}
	case NotAppliedTemplateID:
		return &NotApplied{}
	case MessageTemplateTemplateID:
		return &MessageTemplate{}
	}
	return nil
}

// fresh returns a new message of m's type.
func fresh(m message) message {
	return reflect.New(reflect.TypeOf(m).Elem()).Interface().(message)
}

// same reports whether two messages hold the same values, an empty []byte
// being a nil one.
func same(a, b message) bool { return fmt.Sprintf("%+v", a) == fmt.Sprintf("%+v", b) }

// The values FixpCheck.java sets (internal/gen/sbe/testdata/reallogic).
var (
	session [16]byte
	ts      = uint64(1_700_000_000_123_456_789)
)

func init() {
	for i := range session {
		session[i] = byte(0x10 + i)
	}
}

func fixtures() map[string]message {
	return map[string]message{
		"Negotiate":                   &Negotiate{SessionId: session, Timestamp: ts, ClientFlow: FlowTypeIdempotent, Credentials: []byte("user\x00secret")},
		"NegotiationResponse":         &NegotiationResponse{SessionId: session, RequestTimestamp: ts, ServerFlow: FlowTypeRecoverable},
		"NegotiationReject":           &NegotiationReject{SessionId: session, RequestTimestamp: ts, Code: NegotiationRejectCodeCredentials, Reason: []byte("bad credentials")},
		"Topic":                       &Topic{SessionId: session, Flow: FlowTypeIdempotent, KeepaliveInterval: 500, Classification: []byte("MD")},
		"Establish":                   &Establish{SessionId: session, Timestamp: ts, KeepaliveInterval: 1000, NextSeqNo: Some(uint64(1)), Credentials: []byte("user")},
		"EstablishNoNextSeqNo":        &Establish{SessionId: session, Timestamp: ts, KeepaliveInterval: 1000},
		"EstablishmentAck":            &EstablishmentAck{SessionId: session, RequestTimestamp: ts, KeepaliveInterval: 2000, NextSeqNo: Some(uint64(100))},
		"EstablishmentAckNoNextSeqNo": &EstablishmentAck{SessionId: session, RequestTimestamp: ts, KeepaliveInterval: 2000},
		"EstablishmentReject":         &EstablishmentReject{SessionId: session, RequestTimestamp: ts, Code: EstablishmentRejectCodeKeepaliveInterval, Reason: []byte("too short")},
		"Sequence":                    &Sequence{NextSeqNo: 42},
		"Context":                     &Context{SessionId: session, NextSeqNo: 7},
		"UnsequencedHeartbeat":        &UnsequencedHeartbeat{},
		"RetransmitRequest":           &RetransmitRequest{SessionId: session, Timestamp: ts, FromSeqNo: 10, Count: 5},
		"Retransmission":              &Retransmission{SessionId: session, RequestTimestamp: ts, NextSeqNo: 10, Count: 5},
		"RestransmitReject":           &RestransmitReject{SessionId: session, RequestTimestamp: ts, Code: RetransmitRejectCodeOutOfRange, Reason: []byte("gone")},
		"Terminate":                   &Terminate{SessionId: session, Code: TerminationCodeFinished},
		"FinishedSending":             &FinishedSending{SessionId: session, LastSeqNo: Some(uint64(99))},
		"FinishedSendingNoLastSeqNo":  &FinishedSending{SessionId: session},
		"FinishedReceiving":           &FinishedReceiving{SessionId: session},
		"Applied":                     &Applied{FromSeqNo: 1, Count: 3},
		"NotApplied":                  &NotApplied{FromSeqNo: 4, Count: 6},
		"MessageTemplate":             &MessageTemplate{EncodingType: 0xEB50, Version: []byte("1.0"), Template: []byte("<xml/>")},
	}
}

// Every message of the schema, as Real Logic's codecs encode it: the same
// values encode to the same bytes, and those bytes decode to them.
func TestRealLogic(t *testing.T) {
	f, err := os.Open("../../../internal/gen/sbe/testdata/reallogic/fixp.hex")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := fixtures()
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, h, _ := strings.Cut(sc.Text(), " ")
		b, err := hex.DecodeString(h)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m, ok := want[name]
		if !ok {
			t.Fatalf("%s: no fixture", name)
		}
		seen[name] = true
		enc, err := m.AppendTo(nil)
		if err != nil || !bytes.Equal(enc, b) {
			t.Errorf("%s encodes as\n%x, %v; Real Logic:\n%x", name, enc, err, b)
		}
		got := fresh(m)
		if err := got.Decode(b); err != nil || !same(got, m) {
			t.Errorf("%s decodes as %+v, %v; want %+v", name, got, err, m)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(want) {
		t.Errorf("%d fixtures of %d", len(seen), len(want))
	}
}

// wireFrames reads the frames of a spike's wire log, and returns them as
// standard SOFH frames, with the label of each. Silverflash's frames count
// their length without the header (docs/fixp.md).
func wireFrames(t *testing.T, path string, silverflash bool) ([]string, []byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var labels []string
	var stream []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		b, err := hex.DecodeString(fields[len(fields)-1])
		if err != nil {
			t.Fatal(err)
		}
		n, enc, err := sofh.ParseHeader(b)
		if silverflash {
			n = int(binary.BigEndian.Uint32(b)) + sofh.HeaderLen
		} else if err != nil {
			t.Fatal(err)
		}
		if n != len(b) {
			t.Fatalf("%s: length %d, %d bytes", sc.Text(), n, len(b))
		}
		if stream, err = sofh.AppendFrame(stream, enc, b[sofh.HeaderLen:]); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, strings.Join(fields[:len(fields)-1], " "))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return labels, stream
}

// The frames exchanged with Silverflash, read back through sofh: each
// classifies, decodes, and encodes back to its bytes, Silverflash's padding
// of zeros aside.
func TestSilverflash(t *testing.T) {
	const dir = "../../../docs/fixp-evidence/silverflash-spike/"
	var decoded []message
	for _, log := range []struct {
		name        string
		silverflash bool
	}{
		{"1-standard-sofh.wire.log", false},
		{"2-recoverable-server.wire.log", true},
		{"3-idempotent-server.wire.log", true},
	} {
		labels, stream := wireFrames(t, dir+log.name, log.silverflash)
		r := sofh.NewReader(bytes.NewReader(stream), sofh.ReaderOptions{})
		for _, label := range labels {
			enc, msg, err := r.Next()
			if err != nil {
				t.Fatalf("%s: %s: %v", log.name, label, err)
			}
			kind, h, _, err := Classify(enc, msg)
			if err != nil {
				t.Fatalf("%s: %s: %v", log.name, label, err)
			}
			if h.SchemaId != SchemaID {
				if kind != Application || !strings.Contains(label, " app") {
					t.Errorf("%s: %s: %s of schema %d", log.name, label, kind, h.SchemaId)
				}
				continue
			}
			m := newMessage(h.TemplateId)
			if err := m.Decode(msg); err != nil {
				t.Fatalf("%s: %s: %v", log.name, label, err)
			}
			enc2, err := m.AppendTo(nil)
			if err != nil || !bytes.HasPrefix(msg, enc2) || bytes.Count(msg[len(enc2):], []byte{0}) != len(msg)-len(enc2) {
				t.Errorf("%s: %s: %x encodes back as %x, %v", log.name, label, msg, enc2, err)
			}
			wantKind := Session
			if _, ok := m.(*NotApplied); ok {
				wantKind = Application
			}
			if kind != wantKind {
				t.Errorf("%s: %s: %s", log.name, label, kind)
			}
			decoded = append(decoded, m)
		}
		if _, _, err := r.Next(); err == nil {
			t.Errorf("%s: frames beyond the log", log.name)
		}
	}
	// What the spike's notes say of the Idempotent server's session.
	var ack *EstablishmentAck
	var notApplied *NotApplied
	var terminates int
	for _, m := range decoded {
		switch m := m.(type) {
		case *EstablishmentAck:
			ack = m
		case *NotApplied:
			notApplied = m
		case *Terminate:
			if m.Code != TerminationCodeFinished {
				t.Errorf("Terminate %v", m.Code)
			}
			terminates++
		}
	}
	if ack == nil || ack.NextSeqNo.Valid || ack.KeepaliveInterval != 1000 {
		t.Errorf("EstablishmentAck %+v", ack)
	}
	if notApplied == nil || *notApplied != (NotApplied{FromSeqNo: 4, Count: 6}) {
		t.Errorf("NotApplied %+v", notApplied)
	}
	if terminates != 2 {
		t.Errorf("%d Terminate", terminates)
	}
}

func TestClassify(t *testing.T) {
	header := func(template, schema uint16) []byte {
		b := make([]byte, HeaderSize)
		binary.LittleEndian.PutUint16(b[2:], template)
		binary.LittleEndian.PutUint16(b[4:], schema)
		return b
	}
	for _, tc := range []struct {
		name string
		enc  sofh.Encoding
		msg  []byte
		kind Kind
		err  error
	}{
		{"Negotiate", sofh.SBE10LittleEndian, header(NegotiateTemplateID, SchemaID), Session, nil},
		{"Sequence", sofh.SBE10LittleEndian, header(SequenceTemplateID, SchemaID), Session, nil},
		{"FinishedReceiving", sofh.SBE10LittleEndian, header(FinishedReceivingTemplateID, SchemaID), Session, nil},
		{"Applied", sofh.SBE10LittleEndian, header(AppliedTemplateID, SchemaID), Application, nil},
		{"NotApplied", sofh.SBE10LittleEndian, header(NotAppliedTemplateID, SchemaID), Application, nil},
		{"another schema", sofh.SBE10LittleEndian, header(NegotiateTemplateID, 33), Application, nil},
		{"MessageTemplate", sofh.SBE10LittleEndian, header(MessageTemplateTemplateID, SchemaID), Ignored, nil},
		{"Topic", sofh.SBE10LittleEndian, header(TopicTemplateID, SchemaID), 0, ErrPointToPoint},
		{"Context", sofh.SBE10LittleEndian, header(ContextTemplateID, SchemaID), 0, ErrPointToPoint},
		{"template 0", sofh.SBE10LittleEndian, header(0, SchemaID), 0, ErrTemplate},
		{"template 20", sofh.SBE10LittleEndian, header(20, SchemaID), 0, ErrTemplate},
		{"big-endian", sofh.SBE10BigEndian, header(NegotiateTemplateID, SchemaID), 0, ErrEncoding},
		{"tag=value", sofh.TagValue, []byte("8=FIX.4.4\x01"), 0, ErrEncoding},
		{"short", sofh.SBE10LittleEndian, header(NegotiateTemplateID, SchemaID)[:7], 0, ErrShort},
	} {
		kind, h, body, err := Classify(tc.enc, tc.msg)
		if kind != tc.kind || !errors.Is(err, tc.err) {
			t.Errorf("%s: %s, %v; want %s, %v", tc.name, kind, err, tc.kind, tc.err)
		}
		if err == nil && (len(body) != len(tc.msg)-HeaderSize || h.SchemaId != binary.LittleEndian.Uint16(tc.msg[4:])) {
			t.Errorf("%s: header %+v, body %x", tc.name, h, body)
		}
	}
}

// kindOf is the kind of a message of the FIXP schema, as docs/fixp.md lists
// them, and the error of the two it has no place for.
func kindOf(m message) (Kind, error) {
	switch m.(type) {
	case *Applied, *NotApplied:
		return Application, nil
	case *MessageTemplate:
		return Ignored, nil
	case *Topic, *Context:
		return 0, ErrPointToPoint
	}
	return Session, nil
}

// Every message of the schema classifies as its template says, and
// decodes through DecodeBody as through Decode.
func TestClassifyFixtures(t *testing.T) {
	templates := map[uint16]bool{}
	for name, m := range fixtures() {
		b, err := m.AppendTo(nil)
		if err != nil {
			t.Fatal(err)
		}
		kind, h, body, err := Classify(sofh.SBE10LittleEndian, b)
		templates[h.TemplateId] = true
		wantKind, wantErr := kindOf(m)
		if kind != wantKind || !errors.Is(err, wantErr) {
			t.Errorf("%s: %s, %v; want %s, %v", name, kind, err, wantKind, wantErr)
		}
		if err != nil {
			continue
		}
		got := fresh(m)
		if err := got.(interface {
			DecodeBody(MessageHeader, []byte) error
		}).DecodeBody(h, body); err != nil || !same(got, m) {
			t.Errorf("%s: DecodeBody: %+v, %v", name, got, err)
		}
	}
	if len(templates) != 19 {
		t.Errorf("%d templates of 19", len(templates))
	}
}

// A message of a later version of the FIXP schema, its block longer: a
// session message still, decoded without what it appends (§7.3.1).
func TestClassifyNewerVersion(t *testing.T) {
	b, _ := (&Sequence{NextSeqNo: 42}).AppendTo(nil)
	binary.LittleEndian.PutUint16(b[0:], SequenceBlockLength+4) // blockLength
	binary.LittleEndian.PutUint16(b[6:], SchemaVersion+1)       // version
	b = append(b, 1, 2, 3, 4)
	kind, h, body, err := Classify(sofh.SBE10LittleEndian, b)
	if kind != Session || err != nil {
		t.Fatalf("%s, %v", kind, err)
	}
	var m Sequence
	if err := m.DecodeBody(h, body); err != nil || m.NextSeqNo != 42 {
		t.Fatalf("%+v, %v", m, err)
	}
}

// An application schema whose header has fields after the standard four:
// an application message, its body starting with them.
func TestClassifyLongerHeader(t *testing.T) {
	b := []byte{4, 0, 1, 0, 33, 0, 0, 0, 0xaa, 0xbb, 'b', 'o', 'd', 'y'}
	kind, h, body, err := Classify(sofh.SBE10LittleEndian, b)
	if kind != Application || err != nil || h.SchemaId != 33 || h.TemplateId != 1 || !bytes.Equal(body, b[HeaderSize:]) {
		t.Fatalf("%s, %+v, %x, %v", kind, h, body, err)
	}
}

func FuzzClassify(f *testing.F) {
	for _, m := range fixtures() {
		b, _ := m.AppendTo(nil)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		kind, h, _, err := Classify(sofh.SBE10LittleEndian, b)
		if err != nil || h.SchemaId != SchemaID {
			return
		}
		m := newMessage(h.TemplateId)
		if m == nil {
			t.Fatalf("%s of template %d", kind, h.TemplateId)
		}
		if want, _ := kindOf(m); kind != want {
			t.Fatalf("%T classified %s", m, kind)
		}
		if m.Decode(b) != nil {
			return
		}
		enc, err := m.AppendTo(nil)
		if err != nil {
			t.Fatalf("%T decoded but does not encode: %v", m, err)
		}
		again := fresh(m)
		if err := again.Decode(enc); err != nil || !same(again, m) {
			t.Fatalf("%T: %+v decodes back as %+v, %v", m, m, again, err)
		}
	})
}
