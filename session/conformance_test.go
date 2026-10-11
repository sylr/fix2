package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
	"github.com/sylr/fix2/session"
	"github.com/sylr/fix2/session/transport"
	"github.com/sylr/fix2/session/transport/mem"
)

// TestConformance runs QuickFIX's session acceptance test scripts against
// an acceptor: FIX4.0 to 5.0 SP2, the server suites and those for
// NextExpectedMsgSeqNum, LastMsgSeqNumProcessed and ResendRequest chunking.
// FIX2_QUICKFIX_TESTS must name the _test directory of a quickfix-go
// checkout, which holds definitions/ and, next to it, spec/.
func TestConformance(t *testing.T) {
	dir := os.Getenv("FIX2_QUICKFIX_TESTS")
	if dir == "" {
		t.Skip("FIX2_QUICKFIX_TESTS is not set to quickfix-go's _test directory")
	}
	only := os.Getenv("FIX2_CONFORMANCE_ONLY") // a substring of the script paths to run
	suites := []struct {
		name string
		cfg  func(*session.Config)
	}{
		{"server", func(c *session.Config) { c.ResetOnLogon = true }},
		{"nextexpectedseqnum", func(c *session.Config) { c.NextExpectedMsgSeqNum = true }},
		{"resendreqchunksize", func(c *session.Config) { c.ResetOnLogon = true; c.ResendChunkSize = 5 }},
		{"lastseqnumprocessed", func(c *session.Config) { c.ResetOnLogon = true; c.LastMsgSeqNumProcessed = true }},
	}
	for _, suite := range suites {
		versions, err := os.ReadDir(filepath.Join(dir, "definitions", suite.name))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			scripts, _ := filepath.Glob(filepath.Join(dir, "definitions", suite.name, v.Name(), "*.def"))
			for _, script := range scripts {
				name := suite.name + "/" + v.Name() + "/" + strings.TrimSuffix(filepath.Base(script), ".def")
				if only != "" && !strings.Contains(name, only) {
					continue
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					cfg := conformanceConfig(t, filepath.Join(dir, "..", "spec"), v.Name())
					suite.cfg(&cfg)
					runScript(t, script, cfg)
				})
			}
		}
	}
}

var (
	dictsMu sync.Mutex
	dicts   = map[string]*dictionary.Dict{}
)

func loadDict(t *testing.T, path string) *dictionary.Dict {
	t.Helper()
	dictsMu.Lock()
	defer dictsMu.Unlock()
	if d := dicts[path]; d != nil {
		return d
	}
	d, err := xmldict.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dicts[path] = d
	return d
}

// conformanceConfig returns the acceptor configuration of QuickFIX's test
// server for version dir v ("fix44", "fix50sp2"...).
func conformanceConfig(t *testing.T, spec, v string) session.Config {
	cfg := session.Config{
		ID:      session.SessionID{SenderCompID: "ISLD", TargetCompID: "TW"},
		Handler: &echo{orders: map[string]bool{}},
	}
	if os.Getenv("FIX2_CONFORMANCE_LOG") != "" {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	apps := map[string]struct{ file, applVerID string }{
		"fix50": {"FIX50.xml", "7"}, "fix50sp1": {"FIX50SP1.xml", "8"}, "fix50sp2": {"FIX50SP2.xml", "9"},
	}
	if app, ok := apps[v]; ok {
		cfg.ID.BeginString = session.FIXT11
		cfg.Dictionaries = session.Dictionaries{
			Session:          loadDict(t, filepath.Join(spec, "FIXT11.xml")),
			Apps:             map[string]*dictionary.Dict{app.applVerID: loadDict(t, filepath.Join(spec, app.file))},
			DefaultApplVerID: app.applVerID,
		}
		return cfg
	}
	cfg.ID.BeginString = "FIX." + v[3:4] + "." + v[4:5]
	cfg.Dictionaries.Session = loadDict(t, filepath.Join(spec, strings.ToUpper(v[:3])+v[3:5]+".xml"))
	return cfg
}

// echo is QuickFIX's test server application: it sends back orders (D and
// d), once per ClOrdID for PossResend ones, and rejects other application
// messages as unsupported.
type echo struct {
	session.BaseHandler
	mu     sync.Mutex
	orders map[string]bool
}

func (e *echo) OnLogon(context.Context, *session.Session) {
	e.mu.Lock()
	e.orders = map[string]bool{}
	e.mu.Unlock()
}

// headerTags are the standard header's, which the echo does not copy.
var headerTags = map[int]bool{
	8: true, 9: true, 35: true, 49: true, 56: true, 115: true, 128: true, 90: true, 91: true,
	34: true, 50: true, 142: true, 57: true, 143: true, 116: true, 144: true, 129: true, 145: true,
	43: true, 97: true, 52: true, 122: true, 212: true, 213: true, 347: true, 369: true, 627: true,
	628: true, 629: true, 630: true, 1128: true, 1129: true, 1156: true, 10: true, 93: true, 89: true,
}

func (e *echo) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	mt := string(m.MsgType())
	if mt != "D" && mt != "d" {
		return &session.BusinessRejectError{Reason: session.BusinessRejectUnsupportedMessageType, Text: "Unsupported Message Type"}
	}
	if id := m.Get(11); id != nil {
		e.mu.Lock()
		seen := e.orders[string(id)]
		e.orders[string(id)] = true
		e.mu.Unlock()
		if seen && in.PossResend {
			return nil
		}
	}
	// QuickFIX writes the copy's fields in ascending tag order.
	var copied []fix2.Field
	for _, f := range m.Fields() {
		if !headerTags[f.Tag] {
			copied = append(copied, f)
		}
	}
	slices.SortStableFunc(copied, func(a, b fix2.Field) int { return a.Tag - b.Tag })
	var body []byte
	if in.PossResend {
		body = fix2.AppendTagValue(body, 97, "Y")
	}
	for _, f := range copied {
		body = fix2.AppendTagValue(body, f.Tag, f.Value)
	}
	_, err := s.SendRaw(ctx, mt, body)
	return err
}

