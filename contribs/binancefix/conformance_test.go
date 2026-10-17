package binancefix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sylr/fix2/contribs/binancefix/internal/fixsbe"
	"github.com/sylr/fix2/contribs/binancefix/internal/lesofh"
	"github.com/sylr/fix2/transport"
)

// tap wraps the client end of a connection, counting the bytes the client
// read: a test writing part of a frame waits for the client to have read
// it before writing the rest, so that the client's reads split there.
type tap struct {
	transport.Conn
	n atomic.Int64
}

func (c *tap) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.n.Add(int64(n))
	return n, err
}

// waitRead waits for the client to have read n bytes.
func (c *tap) waitRead(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for c.n.Load() < int64(n) {
		if time.Now().After(deadline) {
			t.Fatalf("the client read %d bytes, want %d", c.n.Load(), n)
		}
		time.Sleep(20 * time.Microsecond)
	}
}

// frameOf returns the frame of m with the peer's next sequence number.
func (p *peer) frameOf(t *testing.T, m encoder) []byte {
	t.Helper()
	b, err := m.AppendWithHeader(nil, MessageHeader{SeqNum: p.seq, SendingTime: time.Now().UnixMicro()})
	if err != nil {
		t.Fatal(err)
	}
	f, err := lesofh.AppendFrame(nil, b)
	if err != nil {
		t.Fatal(err)
	}
	p.seq++
	return f
}

// A frame split in two reads, at every byte boundary (in the SOFH, at its
// end, in the SBE header, in the body), is read whole; so is a frame whose
// first part comes in the read of the frame before it.
func TestFragmentedReads(t *testing.T) {
	f, h := newFake(t), newRecorder()
	var tp *tap
	f.wrap = func(c transport.Conn) transport.Conn { tp = &tap{Conn: c}; return tp }
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	read := int(tp.n.Load()) // the LogonAck
	size := len(p.frameOf(t, report("s000")))
	p.seq--
	if size <= lesofh.HeaderLen+fixsbe.HeaderSize {
		t.Fatalf("frame of %d bytes", size)
	}
	for k := 1; k < size; k++ {
		id := fmt.Sprintf("s%03d", k)
		fr := p.frameOf(t, report(id))
		p.write(t, fr[:k])
		tp.waitRead(t, read+k)
		p.write(t, fr[k:])
		read += len(fr)
		if r := h.next(t, "er"); r.id != id || r.in.Header.SeqNum != p.seq-1 {
			t.Fatalf("split at %d: %q, seqNum %d", k, r.id, r.in.Header.SeqNum)
		}
	}
	for k := range size {
		a, b := p.frameOf(t, report(fmt.Sprintf("a%03d", k))), p.frameOf(t, report(fmt.Sprintf("b%03d", k)))
		p.write(t, append(a, b[:k]...))
		tp.waitRead(t, read+len(a)+k)
		p.write(t, b[k:])
		read += len(a) + len(b)
		for _, id := range []string{fmt.Sprintf("a%03d", k), fmt.Sprintf("b%03d", k)} {
			if r := h.next(t, "er"); r.id != id {
				t.Fatalf("pair split at %d: %q, want %q", k, r.id, id)
			}
		}
	}
	if !c.LoggedOn() {
		t.Error("connection ended")
	}
}

