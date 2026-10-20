package fix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
	"testing/iotest"
)

// beginStrings are the BeginStrings fuzzed messages are built with.
var beginStrings = []string{"FIX.4.0", "FIX.4.2", "FIX.4.4", "FIXT.1.1", "FIX.5.0SP2"}

// FuzzParseBody frames a fuzzed body, so that BodyLength and CheckSum are
// right and the parsers get past them: Parse, ParseInto, ParseFromReader
// and the Framer must agree on it, with any Profile.
func FuzzParseBody(f *testing.F) {
	for _, body := range []string{
		"35=D\x0149=SENDER\x0156=TARGET\x0134=1\x0152=20240101-00:00:00\x0111=ORD\x0155=X\x01",
		"35=A\x0195=6\x0196=A\x0155=X\x01108=30\x01",
		"35=W\x01268=2\x01269=0\x01270=1.5\x01271=10\x01269=1\x01270=2\x01271=5\x01",
		"35=D\x01453=2\x01448=A\x01447=D\x01452=1\x01802=1\x01523=S\x01803=4\x01448=B\x01447=D\x01452=3\x01",
		"35=0\x01212=3\x01213=\x01\x01\x01",
		"35=D\x0195=99999999999999999999\x0196=x\x01",
		"35=D\x0195=-1\x0196=x\x01",
		"35=D\x0196=x\x0195=1\x01",
		"35=\x01=5\x01abc\x01",
		"35=D\x01049=SENDER\x0156=TARGET\x01",
		"035=D\x0149=SENDER\x010=x\x01",
	} {
		f.Add(byte(2), []byte(body), []byte{95, 96})
	}
	f.Fuzz(func(t *testing.T, begin byte, body, pairs []byte) {
		frame := buildMsg(beginStrings[int(begin)%len(beginStrings)], string(body))

		m, err := Parse(frame)
		var into Message
		errInto := ParseInto(&into, frame, nil)
		mr, errReader := ParseFromReader(bytes.NewReader(frame))
		if (err == nil) != (errInto == nil) || (err == nil) != (errReader == nil) {
			t.Fatalf("Parse: %v, ParseInto: %v, ParseFromReader: %v", err, errInto, errReader)
		}
		// A byte at a time, the reader parser reads alike; a reader failing
		// with its last bytes, too.
		mo, errOne := ParseFromReader(iotest.OneByteReader(bytes.NewReader(frame)))
		if (err == nil) != (errOne == nil) {
			t.Fatalf("Parse: %v, ParseFromReader a byte at a time: %v", err, errOne)
		}
		_, _ = ParseFromReader(iotest.DataErrReader(bytes.NewReader(frame)))
		// Tag numbers with leading zeros tolerated, the parsers agree too, and
		// read what they read strictly alike.
		lenient := ParseOptions{TolerateTagLeadingZeros: true}
		ml, errL := lenient.Parse(frame)
		var intoL Message
		errIntoL := lenient.ParseInto(&intoL, frame, nil)
		mrl, errReaderL := lenient.ParseFromReader(bytes.NewReader(frame))
		if (errL == nil) != (errIntoL == nil) || (errL == nil) != (errReaderL == nil) {
			t.Fatalf("tolerant Parse: %v, ParseInto: %v, ParseFromReader: %v", errL, errIntoL, errReaderL)
		}
		if errL == nil && (!slices.EqualFunc(ml.Fields(), intoL.Fields(), fieldEqual) || !slices.EqualFunc(ml.Fields(), mrl.Fields(), fieldEqual)) {
			t.Fatalf("tolerant parsers disagree:\n%v\n%v\n%v", ml.Fields(), intoL.Fields(), mrl.Fields())
		}
		if err == nil && (errL != nil || !slices.EqualFunc(m.Fields(), ml.Fields(), fieldEqual)) {
			t.Fatalf("Parse: %v, tolerant Parse: %v, %v", m.Fields(), ml.Fields(), errL)
		}
		if err == nil {
			if !slices.EqualFunc(m.Fields(), into.Fields(), fieldEqual) || !slices.EqualFunc(m.Fields(), mr.Fields(), fieldEqual) || !slices.EqualFunc(m.Fields(), mo.Fields(), fieldEqual) {
				t.Fatalf("parsers disagree:\n%v\n%v\n%v\n%v", m.Fields(), into.Fields(), mr.Fields(), mo.Fields())
			}
			if c := m.Clone(); !slices.EqualFunc(m.Fields(), c.Fields(), fieldEqual) {
				t.Fatalf("Clone: %v, want %v", c.Fields(), m.Fields())
			}
			fr, err := NewFramer(bytes.NewReader(frame), FramerOptions{}).Next()
			if err != nil || !bytes.Equal(fr, frame) {
				t.Fatalf("Framer: %q, %v; want the whole frame", fr, err)
			}
			// Twice, the second time over its body limit, a byte at a time.
			limit := len(body) / 2
			framer := NewFramer(iotest.OneByteReader(bytes.NewReader(append(slices.Clone(frame), frame...))), FramerOptions{BufferSize: 16})
			fr, err = framer.Next()
			if err != nil || !bytes.Equal(fr, frame) {
				t.Fatalf("Framer, a byte at a time: %q, %v; want the whole frame", fr, err)
			}
			framer.SetMaxBodyLength(limit)
			if fr, err := framer.Next(); err == nil && len(body) > limit && limit > 0 {
				t.Fatalf("Framer over its body limit %d: %q", limit, fr)
			}
			if _, err := RewriteForResend(nil, frame, nil, []byte("20240101-00:00:00")); err != nil {
				t.Logf("RewriteForResend: %v", err)
			}
			// Off by one to 255: refused by every parser, summing field by
			// field or at once, with SIMD or not.
			bad := slices.Clone(frame)
			cs := bad[len(bad)-4 : len(bad)-1]
			wrong := (int(CheckSum(frame[:len(frame)-7])) + 1 + int(begin)%255) % 256
			copy(cs, fmt.Sprintf("%03d", wrong))
			var bm Message
			for name, err := range map[string]error{
				"Parse":                          ParseInto(&bm, bad, nil),
				"ParseFromReader":                second(ParseFromReader(bytes.NewReader(bad))),
				"ParseFromReader, a byte a time": second(ParseFromReader(iotest.OneByteReader(bytes.NewReader(bad)))),
			} {
				if !errors.Is(err, ErrChecksumMismatch) {
					t.Fatalf("%s of %q with CheckSum %03d: %v, want %v", name, bad, wrong, err, ErrChecksumMismatch)
				}
			}
		}

		// Any profile: the DATA pairs come from the fuzzer, valid or not.
		var dp []DataPair
		for i := 0; i+1 < len(pairs) && len(dp) < 8; i += 2 {
			dp = append(dp, DataPair{Length: int(pairs[i]), Data: int(pairs[i+1])})
		}
		if p, err := NewProfile(dp); err == nil {
			var custom Message
			if ParseInto(&custom, frame, p) == nil {
				exerciseGroups(t, custom.Fields(), nil)
			}
			_, _ = RewriteForResend(nil, frame, p, []byte("20240101-00:00:00"))
		}
	})
}

