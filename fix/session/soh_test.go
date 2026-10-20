package session_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/dictionary"
	"github.com/sylr/fix2/fix/dictionary/xmldict"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/transport/mem"
)

// customFIX44 returns the test FIX 4.4 dictionary with a DATA field only it
// pairs with its LENGTH field, CustomBlob (50002) and CustomBlobLen
// (50001), on Logon and in NewOrderSingle's NoBlobs (50003) entries, after
// BlobName (50004). It does not define EncryptedPassword (1402).
func customFIX44(t *testing.T) *dictionary.Dict {
	t.Helper()
	raw, err := os.ReadFile("../dictionary/testdata/FIX44.xml")
	if err != nil {
		t.Fatal(err)
	}
	xml := string(raw)
	for _, r := range []struct{ old, new string }{
		{"<fields>", `<fields>
    <field number='50001' name='CustomBlobLen' type='LENGTH'/>
    <field number='50002' name='CustomBlob' type='DATA'/>
    <field number='50003' name='NoBlobs' type='NUMINGROUP'/>
    <field number='50004' name='BlobName' type='STRING'/>`},
		{"<field name='Password' required='N'/>", `<field name='Password' required='N'/>
      <field name='CustomBlobLen' required='N'/>
      <field name='CustomBlob' required='N'/>`},
		{"<field name='Text' required='N'/>\n    </message>\n    <message name='BusinessMessageReject'", `<field name='Text' required='N'/>
      <group name='NoBlobs' required='N'>
        <field name='BlobName' required='N'/>
        <field name='CustomBlobLen' required='N'/>
        <field name='CustomBlob' required='N'/>
      </group>
    </message>
    <message name='BusinessMessageReject'`},
	} {
		if strings.Count(xml, r.old) != 1 {
			t.Fatalf("FIX44.xml: %q not found once", r.old)
		}
		xml = strings.Replace(xml, r.old, r.new, 1)
	}
	d, err := xmldict.Load(strings.NewReader(xml))
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile().DataTag(50001) != 50002 || d.Profile().DataTag(1401) != 0 {
		t.Fatal("custom dictionary: unexpected DATA fields")
	}
	return d
}

// blobLogon adds CustomBlob to its Logons, set by hand rather than with
// SetData: the session dictionary pairs it with its length.
type blobLogon struct{ *recorder }

func (blobLogon) ToAdmin(_ context.Context, _ *session.Session, msgType string, b *fix.Builder) error {
	if msgType == "A" {
		b.SetInt(50001, 3).Set(50002, []byte("l\x01n"))
	}
	return nil
}

// expectParsed reads the next message and parses it with p.
func (p *peer) expectParsed(prof *fix.Profile, msgType string) *fix.Message {
	p.t.Helper()
	raw := p.expectRaw()
	var m fix.Message
	if err := fix.ParseInto(&m, raw, prof); err != nil {
		p.t.Fatalf("invalid message %s: %v", visible(raw), err)
	}
	if string(m.MsgType()) != msgType {
		p.t.Fatalf("got %s, want MsgType %s", visible(raw), msgType)
	}
	return &m
}