// Many frames in one read are each handled, in order, session messages
// among them.
func TestCoalescedReads(t *testing.T) {
	f, h := newFake(t), newRecorder()
	c, _ := start(t, testConfig(f, h), time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	const n = 300
	var stream []byte
	for i := range n {
		if i == n/2 {
			stream = append(stream, p.frameOf(t, &fixsbe.TestRequest{TestReqID: []byte("mid")})...)
			stream = append(stream, p.frameOf(t, &fixsbe.Heartbeat{TestReqID: []byte{}})...)
		}
		stream = append(stream, p.frameOf(t, report(fmt.Sprintf("c%03d", i)))...)
	}
	p.write(t, stream)
	for i := range n {
		if r := h.next(t, "er"); r.id != fmt.Sprintf("c%03d", i) {
			t.Fatalf("message %d: %q", i, r.id)
		}
	}
	if hb := decode[fixsbe.Heartbeat](t, p.expect(t, fixsbe.HeartbeatTemplateID)); string(hb.TestReqID) != "mid" {
		t.Errorf("TestRequest answered with %q", hb.TestReqID)
	}
}

// A frame the client cannot read, or a message it cannot take, ends the
// connection with a ProtocolError, after our Logout; a stream ending in a
// frame ends it with a read error, without.
func TestMalformedFrames(t *testing.T) {
	const maxLen = 512
	short, err := (&fixsbe.Heartbeat{TestReqID: []byte{}}).AppendWithHeader(nil, MessageHeader{SeqNum: 2})
	if err != nil {
		t.Fatal(err)
	}
	otherSchema := bytes.Clone(short)
	binary.LittleEndian.PutUint16(otherSchema[4:], 2)
	le := func(n uint32, enc uint16, msg []byte) []byte {
		b := binary.LittleEndian.AppendUint32(nil, n)
		return append(binary.LittleEndian.AppendUint16(b, enc), msg...)
	}
	frame := func(msg []byte) []byte { return le(uint32(lesofh.HeaderLen+len(msg)), 0xEB50, msg) }
	for _, tc := range []struct {
		name     string
		data     []byte
		protocol bool  // a ProtocolError, after our Logout
		is       error // what it wraps
	}{
		{"big-endian encodingType", le(26, 0x50EB, short), true, lesofh.ErrEncoding},
		{"SBE 1.0 big-endian encodingType", le(26, 0x5BE0, short), true, lesofh.ErrEncoding},
		{"FIX encodingType", le(26, 0xF000, short), true, lesofh.ErrEncoding},
		{"zero length", le(0, 0xEB50, nil), true, lesofh.ErrShortLength},
		{"length short of the SOFH", le(5, 0xEB50, nil), true, lesofh.ErrShortLength},
		{"empty message", frame(nil), true, fixsbe.ErrShort},
		{"message short of the SBE header", frame(short[:fixsbe.HeaderSize-1]), true, fixsbe.ErrShort},
		{"MaxFrameLength exceeded by one", le(maxLen+1, 0xEB50, nil), true, lesofh.ErrLongLength},
		{"4 GiB length", le(math.MaxUint32, 0xEB50, nil), true, lesofh.ErrLongLength},
		{"other schema", frame(otherSchema), true, nil},
		{"stream ending in the SOFH", le(26, 0xEB50, nil)[:3], false, io.ErrUnexpectedEOF},
		{"stream ending in the message", le(26, 0xEB50, short[:10]), false, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			cfg.MaxFrameLength = maxLen
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			r, err := c.Send(order("o1"))
			if err != nil {
				t.Fatal(err)
			}
			p.expect(t, fixsbe.NewOrderSingleTemplateID)
			p.write(t, tc.data)
			if !tc.protocol {
				p.c.Close()
			}
			e := h.next(t, "logout").ev.(LogoutEvent)
			var pe *ProtocolError
			if errors.As(e.Err, &pe) != tc.protocol || (tc.is != nil && !errors.Is(e.Err, tc.is)) || e.Clean != tc.protocol {
				t.Errorf("LogoutEvent %+v", e)
			}
			if tc.protocol {
				lo := decode[fixsbe.Logout](t, p.expect(t, fixsbe.LogoutTemplateID))
				if len(lo.Text) == 0 {
					t.Error("Logout without Text")
				}
			}
			if r.State() != PossiblyTransmitted || len(e.PossiblyTransmitted) != 1 {
				t.Errorf("receipt %v, event %s", r.State(), describe(e.PossiblyTransmitted))
			}
			if err := runErr(t, done); !errors.Is(err, e.Err) {
				t.Errorf("Run: %v, LogoutEvent: %v", err, e.Err)
			}
		})
	}
}

