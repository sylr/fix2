package xmldict

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
)

// TestDecodeMatchesEncodingXML checks that decode fills the xml* types as
// encoding/xml struct decoding did, reading in one go or byte by byte.
func TestDecodeMatchesEncodingXML(t *testing.T) {
	docs := map[string]string{
		"prolog, comments, CDATA, PI": `<?xml version="1.0" encoding="UTF-8"?>
<!-- <fields><field number="9" name="Hidden" type="STRING"/></fields> -->
<fix type="FIX" major="4" minor="4" servicepack="0">
  <?pi <field name="X"/> ?>
  <header><field name="A" required="Y"/><![CDATA[ <field name="Y"/> ]]></header>
  <trailer><field name="B" required="N">text</field></trailer>
  <messages><message name="M" msgtype="M" msgcat="app"><group name="G" required="N"><field name="A"/><component name="C"/></group></message></messages>
  <components><component name="C"><field name="B" required="Y"/></component></components>
  <fields>
    <field number="1" name="A" type="STRING"><value enum="x" description="a &gt; b = c"/><value enum='y' description='it&apos;s "q" &lt;'/></field>
    <field number=" 2 " name="B" type="STRING"/>
    <field number="" name="E" type="STRING"/>
  </fields>
</fix>`,
		"repeated sections, unknown elements": `<root major="5">
  <unknown><field name="Z"/><deep><deeper/></deep></unknown>
  <header><field name="A"/></header><header><field name="B"/></header>
  <messages><message name="M1" msgtype="1"/><other/></messages><messages><message name="M2" msgtype="2"><x/></message></messages>
  <components><component name="C1"/><junk/></components><components><component name="C2"/></components>
  <fields><field number="1" name="A"/><junk/></fields><fields><field number="2" name="B"><value enum="1"/><junk><value enum="2"/></junk></field></fields>
</root>`,
		"namespaces and repeated attributes": `<f:fix xmlns:f="urn:f" xmlns:g="urn:g" f:major="4" major="5">
  <fields><field g:number="7" number="8" name="A" name="B"/></fields>
</f:fix>`,
	}
	// The QuickFIX dictionaries: Orchestra repositories decode differently.
	files, _ := filepath.Glob("../testdata/FIX*.xml")
	if len(files) == 0 {
		t.Fatal("no test dictionaries found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		docs[filepath.Base(f)] = string(data)
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			var want xmlDict
			werr := xml.Unmarshal([]byte(doc), &want)
			got, err := decode(strings.NewReader(doc), defaultLimits())
			checkSame(t, "whole", &want, werr, got, err)
			got, err = decode(iotest.OneByteReader(strings.NewReader(doc)), defaultLimits())
			checkSame(t, "byte by byte", &want, werr, got, err)
		})
	}
}

func checkSame(t *testing.T, reader string, want *xmlDict, werr error, got *xmlDict, err error) {
	t.Helper()
	if (werr != nil) != (err != nil) {
		t.Fatalf("%s: decode error %v, encoding/xml error %v", reader, err, werr)
	}
	if werr == nil && !reflect.DeepEqual(want, got) {
		t.Errorf("%s: decode = %+v\nencoding/xml = %+v", reader, got, want)
	}
}

// TestDecodeNumberLikeEncodingXML covers number attribute parsing.
func TestDecodeNumberLikeEncodingXML(t *testing.T) {
	for _, number := range []string{
		`number=""`, `number=" "`, `number=" 12 "`, `number="x"`, `number="99999999999999999999"`,
		`number="-3"`, `number="+4"`, `number="0x10"`, `g:number="x" number="5"`, `number="5" g:number="x"`,
		`number="1" number="2"`,
	} {
		doc := `<fix xmlns:g="urn:g"><fields><field ` + number + ` name="A"/></fields></fix>`
		var want xmlDict
		werr := xml.Unmarshal([]byte(doc), &want)
		got, err := decode(strings.NewReader(doc), defaultLimits())
		checkSame(t, number, &want, werr, got, err)
	}
}

