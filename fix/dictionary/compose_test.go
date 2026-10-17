package dictionary_test

import (
	"errors"
	"maps"
	"strconv"
	"testing"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/tests/dicts/fix50sp2"
	"github.com/sylr/fix2/fix/tests/dicts/fixt11"
)

// buildFIXT builds a FIXT.1.1 message from body fields.
func buildFIXT(t *testing.T, body string) fix.Message {
	t.Helper()
	b := []byte("8=FIXT.1.1\x019=" + strconv.Itoa(len(body)) + "\x01" + body)
	b = fix.AppendTrailer(b, 0)
	m, err := fix.Parse(b)
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

// The header's and trailer's timestamps, and those of the transport's
// messages, follow the transport's precision, FIXT 1.1's picoseconds; the
// application messages' bodies, the application's: a FIX 4.3 application
// dictionary's milliseconds.
func TestCompose_TimestampPrecision(t *testing.T) {
	transport, app := fixt11.Dictionary(), fix50sp2.Dictionary()
	ap := *app
	ap.Type, ap.Major, ap.Minor, ap.ServicePack = "FIX", "4", "3", ""
	d := dictionary.Compose(transport, &ap)
	const micro, milli = "20261011-08:00:00.123456", "20261011-08:00:00.123"
	header := func(sent string) string { return "49=S\x0156=T\x0134=2\x0152=" + sent + "\x01" }
	for _, tc := range []struct {
		name, msg string
		tag       int // 0 if valid
	}{
		{"fine SendingTime", "35=D\x01" + header(micro) + "11=ORD\x0154=1\x0160=" + milli + "\x0140=1\x01", 0},
		{"fine TransactTime", "35=D\x01" + header(micro) + "11=ORD\x0154=1\x0160=" + micro + "\x0140=1\x01", 60},
		{"transport's Logon", "35=A\x01" + header(micro) + "98=0\x01108=30\x011137=9\x01", 0},
	} {
		m := buildFIXT(t, tc.msg)
		err := d.ValidateWith(&m, dictionary.CheckTimestampPrecision)
		var ve *dictionary.ValidationError
		switch {
		case tc.tag == 0 && err != nil:
			t.Errorf("%s: %v, want valid", tc.name, err)
		case tc.tag != 0 && (!errors.As(err, &ve) || ve.Tag != tc.tag || ve.Reason != dictionary.ReasonIncorrectDataFormat):
			t.Errorf("%s: %v, want an incorrect data format for %d", tc.name, err, tc.tag)
		}
	}
}

// A FIX 4.0 application dictionary composed over FIXT 1.1 keeps its TIME
// definitions of SendingTime (52) and the like: in the header they follow
// the transport's precision, in the body the application's whole seconds.
func TestCompose_FIX40TimestampPrecision(t *testing.T) {
	app, err := xmldict.LoadFile("testdata/FIX40.xml")
	if err != nil {
		t.Fatal(err)
	}
	if fd := app.Fields[52]; fd == nil || fd.Type != "TIME" {
		t.Fatalf("FIX40.xml SendingTime = %+v, want a TIME", fd)
	}
	d := dictionary.Compose(fixt11.Dictionary(), app)
	const header = "49=S\x0156=T\x0134=2\x0152=20261011-08:00:00.123456\x01"
	for _, tc := range []struct {
		expire string
		ok     bool
	}{{"20261011-09:00:00", true}, {"20261011-09:00:00.123", false}} {
		m := buildFIXT(t, "35=D\x01"+header+"11=ORD\x0121=1\x0155=X\x0154=1\x0138=1\x0140=1\x01126="+tc.expire+"\x01")
		err := d.ValidateWith(&m, dictionary.CheckTimestampPrecision)
		var ve *dictionary.ValidationError
		switch {
		case tc.ok && err != nil:
			t.Errorf("ExpireTime %s: %v, want valid", tc.expire, err)
		case !tc.ok && (!errors.As(err, &ve) || ve.Tag != 126 || ve.Reason != dictionary.ReasonIncorrectDataFormat):
			t.Errorf("ExpireTime %s: %v, want an incorrect data format for 126", tc.expire, err)
		}
		if err := d.ValidateWith(&m, dictionary.CheckFormats); err != nil {
			t.Errorf("ExpireTime %s without CheckTimestampPrecision: %v, want valid", tc.expire, err)
		}
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
