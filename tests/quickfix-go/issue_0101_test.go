package quickfixgo

import (
	"context"
	"slices"
	"testing"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
	"github.com/sylr/fix2/session"
)

// https://github.com/quickfixgo/quickfix/issues/101
//
// FIXT application messages were routed as FIX 5.0 whatever their version:
// FIX 5.0 SP1 and SP2 messages went to the FIX 5.0 routes. A message must be
// taken for the version its ApplVerID (1128) names, or, without one, for the
// counterparty's DefaultApplVerID, and validated with that version's
// dictionary.
func TestIssue101_FIXTMessagesOfTheirApplVerID(t *testing.T) {
	h := &i101Handler{recorder: newRecorder()}
	_, _, l := startAcceptor(t, func(c *session.Config) {
		c.ID = session.SessionID{BeginString: session.FIXT11, SenderCompID: "ACC", TargetCompID: "INI"}
		c.Handler = h
		c.Dictionaries = session.Dictionaries{
			Session: dict(t, "FIXT11"),
			Apps: map[string]*dictionary.Dict{
				"8": dictXML(t, i101App("1", "")),
				"9": dictXML(t, i101App("2", `<field name="OrderOrigination" required="N"/>`)),
			},
			DefaultApplVerID: "9",
		}
	})
	p := dialPeer(t, l)
	p.beginString = session.FIXT11
	p.send("A", 0, "98", "0", "108", "30", "1137", "8")
	p.expect("A")

	order := []string{"11", "1", "55", "EUR/USD", "54", "1", "60", "20240101-00:00:00.000", "40", "1"}
	sp2 := append(slices.Clone(order), "1724", "1") // OrderOrigination, SP2 only
	p.send("D", 0, order...)
	p.send("D", 0, append([]string{"1128", "9"}, sp2...)...)
	p.send("D", 0, append([]string{"1128", "8"}, order...)...)
	h.wait(t, "orders", func() bool { return len(h.apps) == 3 })
	h.mu.Lock()
	got := h.versions
	h.mu.Unlock()
	if want := []string{"8", "9", "8"}; !slices.Equal(got, want) {
		t.Fatalf("orders taken for ApplVerIDs %q, want %q", got, want)
	}

	// Without ApplVerID, the counterparty's default: SP1, which has no
	// OrderOrigination.
	p.send("D", 0, sp2...)
	p.expect("3", "45", "5", "371", "1724", "372", "D")
}

// i101Handler records the ApplVerID of the application messages.
type i101Handler struct {
	*recorder
	versions []string // guarded by the recorder's mu
}

func (h *i101Handler) FromApp(ctx context.Context, s *session.Session, m *fix2.Message, in session.Inbound) error {
	h.mu.Lock()
	h.versions = append(h.versions, in.ApplVerID)
	h.mu.Unlock()
	return h.recorder.FromApp(ctx, s, m, in)
}

// i101App returns a FIX 5.0 dictionary of service pack sp whose
// NewOrderSingle has the fields of extra too.
func i101App(sp, extra string) string {
	return `<fix type="FIX" major="5" minor="0" servicepack="` + sp + `">
  <header/>
  <trailer/>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app">
      <field name="ClOrdID" required="Y"/>
      <field name="Symbol" required="Y"/>
      <field name="Side" required="Y"/>
      <field name="TransactTime" required="Y"/>
      <field name="OrdType" required="Y"/>
      ` + extra + `
    </message>
  </messages>
  <components/>
  <fields>
    <field number="11" name="ClOrdID" type="STRING"/>
    <field number="40" name="OrdType" type="CHAR"/>
    <field number="54" name="Side" type="CHAR"/>
    <field number="55" name="Symbol" type="STRING"/>
    <field number="60" name="TransactTime" type="UTCTIMESTAMP"/>
    <field number="1724" name="OrderOrigination" type="INT"/>
  </fields>
</fix>`
}