func TestMarkupLimits(t *testing.T) {
	attrs := func(n int) string {
		var b strings.Builder
		for i := range n {
			b.WriteString(` a` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `="="`)
		}
		return b.String()
	}
	tests := []struct {
		name string
		doc  string
		err  string // "" when valid
	}{
		{"maxAttrs attributes", `<fix` + attrs(maxAttrs) + `/>`, ""},
		{"maxAttrs+1 attributes", `<fix` + attrs(maxAttrs+1) + `/>`, "more than 16 attributes"},
		{"= and > in quotes", `<fix a="==>==" b='>>='/>`, ""},
		{"tag of maxTagLen bytes", `<fix a="` + strings.Repeat("x", maxTagLen-len(`<fix a=""/>`)) + `"/>`, ""},
		{"tag over maxTagLen bytes", `<fix a="` + strings.Repeat("x", maxTagLen) + `"/>`, "tag longer than"},
		{"long end tag", `<fix></fix` + strings.Repeat(" ", maxTagLen) + `>`, "tag longer than"},
		{"DOCTYPE", `<!DOCTYPE fix><fix/>`, "not supported"},
		{"DOCTYPE with a comment hiding a PI", `<!DOCTYPE fix [<!-- > > <?pi -->]><fix` + attrs(maxAttrs+1) + `/>`, "not supported"},
		{"DOCTYPE with a quote in a comment", `<!DOCTYPE fix [<!-- ' -->]><fix>` + strings.Repeat("x", maxTagLen) + `</fix>`, "not supported"},
		{"invalid <! sequence", `<fix><!-x></fix>`, "not supported"},
		{"invalid CDATA opening", `<fix><![CDAT x]]></fix>`, "not supported"},
		// Longer than a tag may be: the limit is not theirs.
		{"long comment", `<!--` + strings.Repeat("<a b='c'>", maxTagLen/9+1) + `--><fix/>`, ""},
		{"long CDATA", `<fix><![CDATA[` + strings.Repeat("<a b='c'>", maxTagLen/9+1) + `]]></fix>`, ""},
		{"long PI", `<?pi ` + strings.Repeat("<a b='c'>", maxTagLen/9+1) + `?><fix/>`, ""},
		{"long text", `<fix>` + strings.Repeat("a=b ", maxTagLen/4+1) + `</fix>`, ""},
		{"comment with dashes", `<!-- a-b- -x- --><fix` + attrs(maxAttrs) + `/>`, ""},
		{"comment then too many attributes", `<!-- a-b- --><fix` + attrs(maxAttrs+1) + `/>`, "more than 16 attributes"},
		{"CDATA ending in ]]]>", `<fix><![CDATA[ x ]]]><x` + attrs(maxAttrs+1) + `/></fix>`, "more than 16 attributes"},
		{"PI ending in ??>", `<?pi x ??><fix` + attrs(maxAttrs+1) + `/>`, "more than 16 attributes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, oneByte := range []bool{false, true} {
				var r interface{ Read([]byte) (int, error) } = strings.NewReader(tt.doc)
				if oneByte {
					r = iotest.OneByteReader(r)
				}
				_, err := decode(r, defaultLimits())
				if tt.err == "" && err != nil {
					t.Errorf("oneByte=%v: unexpected error %v", oneByte, err)
				}
				if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
					t.Errorf("oneByte=%v: got %v, want %q", oneByte, err, tt.err)
				}
			}
		})
	}
}

func TestLoad_AttributeFloodBounded(t *testing.T) {
	// One root with 8 MB of attributes used to allocate about 400 MiB.
	flood := `<fix` + strings.Repeat(` a=""`, 8<<20/5) + `/>`
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := Load(strings.NewReader(flood))
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "attributes") {
		t.Fatalf("got %v, want attribute limit error", err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
		t.Errorf("Load allocated %d bytes", n)
	}
}

