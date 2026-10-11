package session

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// SensitiveTags are the tags whose values Redact masks by default:
// Password, NewPassword, the encrypted passwords, RawData and SecureData.
var SensitiveTags = []int{554, 925, 1402, 1404, 96, 91}

// Redact appends raw to dst with the values of tags (SensitiveTags if
// none) replaced by "***", keeping the message valid: BodyLength, CheckSum
// and the LENGTH fields of DATA fields are recomputed. raw is parsed with p
// (the default profile if nil); a message that does not parse is appended
// as "<unparsable message>".
func Redact(dst, raw []byte, p *fix2.Profile, tags ...int) []byte {
	if len(tags) == 0 {
		tags = SensitiveTags
	}
	if p == nil {
		p = fix2.DefaultProfile()
	}
	var m fix2.Message
	if err := fix2.ParseInto(&m, raw, p); err != nil {
		return append(dst, "<unparsable message>"...)
	}
	fields := m.Fields()
	start := len(dst)
	dst = fix2.AppendTagValue(dst, 8, fields[0].Value)
	dst = append(dst, "9="...)
	lenPos := len(dst)
	dst = append(dst, "0000000000\x01"...)
	bodyStart := len(dst)
	for i, f := range fields[2 : len(fields)-1] {
		v := f.Value
		if slices.Contains(tags, f.Tag) {
			v = []byte("***")
		} else if d := p.DataTag(f.Tag); d != 0 && i+3 < len(fields)-1 && fields[i+3].Tag == d && slices.Contains(tags, d) {
			v = []byte("3") // the length of "***"
		}
		dst = fix2.AppendTagValue(dst, f.Tag, v)
	}
	n := appendDecimal(nil, uint64(len(dst)-bodyStart))
	copy(dst[lenPos:], n)
	dst[lenPos+len(n)] = 1
	copy(dst[lenPos+len(n)+1:], dst[bodyStart:])
	dst = dst[:len(dst)-(10-len(n))]
	return fix2.AppendTrailer(dst, start)
}

// profileFor returns the profile raw, a whole message, parses with: for
// FIXT, that of its application version, from its ApplVerID (1128), in the
// header before any DATA field of the body, or the counterparty's default.
func (s *Session) profileFor(raw []byte) *fix2.Profile {
	if s.dicts.parseApps == nil {
		return s.dicts.parse
	}
	if i := bytes.Index(raw, []byte("\x011128=")); i >= 0 {
		v := raw[i+6:]
		if j := bytes.IndexByte(v, 1); j >= 0 {
			if p := s.dicts.parseApps[string(v[:j])]; p != nil {
				return p
			}
		}
	}
	s.mu.Lock()
	peer := s.peerApplVerID
	s.mu.Unlock()
	if p := s.dicts.parseApps[peer]; p != nil {
		return p
	}
	return s.dicts.parse
}

// AsyncObserver passes frames to another MessageObserver from a goroutine
// of its own, so that a slow observer (a file, a database) does not slow the
// session: frames are redacted into a bounded queue and dropped, and
// counted, when it is full.
type AsyncObserver struct {
	next    MessageObserver
	tags    []int
	queue   chan observed
	dropped atomic.Uint64
	done    chan struct{}
	once    sync.Once
}

type observed struct {
	s       *Session
	f       Frame
	inbound bool
}

// NewAsyncObserver returns an AsyncObserver passing frames to next, queuing
// at most size (1024 if size <= 0), with the values of tags (SensitiveTags
// if none) redacted. Close stops it.
func NewAsyncObserver(next MessageObserver, size int, tags ...int) *AsyncObserver {
	if size <= 0 {
		size = 1024
	}
	a := &AsyncObserver{next: next, tags: tags, queue: make(chan observed, size), done: make(chan struct{})}
	go a.run()
	return a
}

func (a *AsyncObserver) run() {
	defer close(a.done)
	for o := range a.queue {
		if o.inbound {
			a.next.OnInbound(o.s, o.f)
		} else {
			a.next.OnOutbound(o.s, o.f)
		}
	}
}

