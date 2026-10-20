package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
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
	// A tag number with leading zeros, which a session tolerating them
	// receives, is redacted as its number says, and written without them.
	const body = "35=A\x0149=S\x010554=s3cr3t\x01"
	lz := fmt.Appendf(nil, "8=FIX.4.4\x019=%d\x01%s", len(body), body)
	lz = fmt.Appendf(lz, "10=%03d\x01", fix.CheckSum(lz))
	out = Redact(nil, lz, nil)
	if m, err := fix.Parse(out); err != nil || string(m.Get(554)) != "***" || bytes.Contains(out, []byte("s3cr3t")) {
		t.Errorf("Redact of a tag with leading zeros = %q, %v", out, err)
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

	buf.Reset()
	o.Separator = "---"
	o.OnInbound(s, Frame{Raw: fix.NewBuilder(FIX44, "0").Build(), Seq: 1})
	if got := buf.String(); !strings.Contains(got, `"msg":"---"}`+"\n") || strings.Index(got, `"msg":"---"`) > strings.Index(got, `"msg":"FIX in"`) || strings.Contains(got, `\u001b`) {
		t.Errorf("with a Separator: %s", got)
	}
}

// With a dictionary, header, body and trailer are logged field by field.
func TestSlogObserverDictionary(t *testing.T) {
	d, err := xmldict.LoadFile("../dictionary/testdata/FIX44.xml")
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
		`\x1b[38;5;147m35(MsgType)=D(ORDER_SINGLE)\x1b[0m, 49(SenderCompID)=A, 56(TargetCompID)=B, 34(MsgSeqNum)=2"`, // lavender, escaped by the text handler
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

	for _, tc := range []struct {
		name       string
		o          SlogObserver
		want, lack []string
	}{
		{"NoColor", SlogObserver{NoColor: true, Colors: map[int]string{11: "1"}}, []string{`9(BodyLength)=31, 35(MsgType)=D(ORDER_SINGLE), 49`, `11(ClOrdID)=O1,`}, []string{`\x1b`}},
		{"Colors", SlogObserver{Colors: map[int]string{11: "1"}}, []string{`31, 35(MsgType)=D(ORDER_SINGLE), 49`, `\x1b[1m11(ClOrdID)=O1\x1b[0m, 54`}, nil},
		{"Separator", SlogObserver{Separator: "---"}, []string{"level=DEBUG msg=---\ntime="}, []string{"msg=--- session"}},
	} {
		buf.Reset()
		tc.o.Logger, tc.o.Dictionary = o.Logger, d
		tc.o.OnOutbound(s, Frame{Raw: order, Seq: 2})
		got := buf.String()
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: log lacks %s:\n%s", tc.name, want, got)
			}
		}
		for _, lack := range tc.lack {
			if strings.Contains(got, lack) {
				t.Errorf("%s: log has %s:\n%s", tc.name, lack, got)
			}
		}
		if tc.name == "Separator" && !strings.HasPrefix(got[strings.Index(got, "level="):], "level=DEBUG msg=---\n") {
			t.Errorf("Separator: not logged first:\n%s", got)
		}
	}
}

// yieldHandler yields between records, so that loggers racing interleave.
type yieldHandler struct{ slog.Handler }

func (h yieldHandler) Handle(ctx context.Context, r slog.Record) error {
	runtime.Gosched()
	return h.Handler.Handle(ctx, r)
}

// The records of a message, its separator's among them, follow each other
// although the reader and the writer of a session log at the same time.
func TestSlogObserverConcurrent(t *testing.T) {
	d, err := xmldict.LoadFile("../dictionary/testdata/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{ID: SessionID{BeginString: FIX44, SenderCompID: "A", TargetCompID: "B"}, Handler: BaseHandler{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		d      *dictionary.Dict
		sep    string
		labels []string // of the records after the separator
	}{
		{"Dictionary", d, "---", []string{"Header: ", "Body:   ", "Trailer:"}},
		{"DictionaryWithoutSeparator", d, "", []string{"Header: ", "Body:   ", "Trailer:"}},
		{"Frame", nil, "---", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			o := SlogObserver{
				Logger:     slog.New(yieldHandler{slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})}),
				Dictionary: tc.d,
				Separator:  tc.sep,
			}
			const n = 200
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, inbound := range []bool{true, false} {
				wg.Go(func() {
					<-start
					for i := range n {
						f := Frame{Raw: fix.NewBuilder(FIX44, "0").SetInt(34, i).SetString(112, "T").Build(), Seq: uint64(i)}
						if inbound {
							o.OnInbound(s, f)
						} else {
							o.OnOutbound(s, f)
						}
					}
				})
			}
			close(start)
			wg.Wait()
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			per := len(tc.labels)
			if tc.sep != "" {
				per++
			}
			if len(lines) != 2*n*per {
				t.Fatalf("%d records, want %d", len(lines), 2*n*per)
			}
			for i := 0; i < len(lines); i += per {
				if tc.sep != "" && !strings.Contains(lines[i], `"msg":"---"`) {
					t.Fatalf("record %d is not the separator:\n%s", i, strings.Join(lines[i:i+per], "\n"))
				}
				var first map[string]any
				for j, label := range tc.labels {
					var rec map[string]any
					if err := json.Unmarshal([]byte(lines[i+per-len(tc.labels)+j]), &rec); err != nil {
						t.Fatal(err)
					}
					msg, _ := rec["msg"].(string)
					if j == 0 {
						first = rec
					}
					if !strings.HasPrefix(msg, first["msg"].(string)[:3]+label) || rec["seq"] != first["seq"] {
						t.Fatalf("records %d to %d are not one message's:\n%s", i, i+per-1, strings.Join(lines[i:i+per], "\n"))
					}
				}
			}
		})
	}
}