func second[T any](_ T, err error) error { return err }

// checksumOracle returns the CheckSum data declares and the one FIX defines
// for it, the sum of its bytes up to the end of the body BodyLength gives,
// computed without the parsers; ok is false if data has no BodyLength and
// trailer to compute them from.
func checksumOracle(data []byte) (declared, computed int, ok bool) {
	first := bytes.IndexByte(data, soh)
	if first < 0 {
		return 0, 0, false
	}
	second := bytes.IndexByte(data[first+1:], soh)
	if second < 0 {
		return 0, 0, false
	}
	headerEnd := first + 1 + second + 1
	v, found := bytes.CutPrefix(data[first+1:headerEnd-1], []byte("9="))
	n, isNum := parseUint(v)
	if !found || !isNum || n > len(data)-headerEnd {
		return 0, 0, false
	}
	end := headerEnd + n
	trailer, found := bytes.CutPrefix(data[end:], []byte("10="))
	if !found || len(trailer) < 4 || trailer[3] != soh {
		return 0, 0, false
	}
	declared, isNum = parseUint(trailer[:3])
	if !isNum {
		return 0, 0, false
	}
	var sum int
	for _, c := range data[:end] {
		sum += int(c)
	}
	return declared, sum % 256, true
}

// checkChecksum fails t if a parser's verdict on data contradicts its
// CheckSum: accepted while wrong, or refused as mismatched while right.
func checkChecksum(t *testing.T, parser string, data []byte, err error) {
	t.Helper()
	declared, computed, ok := checksumOracle(data)
	switch {
	case err == nil && (!ok || declared != computed):
		t.Fatalf("%s accepted %q: CheckSum %d, sum %d (found %v)", parser, data, declared, computed, ok)
	case errors.Is(err, ErrChecksumMismatch) && ok && declared == computed:
		t.Fatalf("%s refused %q as mismatched, its CheckSum %d right", parser, data, declared)
	}
}

