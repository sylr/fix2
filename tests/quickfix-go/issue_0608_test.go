package quickfixgo

import (
	"context"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/608
//
// A message received in FromApp and sent back as is had its body sorted by
// tag on the way out, repeating groups scrambled (386|336, 453|448|447|452
// reordered). Forwarded with SendRaw, the body goes out as it came in.
func TestIssue608_ForwardedMessageKeepsGroupOrder(t *testing.T) {
	_, rec, p := loggedOn(t, func(c *session.Config) { c.Handler = &i608Echo{recorder: c.Handler.(*recorder)} })
	body := []string{"1", "10100400", "11", "13976", "21", "1", "386", "1", "336", "NOPL", "55", "EGMFC001MM19",
		"54", "1", "60", "20231231-20:19:41", "38", "1", "40", "2", "44", "12", "59", "0",
		"453", "1", "448", "4501", "447", "D", "452", "28", "354", "6", "355", "Public"}
	p.send("D", 0, body...)
	rec.wait(t, "the order", func() bool { return len(rec.apps) == 1 })
	var want []byte
	for i := 0; i < len(body); i += 2 {
		want = append(want, body[i]+"="+body[i+1]+"\x01"...)
	}
	if got := i608Body(p.expect("D")); got != string(want) {
		t.Errorf("forwarded body %s, want %s", visible([]byte(got)), visible(want))
	}
}

// i608Echo sends every application message back with SendRaw.
type i608Echo struct{ *recorder }

func (h *i608Echo) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	h.recorder.FromApp(ctx, s, m, in)
	_, err := s.SendRaw(ctx, string(m.MsgType()), []byte(i608Body(m)))
	return err
}

// i608Body returns the fields of m after its standard header, in wire order.
func i608Body(m *fix2.Message) string {
	var b []byte
	for _, f := range m.Fields() {
		switch f.Tag {
		case 8, 9, 35, 34, 43, 49, 52, 56, 122, 10:
			continue
		}
		b = fix2.AppendTagValue(b, f.Tag, f.Value)
	}
	return string(b)
}