// A Handler blocked for several HeartBtInts, the event queue having room:
// our Heartbeats and TestRequests go on, Binance's TestRequests are
// answered, the answers to ours are read, and the connection lives.
func TestHandlerStallBeyondHeartBtInt(t *testing.T) {
	const interval = 100 * time.Millisecond
	f, h := newFake(t), newRecorder()
	release := make(chan struct{})
	stalled := make(chan struct{})
	var once sync.Once
	h.onER = func(Inbound, *ExecutionReport) {
		once.Do(func() { close(stalled) })
		<-release
	}
	c, _ := start(t, testConfig(f, h), interval)
	// Released whatever happens, before start's cleanup waits for Run,
	// which waits for the Handler (cleanups run last registered first).
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.send(t, report("x"))
	select {
	case <-stalled:
	case <-time.After(wait):
		t.Fatal("the Handler did not get the ExecutionReport")
	}
	since := time.Now()

	// Binance silent: our Heartbeats, our TestRequests, answered.
	heartbeats, tests := 0, 0
	for until := since.Add(4 * interval); time.Now().Before(until); {
		select {
		case m, ok := <-p.msgs:
			if !ok {
				t.Fatalf("connection ended: %v", p.err)
			}
			switch m.h.TemplateId {
			case fixsbe.HeartbeatTemplateID:
				heartbeats++
			case fixsbe.TestRequestTemplateID:
				tests++
				p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(decode[fixsbe.TestRequest](t, m).TestReqID)})
			case fixsbe.LogoutTemplateID:
				t.Fatalf("Logout: %q", decode[fixsbe.Logout](t, m).Text)
			}
		case <-time.After(interval / 4):
		}
	}
	if heartbeats < 2 || tests < 1 {
		t.Errorf("%d Heartbeats, %d TestRequests in 4 HeartBtInts", heartbeats, tests)
	}

	// Binance's TestRequests: each answered.
	for i := range 4 {
		id := "s" + strconv.Itoa(i)
		p.send(t, &fixsbe.TestRequest{TestReqID: []byte(id)})
		for answered := false; !answered; {
			m := p.next(t)
			switch m.h.TemplateId {
			case fixsbe.HeartbeatTemplateID:
				answered = string(decode[fixsbe.Heartbeat](t, m).TestReqID) == id
			case fixsbe.TestRequestTemplateID:
				p.send(t, &fixsbe.Heartbeat{TestReqID: bytes.Clone(decode[fixsbe.TestRequest](t, m).TestReqID)})
			case fixsbe.LogoutTemplateID:
				t.Fatalf("Logout: %q", decode[fixsbe.Logout](t, m).Text)
			}
		}
	}
	if d := time.Since(since); d < 4*interval {
		t.Fatalf("stalled %v only", d)
	}
	if !c.LoggedOn() {
		t.Fatal("connection ended")
	}
	unblock()
	if r := h.next(t, "er"); r.id != "x" {
		t.Errorf("ExecutionReport %q", r.id)
	}
}