// FuzzMarkupLimiter checks that the limiter's lexer cannot be desynchronised
// from encoding/xml: when it accepts an input, every element encoding/xml
// decodes from it is within the limits. Byte-by-byte reads must agree.
func FuzzMarkupLimiter(f *testing.F) {
	for _, seed := range []string{
		`<fix a="1" b='2'><x/></fix>`,
		`<?xml version="1.0"?><!-- c --><fix><![CDATA[ <a b="c"> ]]><?pi <x a="b"> ?></fix>`,
		`<!DOCTYPE fix [<!-- > > <?pi -->]><fix a="" b="" c="" d="" e="" f="" g="" h="" i=""/>`,
		`<fix a=">" b='"' c="'"></fix >`,
		`<!-- a-b- --><fix/>`,
		`<fix><![CDATA[ ]]]></fix>`,
		`<?pi ??><fix/>`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		lerr := limit(bytes.NewReader(data))
		if oneByte := limit(iotest.OneByteReader(bytes.NewReader(data))); (lerr == nil) != (oneByte == nil) {
			t.Fatalf("whole read: %v, byte by byte: %v", lerr, oneByte)
		}
		if lerr != nil {
			return
		}
		d := xml.NewDecoder(bytes.NewReader(data))
		for {
			start := d.InputOffset()
			tok, err := d.Token()
			if err != nil {
				return
			}
			n := d.InputOffset() - start
			switch tok := tok.(type) {
			case xml.Directive:
				t.Fatal("accepted a directive")
			case xml.StartElement:
				if len(tok.Attr) > maxAttrs || n > maxTagLen {
					t.Fatalf("accepted an element of %d bytes with %d attributes", n, len(tok.Attr))
				}
			case xml.EndElement:
				if n > maxTagLen {
					t.Fatalf("accepted an end tag of %d bytes", n)
				}
			}
		}
	})
}

// limit reads r through a markupLimiter and returns its error, if any.
func limit(r io.Reader) error {
	_, err := io.Copy(io.Discard, &markupLimiter{r: r})
	return err
}