// client is a connection of a script. Its reader delivers frames, then the
// error ending the stream, in order on one channel.
type client struct {
	conn   transport.Conn
	events chan event
}

type event struct {
	frame []byte
	err   error
}

func dial(t *testing.T, l *mem.Listener) *client {
	t.Helper()
	conn, err := l.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := &client{conn: conn, events: make(chan event, 1000)}
	go func() {
		f := fix2.NewFramer(conn, fix2.FramerOptions{})
		for {
			frame, err := f.Next()
			var g *fix2.GarbledError
			if errors.As(err, &g) {
				c.events <- event{frame: []byte("GARBLED: " + g.Error())}
				continue
			}
			if err != nil {
				c.events <- event{err: err}
				return
			}
			c.events <- event{frame: bytes.Clone(frame)}
		}
	}()
	return c
}

const scriptTimeout = 20 * time.Second

func runScript(t *testing.T, path string, cfg session.Config) {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	l := mem.Listen("acceptor")
	a := session.NewAcceptor(session.AcceptorOptions{})
	s, err := a.Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a.Listen(l)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	clients := map[int]*client{}
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || line[0] == '#' {
			continue
		}
		cid, body := 1, line[1:]
		if len(line) > 2 && line[1] >= '0' && line[1] <= '9' && line[2] == ',' {
			cid, body = int(line[1]-'0'), line[3:]
		}
		where := fmt.Sprintf("%s:%d", filepath.Base(path), n+1)
		switch line[0] {
		case 'i':
			switch {
			case body == "CONNECT":
				clients[cid] = dial(t, l)
			case body == "DISCONNECT":
				clients[cid].conn.Close()
			case strings.HasPrefix(body, "SET_SESSION"):
				fields := strings.Fields(body)
				k, v, _ := strings.Cut(fields[len(fields)-1], "=")
				n, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "NEXTSENDERSEQNUM":
					if n >= s.NextSenderSeqNum() {
						err = s.SetNextSenderSeqNum(n)
					}
				case "NEXTTARGETSEQNUM":
					err = s.SetNextTargetSeqNum(n)
				}
				if err != nil {
					t.Fatalf("%s: %s: %v", where, body, err)
				}
			default:
				t.Fatalf("%s: unknown action %q", where, line)
			}
		case 'I':
			msg := fixify(timify(body))
			if _, err := clients[cid].conn.Write(msg); err != nil {
				t.Fatalf("%s: sending %q: %v", where, visible(msg), err)
			}
		case 'E':
			want := fixify(timify(body))
			select {
			case ev := <-clients[cid].events:
				if ev.err != nil {
					t.Fatalf("%s: connection ended (%v), expected %s", where, ev.err, visible(want))
				}
				if reason := compare(want, ev.frame); reason != "" {
					t.Fatalf("%s: %s\nwant %s\ngot  %s", where, reason, visible(want), visible(ev.frame))
				}
			case <-time.After(scriptTimeout):
				t.Fatalf("%s: timed out waiting for %s", where, visible(want))
			}
		case 'e':
			if body != "DISCONNECT" {
				t.Fatalf("%s: unknown expectation %q", where, line)
			}
			select {
			case ev := <-clients[cid].events:
				if ev.err == nil {
					t.Fatalf("%s: expected a disconnection, got %s", where, visible(ev.frame))
				}
				if !errors.Is(ev.err, io.EOF) && !errors.Is(ev.err, io.ErrUnexpectedEOF) {
					t.Logf("%s: connection ended with %v", where, ev.err)
				}
			case <-time.After(scriptTimeout):
				t.Fatalf("%s: timed out waiting for the disconnection", where)
			}
		default:
			t.Fatalf("%s: syntax error %q", where, line)
		}
	}
}

