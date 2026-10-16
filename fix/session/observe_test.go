package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
)

func TestRedact(t *testing.T) {
	raw := fix.NewBuilder(FIX44, "A").SetString(49, "S").SetString(553, "user").SetString(554, "s3cr3t").
		SetData(95, 96, []byte("raw\x01data")).SetString(58, "kept").Build()
	out := Redact(nil, raw, nil)
	m, err := fix.Parse(out)
	if err != nil {
		t.Fatalf("redacted message does not parse: %v\n%q", err, out)
	}
	for tag, want := range map[int]string{554: "***", 96: "***", 95: "3", 553: "user", 58: "kept", 49: "S"} {
		if got := string(m.Get(tag)); got != want {
			t.Errorf("tag %d = %q, want %q", tag, got, want)
		}
	}
	if bytes.Contains(out, []byte("s3cr3t")) {
		t.Error("password left in the redacted message")
	}
	if got := string(Redact(nil, []byte("garbage"), nil)); got != "<unparsable message>" {
		t.Errorf("Redact(garbage) = %q", got)
	}
	// Chosen tags only.
	m2, _ := fix.Parse(Redact(nil, raw, nil, 553))
	if string(m2.Get(553)) != "***" || string(m2.Get(554)) != "s3cr3t" {
		t.Error("Redact with tags did not redact those alone")
	}
}

type collect struct {
	mu     sync.Mutex
	frames []string
	block  chan struct{}
}

func (c *collect) OnInbound(_ *Session, f Frame) {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	c.frames = append(c.frames, "in "+string(f.Raw))
	c.mu.Unlock()
}

func (c *collect) OnOutbound(_ *Session, f Frame) {
	c.mu.Lock()
	c.frames = append(c.frames, "out "+string(f.Raw))
	c.mu.Unlock()
}

func TestAsyncObserver(t *testing.T) {
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"}, Handler: BaseHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	c := &collect{}
	a := NewAsyncObserver(c, 16)
	logon := fix.NewBuilder(FIX44, "A").SetString(554, "pw").Build()
	a.OnInbound(s, Frame{Raw: logon, Seq: 1})
	a.OnOutbound(s, Frame{Raw: logon, Seq: 1})
	a.Close()
	if len(c.frames) != 2 || !strings.HasPrefix(c.frames[0], "in ") || !strings.HasPrefix(c.frames[1], "out ") {
		t.Fatalf("frames = %q", c.frames)
	}
	if strings.Contains(c.frames[0], "554=pw") {
		t.Error("frame not redacted")
	}

	blocked := &collect{block: make(chan struct{})}
	b := NewAsyncObserver(blocked, 1)
	for range 10 {
		b.OnInbound(s, Frame{Raw: logon})
	}
	close(blocked.block)
	b.Close()
	if b.Dropped() == 0 || b.Dropped()+uint64(len(blocked.frames)) != 10 {
		t.Errorf("dropped %d, delivered %d of 10", b.Dropped(), len(blocked.frames))
	}
}

// The frame is an attribute of its own: "message" is the message's key in
// zerolog, and "msg" in zap and slog's own handlers.
func TestSlogObserver(t *testing.T) {
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"}, Handler: BaseHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	o := SlogObserver{Logger: slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	o.OnInbound(s, Frame{Raw: fix.NewBuilder(FIX44, "A").SetString(554, "pw").Build(), Seq: 1})
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	frame, _ := rec["frame"].(string)
	if rec["msg"] != "FIX in" || !strings.HasPrefix(frame, "8=FIX.4.4|") || !strings.Contains(frame, "554=***") {
		t.Errorf("record %v", rec)
	}

	buf.Reset()
	o.Tags = []int{553}
	o.OnInbound(s, Frame{Raw: fix.NewBuilder(FIX44, "A").SetString(553, "key").SetString(554, "pw").Build(), Seq: 1})
	if got := buf.String(); !strings.Contains(got, "553=***") || !strings.Contains(got, "554=pw") {
		t.Errorf("with Tags 553: %s", got)
	}
}

// With a dictionary, header, body and trailer are logged field by field.
func TestSlogObserverDictionary(t *testing.T) {
	d, err := xmldict.LoadFile("../cmd/fixgen/testdata/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"}, Handler: BaseHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	o := SlogObserver{
		Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Dictionary: d,
	}
	order := fix.NewBuilder(FIX44, "D").SetString(49, "A").SetString(56, "B").SetInt(34, 2).
		SetString(11, "O1").SetString(54, "1").Build()
	o.OnOutbound(s, Frame{Raw: order, Seq: 2})
	o.OnInbound(s, Frame{Raw: fix.NewBuilder(FIX44, "A").SetString(554, "pw").SetString(925, "new").Build(), Seq: 1})
	o.OnInbound(s, Frame{Raw: fix.NewBuilder(FIX44, "5").SetString(58, "bye\nFORGED").Build(), Seq: 2})
	got := buf.String()
	for _, want := range []string{
		`msg="-> Header:  8(BeginString)=FIX.4.4, 9(BodyLength)=`,
		`35(MsgType)=D(ORDER_SINGLE), 49(SenderCompID)=A, 56(TargetCompID)=B, 34(MsgSeqNum)=2"`,
		`msg="-> Body:    11(ClOrdID)=O1, 54(Side)=1(BUY)"`,
		`msg="-> Trailer: 10(CheckSum)=`,
		`msg="<- Body:    554(Password)=***, 925=***"`, // redacted, known to the dictionary (Password) or not (NewPassword)
		"session=FIX.4.4:A->B seq=2",
		`msg="<- Body:    58(Text)=bye\\nFORGED"`, // escaped twice: by AppendField, then by the text handler
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s:\n%s", want, got)
		}
	}
}