func fieldEqual(a, b Field) bool { return a.Tag == b.Tag && bytes.Equal(a.Value, b.Value) }

// FuzzGroups reads groups out of arbitrary fields, by counter and by spec
// trees built from the fuzzer's shape, as generated code reads them from
// counterparties' messages: counters lie, tags repeat, groups nest.
func FuzzGroups(f *testing.F) {
	parties := "453=2\x01448=A\x01447=D\x01452=1\x01802=1\x01523=S\x01803=4\x01448=B\x01447=D\x01452=3\x0155=X\x01"
	f.Add([]byte(parties), []byte{0, 1, 2, 2, 3, 1, 5, 6, 1, 7})
	f.Add([]byte("268=3\x01269=0\x01270=1\x01269=1\x01270=2\x01"), []byte{0, 1, 1, 2, 0})
	f.Add([]byte("268=-1\x01269=0\x01268=99999999999999999999\x01269=0\x01"), []byte{0, 1, 0, 0})
	f.Add([]byte("1=1\x011=1\x011=1\x011=1\x01"), []byte{0, 0, 255, 2, 0, 0, 0, 0, 0, 0})
	f.Add([]byte("78=2\x0179=A\x019001=x\x0180=1\x0179=B\x019001=y\x0155=X\x01"), []byte{0, 1, 1, 4, 0, 3})
	f.Fuzz(func(t *testing.T, raw, shape []byte) {
		fields := splitFields(raw)
		tags := make([]int, len(fields))
		for i, fd := range fields {
			tags[i] = fd.Tag
		}
		r := &specReader{b: shape, tags: tags}
		var specs []*GroupSpec
		for len(r.b) > 0 && len(specs) < 4 {
			specs = append(specs, r.spec(0))
		}
		exerciseGroups(t, fields, specs)
	})
}

// splitFields returns the "tag=value" fields of raw, separated by SOH,
// without the parser's checks: a field without '=' or with a tag that is
// not a number gets tag 0.
func splitFields(raw []byte) []Field {
	var fields []Field
	for len(raw) > 0 {
		f, rest, _ := bytes.Cut(raw, []byte{soh})
		raw = rest
		t, v, _ := bytes.Cut(f, []byte("="))
		tag, ok := parseUint(t)
		if !ok {
			tag = 0
		}
		fields = append(fields, Field{Tag: tag, Value: v})
	}
	return fields
}

// specReader builds group spec trees from fuzzed bytes, their tags mostly
// taken from the message's so that they match.
type specReader struct {
	b    []byte
	tags []int
}

func (r *specReader) next() int {
	if len(r.b) == 0 {
		return 0
	}
	v := int(r.b[0])
	r.b = r.b[1:]
	return v
}

func (r *specReader) tag() int {
	v := r.next()
	if len(r.tags) > 0 && v < 240 {
		return r.tags[v%len(r.tags)]
	}
	return v
}

func (r *specReader) spec(depth int) *GroupSpec {
	s := &GroupSpec{Counter: r.tag(), Delim: r.tag()}
	s.Tags = append(s.Tags, s.Delim)
	n := r.next()
	if n == 255 {
		// Wider than a bitset on the stack holds.
		for i := range 300 {
			s.Tags = append(s.Tags, 10000+i)
		}
	}
	for range n % 24 {
		s.Tags = append(s.Tags, r.tag())
	}
	if depth < 3 {
		for range r.next() % 3 {
			s.Nested = append(s.Nested, r.spec(depth+1))
		}
	}
	// Entries keep some of the tags they do not hold.
	if k := r.next(); k%2 == 1 {
		s.Unknown = func(tag int) bool { return tag%4 == k/2%4 }
	}
	return s
}

