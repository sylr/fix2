package session

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fixp"
	"github.com/sylr/fix2/fixp/internal/codec"
	"github.com/sylr/fix2/fixp/store/memstore"
	"github.com/sylr/fix2/sofh"
	"github.com/sylr/fix2/transport/mem"
)

// spikeFrame is a frame of the Silverflash spike's wire log: who sent it,
// and the SBE message.
type spikeFrame struct {
	client bool
	label  string
	msg    []byte
}

// readSpike reads a wire log of the spike, Silverflash's framing: its SOFH
// length leaves the header out.
func readSpike(t *testing.T, path string) []spikeFrame {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var frames []spikeFrame
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		b, err := hex.DecodeString(fields[len(fields)-1])
		if err != nil {
			t.Fatal(err)
		}
		if int(binary.BigEndian.Uint32(b))+sofh.HeaderLen != len(b) {
			t.Fatalf("%s: not Silverflash's framing", sc.Text())
		}
		frames = append(frames, spikeFrame{client: fields[0] == ">", label: strings.Join(fields[1:len(fields)-1], " "), msg: b[sofh.HeaderLen:]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return frames
}

// echoer answers each order "order N" with "ack N", as the spike's server.
type echoer struct {
	BaseHandler
	gaps chan [2]uint64
}

func (e *echoer) OnMessage(ctx context.Context, s *Session, msg []byte, in Inbound) error {
	reply := appMsg{22, "ack" + strings.TrimPrefix(string(msg[8:]), "order")}
	_, err := s.Send(ctx, reply)
	return err
}

func (e *echoer) OnGap(_ context.Context, _ *Session, from, count uint64) {
	e.gaps <- [2]uint64{from, count}
}

// meaningful reads what the server writes, heartbeats aside, until n
// frames, and returns them with the sequence numbers of the application
// messages among them.
func (r *raw) meaningful(t *testing.T, n int) ([]codec.MessageHeader, [][]byte, []uint64) {
	t.Helper()
	var hs []codec.MessageHeader
	var msgs [][]byte
	var seqs []uint64
	var next uint64
	for len(msgs) < n {
		h, msg := r.read()
		if h.SchemaId == codec.SchemaID && h.TemplateId == codec.SequenceTemplateID {
			var s codec.Sequence
			s.Decode(msg)
			next = s.NextSeqNo
			continue
		}
		app := h.SchemaId != codec.SchemaID || h.TemplateId == codec.NotAppliedTemplateID || h.TemplateId == codec.AppliedTemplateID
		var seq uint64
		if app && next != 0 {
			seq = next
			next++
		}
		hs, msgs, seqs = append(hs, h), append(msgs, msg), append(seqs, seq)
	}
	return hs, msgs, seqs
}

// The spike's client, replayed against fix2's server: what fix2 answers,
// field by field, is what Silverflash answered, but for the differences
// docs/fixp.md records (padding). Its Establish carries a NextSeqNo for an
// Idempotent flow, which fix2 accepts and ignores, its Sequence numbering
// the flow (§4.8).
func TestSpikeReplay(t *testing.T) {
	frames := readSpike(t, "../../docs/fixp-evidence/silverflash-spike/3-idempotent-server.wire.log")
	gaps := make(chan [2]uint64, 10)
	l := mem.Listen("server")
	_, cancel, done := newServer(t, l, memstore.New(), &echoer{gaps: gaps}, fixp.Idempotent, func(c *ServerConfig) {
		// The spike ran on 2026-10-16: its timestamps are history now.
		c.MaxClockSkew = 100 * 365 * 24 * time.Hour
		c.Authenticate = func(context.Context, *AuthRequest) (string, error) { return "spike", nil }
		c.KeepaliveInterval = time.Second
	})
	defer func() { cancel(); <-done }()
	r := dialRaw(t, l)

	var neg codec.Negotiate
	if _, err := neg.Decode(frames[0].msg); err != nil {
		t.Fatal(err)
	}
	r.id = neg.SessionId
	step := 0
	// send replays the client's frames up to the next answer, or the
	// Terminate, which has a step of its own.
	terminating := false
	send := func() {
		for ; step < len(frames) && frames[step].client; step++ {
			if strings.HasPrefix(frames[step].label, "Establish next=11") {
				step = len(frames) // the harness's re-establishment: excluded
				return
			}
			if strings.HasPrefix(frames[step].label, "Terminate") && !terminating {
				return
			}
			b, _ := sofh.AppendFrame(nil, sofh.SBE10LittleEndian, frames[step].msg)
			r.sendBytes(b)
		}
	}
	// theirs returns Silverflash's answers up to the next client frame.
	theirs := func() []spikeFrame {
		var out []spikeFrame
		for ; step < len(frames) && !frames[step].client; step++ {
			out = append(out, frames[step])
		}
		return out
	}

	// Negotiate.
	send()
	ans := theirs()
	var want, got codec.NegotiationResponse
	want.Decode(ans[0].msg)
	_, msgs, _ := r.meaningful(t, 1)
	if _, err := got.Decode(msgs[0]); err != nil || got.SessionId != want.SessionId || got.RequestTimestamp != want.RequestTimestamp || got.ServerFlow != want.ServerFlow {
		t.Fatalf("NegotiationResponse: fix2 %+v, Silverflash %+v, %v", got, want, err)
	}

	// Establish.
	send()
	ans = theirs()
	var wantAck, gotAck codec.EstablishmentAck
	wantAck.Decode(ans[0].msg)
	_, msgs, _ = r.meaningful(t, 1)
	if _, err := gotAck.Decode(msgs[0]); err != nil || gotAck.SessionId != wantAck.SessionId || gotAck.RequestTimestamp != wantAck.RequestTimestamp || gotAck.KeepaliveInterval != wantAck.KeepaliveInterval {
		t.Fatalf("EstablishmentAck: fix2 %+v, Silverflash %+v, %v", gotAck, wantAck, err)
	}
	// Neither announces the NextSeqNo of an Idempotent server flow, which
	// starts with a Sequence (§4.3.2, §4.8).
	if wantAck.NextSeqNo.Valid || gotAck.NextSeqNo.Valid {
		t.Fatalf("EstablishmentAck NextSeqNo: fix2 %+v, Silverflash %+v", gotAck.NextSeqNo, wantAck.NextSeqNo)
	}

	// Orders 1 to 3, the Sequence 4 heartbeats, Sequence 10 and order 10:
	// Silverflash answered acks 1 to 3, NotApplied 4+6, ack 10.
	var wantApps []string
	for step < len(frames) && !strings.HasPrefix(frames[step].label, "Terminate") {
		send()
		for _, f := range theirs() {
			if h, _, _ := codec.ReadHeader(f.msg); h.SchemaId == codec.SchemaID && h.TemplateId == codec.SequenceTemplateID {
				continue
			}
			wantApps = append(wantApps, describe(f.msg))
		}
	}
	_, msgs, seqs := r.meaningful(t, len(wantApps))
	var gotApps []string
	for _, m := range msgs {
		gotApps = append(gotApps, describe(m))
	}
	if fmt.Sprint(gotApps) != fmt.Sprint(wantApps) {
		t.Fatalf("answers: fix2 %q, Silverflash %q", gotApps, wantApps)
	}
	if fmt.Sprint(seqs) != "[1 2 3 4 5]" {
		t.Fatalf("fix2's sequence numbers %v, Silverflash's 1 to 5: %q", seqs, gotApps)
	}
	select {
	case g := <-gaps:
		if g != [2]uint64{4, 6} {
			t.Fatalf("gap %v", g)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no OnGap")
	}

	// Terminate.
	terminating = true
	send()
	ans = theirs()
	var wantTerm, gotTerm codec.Terminate
	wantTerm.Decode(ans[0].msg)
	_, msgs, _ = r.meaningful(t, 1)
	if _, err := gotTerm.Decode(msgs[0]); err != nil || gotTerm.SessionId != wantTerm.SessionId || gotTerm.Code != wantTerm.Code {
		t.Fatalf("Terminate: fix2 %+v, Silverflash %+v, %v", gotTerm, wantTerm, err)
	}
}

// describe names a message for the comparison: an application message by
// its body, NotApplied by its range.
func describe(msg []byte) string {
	h, _, _ := codec.ReadHeader(msg)
	if h.SchemaId == codec.SchemaID && h.TemplateId == codec.NotAppliedTemplateID {
		var na codec.NotApplied
		na.Decode(msg)
		return fmt.Sprintf("NotApplied %d+%d", na.FromSeqNo, na.Count)
	}
	if h.SchemaId == codec.SchemaID {
		return fmt.Sprintf("template %d", h.TemplateId)
	}
	return fmt.Sprintf("%d/%d %s", h.SchemaId, h.TemplateId, msg[8:8+h.BlockLength])
}
