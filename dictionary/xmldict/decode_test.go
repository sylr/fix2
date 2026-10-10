package xmldict

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
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
	files, _ := filepath.Glob("../../cmd/fixgen/testdata/*.xml")
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
			got, err := decode(strings.NewReader(doc))
			checkSame(t, "whole", &want, werr, got, err)
			got, err = decode(iotest.OneByteReader(strings.NewReader(doc)))
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
		got, err := decode(strings.NewReader(doc))
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
		{"maxAttrs+1 attributes", `<fix` + attrs(maxAttrs+1) + `/>`, "more than 8 attributes"},
		{"= and > in quotes", `<fix a="==>==" b='>>='/>`, ""},
		{"tag of maxTagLen bytes", `<fix a="` + strings.Repeat("x", maxTagLen-len(`<fix a=""/>`)) + `"/>`, ""},
		{"tag over maxTagLen bytes", `<fix a="` + strings.Repeat("x", maxTagLen) + `"/>`, "tag longer than"},
		{"long end tag", `<fix></fix` + strings.Repeat(" ", maxTagLen) + `>`, "tag longer than"},
		{"DOCTYPE", `<!DOCTYPE fix><fix/>`, "not supported"},
		{"DOCTYPE with a comment hiding a PI", `<!DOCTYPE fix [<!-- > > <?pi -->]><fix` + attrs(maxAttrs+1) + `/>`, "not supported"},
		{"DOCTYPE with a quote in a comment", `<!DOCTYPE fix [<!-- ' -->]><fix>` + strings.Repeat("x", maxTagLen) + `</fix>`, "not supported"},
		{"invalid <! sequence", `<fix><!-x></fix>`, "not supported"},
		{"invalid CDATA opening", `<fix><![CDAT x]]></fix>`, "not supported"},
		{"long comment", `<!--` + strings.Repeat("<a b='c'>", maxTagLen) + `--><fix/>`, ""},
		{"long CDATA", `<fix><![CDATA[` + strings.Repeat("<a b='c'>", maxTagLen) + `]]></fix>`, ""},
		{"long PI", `<?pi ` + strings.Repeat("<a b='c'>", maxTagLen) + `?><fix/>`, ""},
		{"long text", `<fix>` + strings.Repeat("a=b ", maxTagLen) + `</fix>`, ""},
		{"comment with dashes", `<!-- a-b- -x- --><fix` + attrs(maxAttrs) + `/>`, ""},
		{"comment then too many attributes", `<!-- a-b- --><fix` + attrs(maxAttrs+1) + `/>`, "more than 8 attributes"},
		{"CDATA ending in ]]]>", `<fix><![CDATA[ x ]]]><x` + attrs(maxAttrs+1) + `/></fix>`, "more than 8 attributes"},
		{"PI ending in ??>", `<?pi x ??><fix` + attrs(maxAttrs+1) + `/>`, "more than 8 attributes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, oneByte := range []bool{false, true} {
				var r interface{ Read([]byte) (int, error) } = strings.NewReader(tt.doc)
				if oneByte {
					r = iotest.OneByteReader(r)
				}
				_, err := decode(r)
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