var timeRe = regexp.MustCompile(`<TIME([+-]\d+)?>`)

// timify replaces <TIME>, <TIME+n> and <TIME-n> with the time now, plus or
// minus n seconds.
func timify(s string) string {
	now := time.Now().UTC()
	return timeRe.ReplaceAllStringFunc(s, func(m string) string {
		t := now
		if off := m[5 : len(m)-1]; off != "" {
			n, _ := strconv.Atoi(off)
			t = t.Add(time.Duration(n) * time.Second)
		}
		return t.Format("20060102-15:04:05")
	})
}

// fixify adds BodyLength and CheckSum to a message lacking them.
func fixify(s string) []byte {
	if !strings.HasPrefix(s, "8=") {
		return []byte(s)
	}
	head, rest, _ := strings.Cut(s, "\x01")
	head += "\x01"
	if !strings.HasPrefix(rest, "9=") {
		rest = "9=" + strconv.Itoa(len(strings.TrimSuffix(trailer.ReplaceAllString(rest, ""), "\x01x"))) + "\x01" + rest
	}
	if !trailer.MatchString(rest) {
		b := []byte(head + rest)
		return fix2.AppendTrailer(b, 0)
	}
	return []byte(head + rest)
}

var trailer = regexp.MustCompile(`\x0110=[^\x01]*\x01$|^10=[^\x01]*\x01$`)

// patterns are fields.fmt: values matched by pattern rather than compared.
var patterns = map[int]*regexp.Regexp{
	10:  regexp.MustCompile(`\d{3}`),
	42:  regexp.MustCompile(`\d{8}-\d{2}:\d{2}:\d{2}`),
	52:  regexp.MustCompile(`\d{8}-\d{2}:\d{2}:\d{2}|\d{8}-\d{2}:\d{2}:\d{2}[.]\d{3}`),
	60:  regexp.MustCompile(`\d{8}-\d{2}:\d{2}:\d{2}`),
	122: regexp.MustCompile(`\d{8}-\d{2}:\d{2}:\d{2}`),
}

type field struct {
	tag   int
	value string
}

func fields(msg []byte) []field {
	var out []field
	for _, f := range strings.Split(strings.TrimSuffix(string(msg), "\x01"), "\x01") {
		k, v, _ := strings.Cut(f, "=")
		tag, _ := strconv.Atoi(k)
		out = append(out, field{tag, v})
	}
	return out
}

// compare returns why got does not match want, or "". BodyLength and
// CheckSum are checked for validity rather than compared, and a Text is
// matched by prefix, or allowed when none is expected: ours say more than
// QuickFIX's.
func compare(want, got []byte) string {
	if _, err := fix2.Parse(got); err != nil {
		return "invalid message: " + err.Error()
	}
	wf, gf := fields(want), fields(got)
	if !slices.ContainsFunc(wf, func(f field) bool { return f.tag == 58 }) {
		gf = slices.DeleteFunc(gf, func(f field) bool { return f.tag == 58 })
	}
	if len(wf) != len(gf) {
		return fmt.Sprintf("%d fields, want %d", len(gf), len(wf))
	}
	for i := range wf {
		w, g := wf[i], gf[i]
		switch {
		case w.tag != g.tag:
			return fmt.Sprintf("field %d is tag %d, want %d", i, g.tag, w.tag)
		case w.tag == 9 || w.tag == 10:
		case w.tag == 58:
			if !strings.HasPrefix(g.value, w.value) {
				return fmt.Sprintf("Text %q does not start with %q", g.value, w.value)
			}
		case patterns[w.tag] != nil:
			if !patterns[w.tag].MatchString(g.value) {
				return fmt.Sprintf("tag %d value %q does not match %s", w.tag, g.value, patterns[w.tag])
			}
		case w.value != g.value:
			return fmt.Sprintf("tag %d is %q, want %q", w.tag, g.value, w.value)
		}
	}
	return ""
}

func visible(b []byte) string { return strings.ReplaceAll(string(b), "\x01", "|") }