// exerciseGroups calls every group reader on fields with specs, checking
// what they promise.
func exerciseGroups(t *testing.T, fields []Field, specs []*GroupSpec) {
	m := &Message{fields: fields}
	// Each lookup scans the fields: a sample keeps long inputs linear.
	for _, f := range fields[:min(len(fields), 64)] {
		m.Get(f.Tag)
		m.GetTopLevel(f.Tag, specs...)
	}
	if got := slices.Collect(m.TopLevelFields()); !slices.EqualFunc(got, fields, fieldEqual) {
		t.Fatalf("TopLevelFields without specs: %v, want every field", got)
	}
	top := slices.Collect(m.TopLevelFields(specs...))
	if !isSubsequence(top, fields) {
		t.Fatalf("TopLevelFields: %v, not a subsequence of %v", top, fields)
	}
	lookup := func(tag int) *GroupSpec { return groupSpecFor(specs, tag) }
	if got := slices.Collect(m.TopLevelFieldsFunc(lookup)); !slices.EqualFunc(got, top, fieldEqual) {
		t.Fatalf("TopLevelFieldsFunc: %v, TopLevelFields: %v", got, top)
	}
	for i, s := range specs {
		others := slices.Delete(slices.Clone(specs), i, i+1)
		exerciseSpec(t, m.GroupsOf(s, others...), s)
	}
	// By counter, the counters and tags of the fields themselves.
	for i := 0; i+1 < len(fields) && i < 8; i++ {
		var child []int
		for _, f := range fields[i+1 : min(len(fields), i+5)] {
			child = append(child, f.Tag)
		}
		for _, g := range m.Groups(fields[i].Tag, child) {
			if len(g.Fields()) == 0 || g.Fields()[0].Tag != child[0] {
				t.Fatalf("Groups(%d, %v): entry %v does not start with its delimiter", fields[i].Tag, child, g.Fields())
			}
			for _, f := range g.Fields()[:min(len(g.Fields()), 16)] {
				g.Get(f.Tag)
			}
			g.Groups(child[0], child)
		}
	}
}

// exerciseSpec checks the entries GroupsOf returned for s, and reads their
// nested groups.
func exerciseSpec(t *testing.T, groups []Group, s *GroupSpec) {
	for _, g := range groups {
		if len(g.Fields()) == 0 || g.Fields()[0].Tag != s.Delim {
			t.Fatalf("GroupsOf: entry %v does not start with delimiter %d", g.Fields(), s.Delim)
		}
		if cap(g.fields) != len(g.fields) {
			t.Fatalf("GroupsOf: entry has spare capacity %d, appending would overwrite what follows", cap(g.fields)-len(g.fields))
		}
		for _, f := range g.Fields()[:min(len(g.Fields()), 16)] {
			g.Get(f.Tag)
			g.GetTopLevel(f.Tag, s.Nested...)
		}
		if !isSubsequence(slices.Collect(g.TopLevelFields(s.Nested...)), g.Fields()) {
			t.Fatal("Group.TopLevelFields: not a subsequence of the entry")
		}
		for i, n := range s.Nested {
			if s.Child(n.Counter) == nil {
				t.Fatalf("Child(%d) = nil", n.Counter)
			}
			others := slices.Delete(slices.Clone(s.Nested), i, i+1)
			exerciseSpec(t, g.GroupsOf(n, others...), n)
		}
	}
}

// isSubsequence reports whether sub is made of fields of all, in order.
func isSubsequence(sub, all []Field) bool {
	i := 0
	for _, f := range sub {
		for i < len(all) && !fieldEqual(all[i], f) {
			i++
		}
		if i == len(all) {
			return false
		}
		i++
	}
	return true
}