// A write failing after n bytes of a batch of two requests, for every n
// in the first's frame and at the edges of the second's: the requests begun
// are PossiblyTransmitted, the others NeverWritten; the peer reads the
// frames wholly written.
func TestPartialWriteEverySplit(t *testing.T) {
	fa, err := encode(order("a"))
	if err != nil {
		t.Fatal(err)
	}
	fb, err := encode(order("b"))
	if err != nil {
		t.Fatal(err)
	}
	var splits []int
	for n := range len(fa) + 2 {
		splits = append(splits, n)
	}
	splits = append(splits, len(fa)+len(fb)-1, len(fa)+len(fb))
	for _, n := range splits {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			var g *gate
			f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			steps := make(chan int)
			g.mu.Lock()
			g.steps = steps
			g.mu.Unlock()
			blk, _ := c.Send(order("blk"))
			g.waitWrite(t)
			a, _ := c.Send(order("a"))
			b, _ := c.Send(order("b"))
			steps <- -1 // blk
			g.waitWrite(t)
			steps <- n // a and b, one write
			e := h.next(t, "logout").ev.(LogoutEvent)
			possibly, never := []*Receipt{blk}, []*Receipt(nil)
			for _, x := range []struct {
				r     *Receipt
				begun bool
			}{{a, n > 0}, {b, n > len(fa)}} {
				if x.begun {
					possibly = append(possibly, x.r)
				} else {
					never = append(never, x.r)
				}
			}
			if describe(e.PossiblyTransmitted) != describe(possibly) || describe(e.NeverWritten) != describe(never) {
				t.Errorf("PossiblyTransmitted %s, NeverWritten %s", describe(e.PossiblyTransmitted), describe(e.NeverWritten))
			}
			for _, r := range possibly {
				if r.State() != PossiblyTransmitted {
					t.Errorf("%v: %v", r.IDs(), r.State())
				}
			}
			for _, r := range never {
				if r.State() != NeverWritten {
					t.Errorf("%v: %v", r.IDs(), r.State())
				}
			}
			orders := 0
			for _, m := range p.ended(t) {
				if m.h.TemplateId == fixsbe.NewOrderSingleTemplateID {
					orders++
				}
			}
			want := 1
			if n >= len(fa) {
				want++
			}
			if n >= len(fa)+len(fb) {
				want++
			}
			if orders != want {
				t.Errorf("the peer read %d orders, want %d", orders, want)
			}
			if err := runErr(t, done); err == nil {
				t.Error("Run: nil")
			}
		})
	}
}

// The connection lost (by the peer, without Logout) at each phase of a
// request: queued behind another, in the writer's hands with nothing
// written, wholly written but the write not returned, partly written,
// written, answered. Nothing written is NeverWritten, anything written
// and unanswered PossiblyTransmitted, an answer final.
func TestDisconnectPhases(t *testing.T) {
	for _, phase := range []string{"queued", "in the writer", "writing", "partly written", "written", "answered"} {
		t.Run(phase, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			var g *gate
			f.wrap = func(c transport.Conn) transport.Conn { g = newGate(c); g.release(0); return g }
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			steps := make(chan int, 1)
			g.mu.Lock()
			g.steps = steps
			g.mu.Unlock()
			var never, possibly []*Receipt
			var answered *Receipt
			send := func(id string) *Receipt {
				r, err := c.Send(order(id))
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			disconnect := true
			switch phase {
			case "queued":
				blk := send("blk")
				g.waitWrite(t)
				r := send("r")
				waitState(t, r, Queued)
				never = []*Receipt{blk, r}
			case "in the writer":
				r := send("r")
				g.waitWrite(t)
				never = []*Receipt{r}
			case "writing":
				r := send("r")
				g.waitWrite(t)
				steps <- -2 // written, the write not returned
				p.expect(t, fixsbe.NewOrderSingleTemplateID)
				possibly = []*Receipt{r}
			case "partly written":
				r := send("r")
				g.waitWrite(t)
				steps <- 10
				possibly, disconnect = []*Receipt{r}, false
			case "written":
				r := send("r")
				g.waitWrite(t)
				steps <- -1
				p.expect(t, fixsbe.NewOrderSingleTemplateID)
				waitState(t, r, Written)
				possibly = []*Receipt{r}
			case "answered":
				r := send("r")
				g.waitWrite(t)
				steps <- -1
				p.expect(t, fixsbe.NewOrderSingleTemplateID)
				p.send(t, report("r"))
				if x := h.next(t, "er"); x.in.Receipt != r {
					t.Fatalf("ExecutionReport %+v", x)
				}
				answered = r
			}
			if disconnect {
				p.c.Close()
			}
			e := h.next(t, "logout").ev.(LogoutEvent)
			if e.Err == nil || e.Clean {
				t.Errorf("LogoutEvent %+v", e)
			}
			if describe(e.NeverWritten) != describe(never) || describe(e.PossiblyTransmitted) != describe(possibly) {
				t.Errorf("NeverWritten %s, PossiblyTransmitted %s", describe(e.NeverWritten), describe(e.PossiblyTransmitted))
			}
			for _, r := range never {
				if r.State() != NeverWritten || r.Err() == nil {
					t.Errorf("%v: %v, %v", r.IDs(), r.State(), r.Err())
				}
			}
			for _, r := range possibly {
				if r.State() != PossiblyTransmitted || r.Err() == nil {
					t.Errorf("%v: %v, %v", r.IDs(), r.State(), r.Err())
				}
			}
			if answered != nil {
				if st, err := answered.Wait(t.Context()); st != Answered || err != nil {
					t.Errorf("answered: %v, %v", st, err)
				}
			}
			if err := runErr(t, done); err == nil {
				t.Error("Run: nil")
			}
		})
	}
}