// FuzzLoad checks that Load returns a dictionary or an error on any input,
// within bounded time and memory, and that a dictionary it returns validates
// messages without panicking: for each message type, one with all its fields
// and a bare one.
func FuzzLoad(f *testing.F) {
	// An Orchestra repository with two fields, for the seeds to add to.
	const orch = `<fixr:repository xmlns:fixr="http://fixprotocol.io/2020/orchestra/repository" name="FIX.4.4"><fixr:fields><fixr:field id="1" name="A" type="String"/><fixr:field id="2" name="N" type="NumInGroup"/></fixr:fields>`
	for _, seed := range []string{
		`<fix type="FIX" major="4" minor="4"><header><field name="MsgType" required="Y"/></header><trailer/><messages><message name="M" msgtype="M"><component name="C"/><group name="NoG" required="Y"><field name="A"/><group name="NoH"><field name="B"/></group></group></message></messages><components><component name="C"><field name="A"/><component name="D"/></component><component name="D"><field name="B" required="Y"/></component></components><fields><field number="35" name="MsgType" type="STRING"/><field number="1" name="A" type="STRING"/><field number="2" name="B" type="INT"><value enum="1" description="ONE"/></field><field number="3" name="NoG" type="NUMINGROUP"/><field number="4" name="NoH" type="NUMINGROUP"/></fields></fix>`,
		`<fix><messages><message name="M" msgtype="M"><component name="C"/></message></messages><components><component name="C"><component name="C"/></component></components></fix>`,
		`<fix><messages><message name="M" msgtype="M"><group name="G"><group name="G"><field name="A"/></group></group></message></messages><fields><field number="1" name="A" type="X"/><field number="1" name="G" type="NUMINGROUP"/></fields></fix>`,
		`<fix><messages><message name="M" msgtype="M"><field name="Missing"/></message></messages></fix>`,
		`<fix><messages><message name="M" msgtype="M"/><message name="N" msgtype="M"/></messages><fields><field number="-1" name="A"/><field number="0" name="B"/><field number="1" name="A"/></fields></fix>`,
		`<fix><header><group name="NoHops"><field name="HopCompID"/></group></header><fields><field number="627" name="NoHops" type="NUMINGROUP"/><field number="628" name="HopCompID" type="STRING"/></fields></fix>`,
		`<fix><messages><message name="M" msgtype="M"><group name="NoG"><field name="Len"/><field name="Data"/></group></message></messages><fields><field number="1" name="NoG" type="NUMINGROUP"/><field number="95" name="Len" type="LENGTH"/><field number="96" name="Data" type="DATA"/></fields></fix>`,
		`<`, ``, `<fix>`, `<fix><messages><message/></messages></fix>`,
		testOrchestraXML,
		`<fixr:repository xmlns:fixr="http://fixprotocol.io/2020/orchestra/repository" name="FIX.4.4"><fixr:fields><fixr:field id="1" name="A" type="String"/><fixr:field id="2" name="N" type="NumInGroup"/></fixr:fields><fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="2"/><fixr:fieldRef id="1"/><fixr:groupRef id="5"/></fixr:group></fixr:groups><fixr:messages><fixr:message name="M" msgType="M"><fixr:structure><fixr:groupRef id="5"/></fixr:structure></fixr:message></fixr:messages></fixr:repository>`,
		// Malformed QuickFIX dictionaries: an unclosed element in a component
		// and in a field, an element no body holds, a trailer field and a
		// group counter that are not defined.
		`<fix><components><component name="C"><x></component></components></fix>`,
		`<fix><messages><message name="M" msgtype="M"><field name="A"><`,
		`<fix><messages><message name="M" msgtype="M"><x/></message></messages></fix>`,
		`<fix><trailer><field name="Missing"/></trailer></fix>`,
		`<fix><header><group name="NoHops"/></header></fix>`,
		// Malformed Orchestra repositories: an element each list skips, ids
		// that are not numbers, references to a field that is not defined.
		orch + `<fixr:codeSets><fixr:x/></fixr:codeSets></fixr:repository>`,
		orch + `<fixr:fields><fixr:x/></fixr:fields></fixr:repository>`,
		orch + `<fixr:groups><fixr:x/></fixr:groups></fixr:repository>`,
		orch + `<fixr:messages><fixr:x/></fixr:messages></fixr:repository>`,
		orch + `<fixr:groups><fixr:group id="x" name="G"/></fixr:groups></fixr:repository>`,
		orch + `<fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="x"/></fixr:group></fixr:groups></fixr:repository>`,
		orch + `<fixr:groups><fixr:group id="5" name="G"><fixr:numInGroup id="2"/><fixr:fieldRef id="99"/></fixr:group></fixr:groups></fixr:repository>`,
		orch + `<fixr:components><fixr:component id="7" name="C"><fixr:fieldRef id="99"/></fixr:component></fixr:components></fixr:repository>`,
	} {
		f.Add([]byte(seed))
	}
	for _, name := range []string{"FIX40", "FIX44", "FIXT11", "OrchestraFIXLatest"} {
		if data, err := os.ReadFile("../testdata/" + name + ".xml"); err == nil {
			f.Add(data)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		d, err := Load(bytes.NewReader(data))
		runtime.ReadMemStats(&after)
		if took := time.Since(start); took > 5*time.Second {
			t.Fatalf("Load took %v for %d bytes", took, len(data))
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 1<<30 {
			t.Fatalf("Load allocated %d bytes for %d bytes", n, len(data))
		}
		if err != nil {
			if d != nil {
				t.Fatalf("Load returned a dictionary and %v", err)
			}
			return
		}
		useLoaded(t, d)
	})
}

// useLoaded validates messages with d, as a session would.
func useLoaded(t *testing.T, d *dictionary.Dict) {
	t.Helper()
	begin := "FIX." + d.Major + "." + d.Minor
	if strings.ContainsRune(begin, 1) {
		return
	}
	// Every message, in a fixed order, so that an input reproduces.
	for _, mt := range slices.Sorted(maps.Keys(d.Messages)) {
		md := d.Messages[mt]
		var all strings.Builder
		for _, ref := range md.Fields {
			fmt.Fprintf(&all, "%d=1\x01", ref.Tag)
		}
		for _, g := range md.Groups {
			fmt.Fprintf(&all, "%d=2\x01%d=1\x01%d=1\x01", g.CounterTag, g.DelimTag, g.DelimTag)
		}
		for _, body := range []string{"", all.String()} {
			msg := "35=" + md.MsgType + "\x01" + body
			wire := fmt.Sprintf("8=%s\x019=%d\x01%s", begin, len(msg), msg)
			var sum byte
			for i := range len(wire) {
				sum += wire[i]
			}
			wire += fmt.Sprintf("10=%03d\x01", sum)
			var m fix.Message
			if err := fix.ParseInto(&m, []byte(wire), d.Profile()); err != nil {
				continue
			}
			err := d.ValidateWith(&m, dictionary.CheckAll)
			var ve *dictionary.ValidationError
			if err != nil && !errors.As(err, &ve) {
				t.Fatalf("ValidateWith returned %T: %v", err, err)
			}
			d.Validate(&m)
			d.Sections(&m)
			for _, fl := range m.Fields() {
				d.AppendField(nil, fl.Tag, fl.Value)
			}
		}
	}
	dictionary.Compose(d, d)
}
