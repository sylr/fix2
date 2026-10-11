package dictionary_test

import (
	"errors"
	"maps"
	"strconv"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/tests/fix50sp2"
	"github.com/sylr/fix2/tests/fixt11"
)

// buildFIXT builds a FIXT.1.1 message from body fields.
func buildFIXT(t *testing.T, body string) fix2.Message {
	t.Helper()
	b := []byte("8=FIXT.1.1\x019=" + strconv.Itoa(len(body)) + "\x01" + body)
	b = fix2.AppendTrailer(b, 0)
	m, err := fix2.Parse(b)
	if err != nil {
		t.Fatalf("Parse(%q): %v", b, err)
	}
	return m
}

func TestCompose(t *testing.T) {
	transport, app := fixt11.Dictionary(), fix50sp2.Dictionary()
	d := dictionary.Compose(transport, app)
	const header = "49=S\x0156=T\x0134=2\x0152=20261011-08:00:00.000\x011128=9\x01"
	order := buildFIXT(t, "35=D\x01"+header+"11=ORD\x0154=1\x0160=20261011-08:00:00\x0140=1\x01")
	if err := d.Validate(&order); err != nil {
		t.Errorf("composed: NewOrderSingle with a FIXT header: %v", err)
	}
	if err := app.Validate(&order); !errors.Is(err, dictionary.ErrUnexpectedTag) {
		t.Errorf("app alone: got %v, want the header tags unexpected", err)
	}
	logon := buildFIXT(t, "35=A\x01"+header+"98=0\x01108=30\x011137=9\x01")
	if err := d.Validate(&logon); err != nil {
		t.Errorf("composed: Logon: %v", err)
	}
	// Required header tags come from the transport dictionary.
	noSender := buildFIXT(t, "35=D\x0156=T\x0134=2\x0152=20261011-08:00:00.000\x0111=ORD\x0154=1\x0160=20261011-08:00:00\x0140=1\x01")
	var ve *dictionary.ValidationError
	if err := d.Validate(&noSender); !errors.As(err, &ve) || ve.Tag != 49 {
		t.Errorf("composed: missing SenderCompID: got %v", err)
	}
	if d.ServicePack != app.ServicePack || d.Major != "5" {
		t.Errorf("composed version %s.%s SP%s, want the app's", d.Major, d.Minor, d.ServicePack)
	}
	// MsgType values of both dictionaries are valid.
	if fd := d.Fields[35]; fd.Enums != nil && (fd.Enums["A"] == "" || fd.Enums["D"] == "") {
		t.Errorf("composed MsgType enums lack A or D")
	}
	// The originals keep validating as before.
	if err := transport.Validate(&logon); err != nil {
		t.Errorf("transport after Compose: Logon: %v", err)
	}
	if err := app.Validate(&order); !errors.Is(err, dictionary.ErrUnexpectedTag) {
		t.Errorf("app after Compose: got %v", err)
	}
}

// An empty enum map restricts nothing, in Compose as in Validate.
func TestCompose_EmptyEnums(t *testing.T) {
	transport, app := fixt11.Dictionary(), fix50sp2.Dictionary()
	tr := *transport
	tr.Fields = maps.Clone(transport.Fields)
	text := *tr.Fields[58]
	text.Enums = map[string]string{}
	tr.Fields[58] = &text
	ap := *app
	ap.Fields = maps.Clone(app.Fields)
	appText := dictionary.FieldDef{Tag: 58, Name: "Text", Type: "STRING", Enums: map[string]string{"A": "A"}}
	ap.Fields[58] = &appText
	d := dictionary.Compose(&tr, &ap)
	if fd := d.Fields[58]; len(fd.Enums) != 0 {
		t.Errorf("composed Text enums = %v, want none", fd.Enums)
	}
}