// Binance's sample Logon, read byte by byte by the client's framing and
// decoded by its codecs, as it reads Binance's messages: a version 0
// message of schema 1.
func TestLogonSampleRead(t *testing.T) {
	sample := unhex(t, logonSample)
	fr := lesofh.NewReader(iotest.OneByteReader(bytes.NewReader(sample)), 0)
	msg, err := fr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fr.Next(); err != io.EOF {
		t.Errorf("after the sample: %v", err)
	}
	h, body, err := fixsbe.ReadHeader(msg)
	if err != nil {
		t.Fatal(err)
	}
	want := MessageHeader{BlockLength: fixsbe.LogonBlockLength, TemplateId: fixsbe.LogonTemplateID, SchemaId: fixsbe.SchemaID,
		Version: 0, SeqNum: 1, SendingTime: time.Date(2024, 6, 27, 9, 17, 25, 223e6, time.UTC).UnixMicro()}
	if h != want {
		t.Errorf("header %+v, want %+v", h, want)
	}
	var l fixsbe.Logon
	if n, err := l.DecodeBody(h, body); err != nil || n != len(body) {
		t.Fatalf("DecodeBody = %d, %v; want %d", n, err, len(body))
	}
	if l.EncryptMethod != Some[uint8](0) || l.HeartBtInt != 30 || l.ResetSeqNumFlag != Some(fixsbe.BoolEnumTrue) ||
		l.MessageHandling != Some(fixsbe.MessageHandlingSequential) || l.ResponseMode.Valid || l.ExecutionReportType.Valid ||
		l.DropCopyFlag.Valid || l.RecvWindow.Valid || string(l.SenderCompId) != "EXAMPLE" || string(l.TargetCompId) != TargetCompID ||
		string(l.RawData) != docSignature || string(l.Username) != docAPIKey {
		t.Errorf("Logon %+v", l)
	}
	// What the client builds of the same Config is that Logon.
	cfg := &Config{APIKey: docAPIKey, SenderCompID: "EXAMPLE", HeartBtInt: 30 * time.Second, MessageHandling: fixsbe.MessageHandlingSequential}
	built := buildLogon(cfg, []byte(docSignature))
	if fmt.Sprintf("%+v", *built) != fmt.Sprintf("%+v", l) {
		t.Errorf("built %+v\ndecoded %+v", *built, l)
	}
}