func (a *AsyncObserver) push(s *Session, f Frame, inbound bool) {
	f.Raw = Redact(nil, f.Raw, s.profileFor(f.Raw), a.tags...)
	select {
	case a.queue <- observed{s, f, inbound}:
	default:
		a.dropped.Add(1)
	}
}

// OnInbound implements MessageObserver.
func (a *AsyncObserver) OnInbound(s *Session, f Frame) { a.push(s, f, true) }

// OnOutbound implements MessageObserver.
func (a *AsyncObserver) OnOutbound(s *Session, f Frame) { a.push(s, f, false) }

// Dropped returns the number of frames dropped because the queue was full.
func (a *AsyncObserver) Dropped() uint64 { return a.dropped.Load() }

// Close passes the frames queued, then stops. The observer must no longer
// be called.
func (a *AsyncObserver) Close() {
	a.once.Do(func() { close(a.queue) })
	<-a.done
}

// SlogObserver logs every frame to a slog.Logger, at level Debug, with the
// values of Tags redacted: with a zerolog or zap slog handler, it logs there.
// Without a Dictionary, a frame is logged whole, as the "frame" attribute,
// SOH separators shown as "|". With one, it is logged as people read it:
// header, body and trailer in a record each, as in "<- Body: 55(Symbol)=BTCUSDT,
// 54(Side)=1(BUY)", every field named, its value described (see
// dictionary.Dict.AppendField).
type SlogObserver struct {
	Logger *slog.Logger
	// Tags are the tags whose values are redacted, SensitiveTags if nil:
	// add Username (553) where it carries an API key, for one.
	Tags []int
	// Dictionary names the fields and describes their values, if set.
	Dictionary *dictionary.Dict
}

func (o SlogObserver) log(s *Session, f Frame, inbound bool) {
	ctx := context.Background()
	if !o.Logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	p := s.profileFor(f.Raw)
	raw := Redact(nil, f.Raw, p, o.Tags...)
	attrs := []slog.Attr{slog.String("session", s.id.String()), slog.Uint64("seq", f.Seq), slog.Bool("resend", f.Resend)}
	if o.Dictionary != nil && o.logFields(ctx, raw, p, inbound, attrs) {
		return
	}
	for i, c := range raw {
		if c == 1 {
			raw[i] = '|'
		}
	}
	dir := "FIX out"
	if inbound {
		dir = "FIX in"
	}
	o.Logger.LogAttrs(ctx, slog.LevelDebug, dir, append(attrs, slog.String("frame", string(raw)))...)
}

// sectionLabels label the records of a message's header, body and trailer.
var sectionLabels = [...]string{
	dictionary.SectionHeader:  "Header: ",
	dictionary.SectionBody:    "Body:   ",
	dictionary.SectionTrailer: "Trailer:",
}

// logFields logs raw, a message, field by field, in a record per section. It
// reports false if raw does not parse.
func (o SlogObserver) logFields(ctx context.Context, raw []byte, p *fix2.Profile, inbound bool, attrs []slog.Attr) bool {
	var m fix2.Message
	if err := fix2.ParseInto(&m, raw, p); err != nil {
		return false
	}
	var parts [len(sectionLabels)][]byte
	for _, f := range m.Fields() {
		b := &parts[o.Dictionary.SectionOf(f.Tag)]
		if len(*b) > 0 {
			*b = append(*b, ", "...)
		}
		*b = o.Dictionary.AppendField(*b, f.Tag, f.Value)
	}
	dir := "-> "
	if inbound {
		dir = "<- "
	}
	for _, sec := range [...]dictionary.Section{dictionary.SectionHeader, dictionary.SectionBody, dictionary.SectionTrailer} {
		if len(parts[sec]) > 0 {
			o.Logger.LogAttrs(ctx, slog.LevelDebug, dir+sectionLabels[sec]+" "+string(parts[sec]), attrs...)
		}
	}
	return true
}

// OnInbound implements MessageObserver.
func (o SlogObserver) OnInbound(s *Session, f Frame) { o.log(s, f, true) }

// OnOutbound implements MessageObserver.
func (o SlogObserver) OnOutbound(s *Session, f Frame) { o.log(s, f, false) }
