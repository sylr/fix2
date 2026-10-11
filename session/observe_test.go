package session

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	fix2 "github.com/sylr/fix2"
)

func TestRedact(t *testing.T) {
	raw := fix2.NewBuilder(FIX44, "A").SetString(49, "S").SetString(553, "user").SetString(554, "s3cr3t").
		SetData(95, 96, []byte("raw\x01data")).SetString(58, "kept").Build()
	out := Redact(nil, raw, nil)
	m, err := fix2.Parse(out)
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
	m2, _ := fix2.Parse(Redact(nil, raw, nil, 553))
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
	logon := fix2.NewBuilder(FIX44, "A").SetString(554, "pw").Build()
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