// The session dictionary decides which fields are DATA: a value holding SOH
// goes out in one of its pairs, from ToAdmin as from Send, a group entry's
// included, and is refused in a pair only the default profile has.
func TestSOHInDictionaryDataFields(t *testing.T) {
	d := customFIX44(t)
	l := mem.Listen("acceptor")
	s, rec := startInitiator(t, l, func(c *session.Config) {
		c.Dictionaries.Session = d
		c.Handler = blobLogon{c.Handler.(*recorder)}
	})
	p := acceptPeer(t, l)
	if got := p.expectParsed(d.Profile(), "A").Get(50002); string(got) != "l\x01n" {
		t.Errorf("Logon's CustomBlob = %q", got)
	}
	p.send("A", 0, "98", "0", "108", "30")
	rec.wait(t, func() bool { return rec.logons == 1 })

	o := order("O1")
	o.SetGroups(func(dst []fix.Field) []fix.Field {
		dst = fix.AppendCount(dst, 50003, 1)
		return append(dst, fix.Field{Tag: 50004, Value: []byte("b")},
			fix.Field{Tag: 50001, Value: []byte("3")}, fix.Field{Tag: 50002, Value: []byte("o\x01d")})
	}, 50003)
	if _, err := s.Send(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	m := p.expectParsed(d.Profile(), "D")
	if g := m.Groups(50003, []int{50004, 50001, 50002}); len(g) != 1 || string(g[0].Get(50002)) != "o\x01d" {
		t.Errorf("NoBlobs = %v", g)
	}

	// EncryptedPassword (1402) is DATA in the default profile, not in the
	// dictionary: its SOH would end it, a forged Text following.
	var serr *fix.SOHValueError
	forged := order("O2").SetInt(1401, 12).SetString(1402, "O1\x0158=forged")
	if _, err := s.Send(context.Background(), forged); !errors.As(err, &serr) || serr.Tag != 1402 {
		t.Errorf("Send = %v, want a SOHValueError for 1402", err)
	}
}

// The session writes MsgType apart from the body: one holding SOH is
// refused before anything is sequenced, whichever way it was set.
func TestSOHInMsgType(t *testing.T) {
	s, _, p := loggedOn(t, nil)
	for name, send := range map[string]func() error{
		"NewBuilder": func() error {
			_, err := s.Send(context.Background(), fix.NewBuilder(session.FIX44, "D\x0158=forged").SetString(11, "O1"))
			return err
		},
		"SetString": func() error {
			_, err := s.Send(context.Background(), order("O1").SetString(35, "D\x0158=forged"))
			return err
		},
		"SendRaw": func() error {
			_, err := s.SendRaw(context.Background(), "D\x0158=forged", []byte("11=O1\x01"))
			return err
		},
	} {
		var serr *fix.SOHValueError
		if err := send(); !errors.As(err, &serr) || serr.Tag != 35 {
			t.Errorf("%s: Send = %v, want a SOHValueError for 35", name, err)
		}
	}
	if _, err := s.Send(context.Background(), order("O2")); err != nil {
		t.Fatal(err)
	}
	p.expect("D", "34", "2", "11", "O2")
}

// A handler's BusinessRejectError whose RefID holds SOH.
type sohRefID struct{ *recorder }

func (sohRefID) FromApp(context.Context, *session.Session, *fix.Message, session.Inbound) error {
	return &session.BusinessRejectError{Reason: session.BusinessRejectUnknownID, RefID: "O1\x0158=forged", Text: "unknown order"}
}

// A RefID holding SOH would end BusinessRejectRefID (379) early, the rest
// read as fields of their own: the BusinessMessageReject goes without it,
// with a warning, and the message counts as processed.
func TestBusinessRejectRefIDWithSOH(t *testing.T) {
	logs := &logBuffer{}
	s, _, p := loggedOn(t, func(c *session.Config) {
		c.Handler = sohRefID{c.Handler.(*recorder)}
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	p.send("D", 0, "11", "O1")
	rej := p.expect("j", "45", "2", "372", "D", "380", "1", "58", "unknown order")
	if rej.Get(379) != nil {
		t.Errorf("BusinessRejectRefID sent: %q", rej.Get(379))
	}
	n := 0
	for _, f := range rej.Fields() {
		if f.Tag == 58 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d Text fields, want 1", n)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "BusinessRejectRefID holds SOH") {
		t.Errorf("no warning logged:\n%s", out)
	}
	if got := s.Stats().RejectsSent.Load(); got != 1 {
		t.Errorf("RejectsSent = %d, want 1", got)
	}
	// Processed: the session goes on, expecting the next one.
	p.send("1", 0, "112", "T1")
	p.expect("0", "112", "T1")
	if got := s.NextTargetSeqNum(); got != 4 {
		t.Errorf("NextTargetSeqNum = %d, want 4", got)
	}
}

// A ToAdmin setting a value holding SOH cancels the Logon, as a ToAdmin
// error does: nothing is sent.
func TestToAdminSOH(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) { c.Handler = user{c.Handler.(*recorder), "a\x0158=forged"} })
	acceptPeer(t, l).expectNoLogon()
}

// sohSigner signs its Logons with a Text holding SOH.
type sohSigner struct{ *recorder }

func (sohSigner) SignLogon(_ context.Context, _ *session.Session, _ session.LogonHeader, b *fix.Builder) error {
	b.SetString(58, "a\x0158=forged")
	return nil
}

// A LogonSigner setting a value holding SOH cancels the Logon, as its error
// does: nothing is sent.
func TestLogonSignerSOH(t *testing.T) {
	l := mem.Listen("acceptor")
	startInitiator(t, l, func(c *session.Config) { c.Handler = sohSigner{c.Handler.(*recorder)} })
	acceptPeer(t, l).expectNoLogon()
}

// expectNoLogon waits for the connection to end before any message.
func (p *peer) expectNoLogon() {
	p.t.Helper()
	select {
	case ev := <-p.frames:
		if ev.err == nil {
			p.t.Fatalf("got %s, want no Logon", visible(ev.frame))
		}
	case <-time.After(5 * time.Second):
		p.t.Fatal("connection still open")
	}
}

// The identities the session writes in every header cannot hold SOH.
func TestConfigSOH(t *testing.T) {
	_, err := session.New(session.Config{
		ID:      session.SessionID{BeginString: session.FIX44, SenderCompID: "INI", TargetCompID: "ACC", TargetSubID: "a\x01b"},
		Handler: session.BaseHandler{},
	})
	if err == nil || !strings.Contains(err.Error(), "ID.TargetSubID") {
		t.Errorf("New = %v, want an error about ID.TargetSubID", err)
	}
}
