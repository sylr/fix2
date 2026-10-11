package dictionary_test

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/dictionary/xmldict"
)

// fuzzDicts are the dictionaries FuzzValidate runs against: the test
// dictionaries of cmd/fixgen, FIX 5.0 SP2 composed with FIXT 1.1 as a session
// does.
var fuzzDicts = sync.OnceValue(func() []fuzzDict {
	load := func(name string) *dictionary.Dict {
		d, err := xmldict.LoadFile("../cmd/fixgen/testdata/" + name + ".xml")
		if err != nil {
			panic(err)
		}
		return d
	}
	return []fuzzDict{
		{"FIX.4.0", load("FIX40")},
		{"FIX.4.4", load("FIX44")},
		{"FIXT.1.1", dictionary.Compose(load("FIXT11"), load("FIX50SP2"))},
	}
})

type fuzzDict struct {
	begin string
	dict  *dictionary.Dict
}

// frame returns a message of the given BeginString around body, which holds
// MsgType and what follows, with the BodyLength and CheckSum that make it
// parse.
func frame(begin string, body []byte) []byte {
	if len(body) == 0 || body[len(body)-1] != 1 {
		body = append(bytes.Clone(body), 1)
	}
	msg := []byte("8=" + begin + "\x019=" + strconv.Itoa(len(body)) + "\x01")
	msg = append(msg, body...)
	var sum byte
	for _, c := range msg {
		sum += c
	}
	return append(msg, fmt.Sprintf("10=%03d\x01", sum)...)
}

// soh makes the readable seeds of the fuzz targets, with '|' for SOH.
func soh(s string) []byte { return bytes.ReplaceAll([]byte(s), []byte("|"), []byte{1}) }

// FuzzValidate checks that validating a message a counterparty could send
// never panics, whichever checks are on, and that the errors are the
// ValidationErrors Validate documents. Input is the dictionary and checks
// to use, and the message from MsgType on, framed so it parses.
func FuzzValidate(f *testing.F) {
	const hdr = "49=S|56=T|34=1|52=20231001-12:00:00.000|"
	for _, s := range []struct {
		sel  uint8
		body string
	}{
		{0, "35=D|" + hdr + "11=O1|21=1|55=IBM|54=1|60=20231001-12:00:00|38=100|40=2|44=10.5|"},
		{0, "35=8|" + hdr + "37=O|11=O1|17=E|20=0|39=0|150=0|55=IBM|54=1|38=1|14=0|6=0|151=1|"},
		{0, "35=A|" + hdr + "98=0|108=30|95=5|96=ab\x01cd|"},
		{0, "35=A|" + hdr + "95=99|96=ab|"},
		{0, "35=0|" + hdr},
		{1, "35=D|" + hdr + "11=O|54=1|60=20231001-12:00:00|40=2|55=X|78=2|79=A|80=1|5000=2|5001=a|5002=b|79=B|5000=0|"},
		{1, "35=D|" + hdr + "11=O|54=1|60=20231001-12:00:00|40=2|55=X|78=3|79=A|79=B|"},
		{1, "35=D|" + hdr + "11=O|54=x|60=bad|40=2|55=|78=-1|44=1e5|"},
		{1, "35=D|" + hdr + "35=D|11=O|11=P|10=000|"},
		{1, "35=Z|" + hdr},
		{2, "35=D|" + hdr + "11=O|54=1|60=20231001-12:00:00|40=2|55=X|453=2|448=P1|447=D|452=1|802=1|523=s|803=1|448=P2|"},
		{2, "35=D|" + hdr + "11=O|54=1|55=X|453=1|448=P1|539=2|524=n|525=D|538=1|"},
		{2, "35=A|" + hdr + "98=0|108=30|1137=9|95=3|96=a|b|"},
		{2, "35=A|1128=9|627=2|628=a|629=20231001-12:00:00|630=1|628=b|"},
		{2, "35=8|" + hdr + "453=99999999999999999999|"},
	} {
		f.Add(s.sel, uint8(dictionary.CheckAll), []byte(s.body))
		f.Add(s.sel, uint8(0), []byte(s.body))
	}
	f.Fuzz(func(t *testing.T, sel, checks uint8, body []byte) {
		fd := fuzzDicts()[int(sel)%len(fuzzDicts())]
		d := fd.dict
		data := frame(fd.begin, soh(string(body)))
		var msg fix2.Message
		if err := fix2.ParseInto(&msg, data, d.Profile()); err != nil {
			return
		}
		verr := d.Validate(&msg)
		checkValidationError(t, verr)
		cs := dictionary.Checks(checks) & dictionary.CheckAll
		werr := d.ValidateWith(&msg, cs)
		checkValidationError(t, werr)
		if verr != nil && werr == nil {
			t.Fatalf("Validate = %v, but ValidateWith(%d) = nil", verr, cs)
		}
		if werr := d.ValidateWith(&msg, dictionary.CheckAll); werr == nil && verr != nil {
			t.Fatalf("Validate = %v, but ValidateWith(CheckAll) = nil", verr)
		}
		if e := d.ValidateWith(&msg, 0); fmt.Sprint(e) != fmt.Sprint(verr) {
			t.Fatalf("Validate = %v, ValidateWith(0) = %v", verr, e)
		}

		fields := msg.Fields()
		secs := d.Sections(&msg)
		if len(secs) != len(fields) {
			t.Fatalf("Sections returned %d sections for %d fields", len(secs), len(fields))
		}
		var line []byte
		for i, f := range fields {
			if s := secs[i]; s < dictionary.SectionHeader || s > dictionary.SectionTrailer {
				t.Fatalf("section %d of field %d", s, i)
			}
			line = d.AppendField(line[:0], f.Tag, f.Value)
			if bytes.ContainsAny(line, "\x01\n\r") {
				t.Fatalf("AppendField(%d, %q) = %q holds a control character", f.Tag, f.Value, line)
			}
		}
	})
}

// checkValidationError fails unless err is nil or a ValidationError with a
// sentinel and a message.
func checkValidationError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var ve *dictionary.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("validation returned %T: %v", err, err)
	}
	if !errors.Is(err, dictionary.ErrValidation) || ve.Err == nil || ve.Tag < 0 || ve.Msg == "" {
		t.Fatalf("incomplete validation error: %+v", ve)
	}
}