// FuzzBuilder applies fuzzed operations to a Builder, then parses what it
// builds: every field must come back as set, in order, except where FIX
// cannot carry it (a value with SOH outside a DATA field, an empty value).
func FuzzBuilder(f *testing.F) {
	f.Add([]byte{0, 0, 55, 3, 'X', 'Y', 'Z', 1, 0, 44, 2, '1', '0', 4, 0, 38, 8, 0, 0, 0, 0, 0, 0, 0xf0, 0x3f})
	f.Add([]byte{2, 0, 95, 4, 'a', 1, 'b', 'c', 0, 0, 35, 1, 'D', 0, 0, 8, 7, 'F', 'I', 'X', '.', '4', '.', '2'})
	f.Add([]byte{6, 0, 0, 0, 0, 1, 0, 0, 1, 1, 0, 0, 1, 1})
	// SetFloat on MsgType, a tag the Builder manages: 1.5.
	f.Add([]byte{4, 0, 35, 8, 0x3f, 0xf8, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, ops []byte) {
		b := NewBuilder("FIX.4.4", "D")
		begin, msgType := "FIX.4.4", "D"
		var want []Field
		set := func(tag int, value []byte) {
			for i := range want {
				if want[i].Tag == tag {
					want[i].Value = value
					return
				}
			}
			want = append(want, Field{Tag: tag, Value: value})
		}
		for len(ops) >= 4 {
			op, tag, n := ops[0]%8, int(binary.BigEndian.Uint16(ops[1:3])), int(ops[3]%32)
			ops = ops[4:]
			value := ops[:min(n, len(ops))]
			ops = ops[len(value):]
			if tag == tagBodyLen || tag == tagCheckSum {
				continue // documented to panic
			}
			switch op {
			case 0, 1:
				if op == 0 {
					b.Set(tag, value)
				} else {
					b.Append(tag, value)
				}
				switch {
				case tag == tagBeginStr:
					begin = string(value)
				case tag == tagMsgType:
					msgType = string(value)
				case op == 0:
					set(tag, bytes.Clone(value))
				default:
					want = append(want, Field{Tag: tag, Value: bytes.Clone(value)})
				}
			case 2:
				// The default profile's pairs, by index.
				pairs := defaultProfile.pairs
				p := pairs[tag%len(pairs)]
				b.SetData(p.Length, p.Data, value)
				want = slices.DeleteFunc(want, func(f Field) bool { return f.Tag == p.Length || f.Tag == p.Data })
				want = append(want, Field{Tag: p.Length, Value: []byte(itoa(len(value)))}, Field{Tag: p.Data, Value: bytes.Clone(value)})
			case 3:
				v := int(int64(binary.BigEndian.Uint64(append(make([]byte, 8-min(8, len(value))), value[:min(8, len(value))]...))))
				b.SetInt(tag, v)
				if tag != tagBeginStr && tag != tagMsgType {
					set(tag, []byte(itoa(v)))
				} else if tag == tagBeginStr {
					begin = itoa(v)
				} else {
					msgType = itoa(v)
				}
			case 4:
				v := math.Float64frombits(binary.BigEndian.Uint64(append(make([]byte, 8-min(8, len(value))), value[:min(8, len(value))]...)))
				if math.IsNaN(v) || math.IsInf(v, 0) {
					continue // documented to panic
				}
				b.SetFloat(tag, v)
				if tag != tagBeginStr && tag != tagMsgType {
					set(tag, AppendFloat(nil, v))
				} else if tag == tagBeginStr {
					begin = string(AppendFloat(nil, v))
				} else {
					msgType = string(AppendFloat(nil, v))
				}
			case 5:
				b.SetBool(tag, len(value)%2 == 1)
				v := "N"
				if len(value)%2 == 1 {
					v = "Y"
				}
				switch tag {
				case tagBeginStr:
					begin = v
				case tagMsgType:
					msgType = v
				default:
					set(tag, []byte(v))
				}
			case 6:
				b.Reset()
				want = want[:0]
			case 7:
				b.SetString(tag, string(value))
				switch tag {
				case tagBeginStr:
					begin = string(value)
				case tagMsgType:
					msgType = string(value)
				default:
					set(tag, bytes.Clone(value))
				}
			}
		}
		out := b.Build()
		if !bytes.Equal(out, b.AppendTo(nil)) || !bytes.Equal(out, b.BuildWith(nil)) {
			t.Fatal("Build, AppendTo and BuildWith differ")
		}
		_, _ = b.AppendBody(nil, nil, func(tag int) bool { return tag == 34 })
		m, err := Parse(out)
		if !carriable(begin, msgType, want) {
			return
		}
		if err != nil {
			t.Fatalf("Build wrote %q, which does not parse: %v", out, err)
		}
		got := m.Fields()
		if string(m.BeginString()) != begin || string(m.MsgType()) != msgType {
			t.Fatalf("BeginString %q MsgType %q, want %q %q", m.BeginString(), m.MsgType(), begin, msgType)
		}
		// 8, 9, 35, then the body, then 10.
		if body := got[3 : len(got)-1]; !slices.EqualFunc(body, want, fieldEqual) {
			t.Fatalf("parsed body %v, want %v", body, want)
		}
	})
}