// A LogonAck whose required fields are null (their null values, which SBE
// forbids a producer to send: §4.4.2) or empty, or out of Binance's
// bounds, does not establish the session: the Logon failed, retried as one
// Binance refused, MaxRefusals times at most, OnLogon never called.
func TestInvalidLogonAck(t *testing.T) {
	const interval = 10 * time.Millisecond
	for _, tc := range []struct {
		name    string
		ack     fixsbe.LogonAck
		missing bool // ErrMissingRequired
	}{
		{"HeartBtInt null", fixsbe.LogonAck{HeartBtInt: math.MaxUint32, UUID: []byte("u")}, true},
		{"HeartBtInt 0", fixsbe.LogonAck{HeartBtInt: 0, UUID: []byte("u")}, false},
		{"HeartBtInt 4", fixsbe.LogonAck{HeartBtInt: 4, UUID: []byte("u")}, false},
		{"HeartBtInt 61", fixsbe.LogonAck{HeartBtInt: 61, UUID: []byte("u")}, false},
		{"SbeSchemaIdVersionDeprecated null", fixsbe.LogonAck{HeartBtInt: 30, SbeSchemaIdVersionDeprecated: 255, UUID: []byte("u")}, true},
		{"SbeSchemaIdVersionDeprecated 2", fixsbe.LogonAck{HeartBtInt: 30, SbeSchemaIdVersionDeprecated: 2, UUID: []byte("u")}, false},
		{"UUID empty", fixsbe.LogonAck{HeartBtInt: 30, UUID: []byte{}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.MaxRefusals = 2
			c, done := start(t, cfg, interval)
			for range 2 {
				p := f.accept()
				p.expect(t, fixsbe.LogonTemplateID)
				p.send(t, &tc.ack)
				p.ended(t)
			}
			err := runErr(t, done)
			var refused *LogonRefusedError
			if !errors.As(err, &refused) || !errors.Is(err, ErrInvalidLogonAck) || errors.Is(err, ErrMissingRequired) != tc.missing {
				t.Errorf("Run: %v", err)
			}
			select {
			case r := <-h.recs:
				t.Errorf("event %s", r.kind)
			default:
			}
			if c.LoggedOn() {
				t.Error("logged on")
			}
		})
	}
	// Bounds and values allowed.
	for _, ack := range []fixsbe.LogonAck{
		{HeartBtInt: 5, UUID: []byte("u")},
		{HeartBtInt: 60, SbeSchemaIdVersionDeprecated: fixsbe.BoolEnumTrue, UUID: []byte("u")},
		{EncryptMethod: Some[uint8](0), HeartBtInt: 30, UUID: []byte("u")},
	} {
		if err := checkLogonAck(&ack); err != nil {
			t.Errorf("%+v: %v", ack, err)
		}
	}
}

// Logged on, a News without its required Headline is told of, its
// countdown unknown: a notice, it does not end the session.
func TestNewsWithoutHeadline(t *testing.T) {
	f, h := newFake(t), newRecorder()
	cfg := testConfig(f, h)
	cfg.Reconnect.Disabled = true
	c, _ := start(t, cfg, time.Hour)
	p := f.accept()
	p.logon(t, &c.cfg)
	h.next(t, "logon")
	p.send(t, &fixsbe.News{Headline: []byte{}})
	if e := h.next(t, "news").ev.(NewsEvent); e.Headline != "" || e.Remaining != -1 {
		t.Errorf("NewsEvent %+v", e)
	}
	p.send(t, &fixsbe.TestRequest{TestReqID: []byte("STILL")})
	if hb := decode[fixsbe.Heartbeat](t, p.expect(t, fixsbe.HeartbeatTemplateID)); string(hb.TestReqID) != "STILL" {
		t.Errorf("Heartbeat %q, want STILL", hb.TestReqID)
	}
}

// Logged on, a TestRequest without TestReqID, a required string (its
// answer could not be told from a Heartbeat), ends the connection with a
// ProtocolError, after our Logout. A Heartbeat without TestReqID,
// optional, is taken.
func TestSessionMessageMissingRequired(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    encoder
	}{
		{"TestRequest", &fixsbe.TestRequest{TestReqID: []byte{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, h := newFake(t), newRecorder()
			cfg := testConfig(f, h)
			cfg.Reconnect.Disabled = true
			c, done := start(t, cfg, time.Hour)
			p := f.accept()
			p.logon(t, &c.cfg)
			h.next(t, "logon")
			p.send(t, &fixsbe.Heartbeat{TestReqID: []byte{}})
			p.send(t, tc.m)
			lo := decode[fixsbe.Logout](t, p.expect(t, fixsbe.LogoutTemplateID))
			if !bytes.Contains(lo.Text, []byte(tc.name)) {
				t.Errorf("Logout %q", lo.Text)
			}
			e := h.next(t, "logout").ev.(LogoutEvent)
			var pe *ProtocolError
			if !errors.As(e.Err, &pe) || !errors.Is(e.Err, ErrMissingRequired) || !e.Clean {
				t.Errorf("LogoutEvent %+v", e)
			}
			if err := runErr(t, done); !errors.Is(err, ErrMissingRequired) {
				t.Errorf("Run: %v", err)
			}
		})
	}
}