// carriable reports whether FIX can carry a message with this BeginString,
// MsgType and body as is: a real BeginString (the parser finds shorter
// messages implausible), values not empty, holding no SOH unless DATA, and
// positive tags. A LENGTH field set by itself must announce its DATA field.
func carriable(begin, msgType string, body []Field) bool {
	if !slices.Contains(beginStrings, begin) || msgType == "" || bytes.ContainsAny([]byte(msgType), "\x01=") {
		return false
	}
	for i, f := range body {
		if f.Tag <= 0 || len(f.Value) == 0 {
			return false
		}
		switch kind := defaultProfile.kind(f.Tag); {
		case kind > 0:
			// A LENGTH tag: its DATA field must follow with as many bytes.
			if i+1 >= len(body) || body[i+1].Tag != kind || itoa(len(body[i+1].Value)) != string(f.Value) {
				return false
			}
		case kind < 0:
			// A DATA tag: announced by the field before.
			if i == 0 || defaultProfile.kind(body[i-1].Tag) != f.Tag {
				return false
			}
		default:
			if bytes.IndexByte(f.Value, soh) >= 0 {
				return false
			}
		}
	}
	return true
}

func itoa(v int) string { return strconv.Itoa(v) }

// FuzzChecksum checks the checksum the parsers and CheckSum use, vectorized
// with GOEXPERIMENT=simd, against a plain sum.
func FuzzChecksum(f *testing.F) {
	f.Add([]byte("8=FIX.4.4\x019=5\x0135=0\x01"))
	f.Add(bytes.Repeat([]byte{0xff}, 1000))
	f.Add(bytes.Repeat([]byte{0xff}, 2048*3+7))
	f.Fuzz(func(t *testing.T, data []byte) {
		var want uint32
		for _, c := range data {
			want += uint32(c)
		}
		if got := checksumBlock(data); got != want {
			t.Fatalf("checksumBlock = %d, want %d", got, want)
		}
		if got := CheckSum(data); got != byte(want) {
			t.Fatalf("CheckSum = %d, want %d", got, byte(want))
		}
		// Every alignment of the vector loads.
		for from := range min(len(data), 64) {
			if got, want := checksumBlock(data[from:]), checksumBlockGeneric(data[from:]); got != want {
				t.Fatalf("checksumBlock(data[%d:]) = %d, want %d", from, got, want)
			}
			if got, want := checksumRange(data, from, len(data)), checksumBlockGeneric(data[from:]); got != want {
				t.Fatalf("checksumRange(%d) = %d, want %d", from, got, want)
			}
		}
		// Long enough to cross the arm64 loop's flushes of its 16-bit lanes,
		// every 2048 bytes, around them and up to a large message.
		if len(data) == 0 {
			return
		}
		long := bytes.Repeat(data, 1+(1<<17)/len(data))
		for _, n := range []int{2047, 2048, 2049, 2*2048 + 15, 65535, 65536 + 17, len(long)} {
			if n > len(long) {
				continue
			}
			if got, want := checksumBlock(long[:n]), checksumBlockGeneric(long[:n]); got != want {
				t.Fatalf("checksumBlock of %d bytes = %d, want %d", n, got, want)
			}
		}
	})
}
