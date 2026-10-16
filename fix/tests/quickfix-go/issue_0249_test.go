package quickfixgo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session"
	"github.com/sylr/fix2/fix/tests/dicts/fix40"
)

// https://github.com/quickfixgo/quickfix/issues/249
//
// The message router answered every type without a route as unsupported,
// administrative messages and BusinessMessageRejects included. Neither must
// be rejected: admin messages are the session's, and a reject answered with
// a reject can loop between two engines.

// A generated Dispatch handling only NewOrderSingle: the session answers
// admin messages itself and never hands them to it.
func TestIssue249_DispatchNeverRejectsAdminMessages(t *testing.T) {
	id := session.SessionID{BeginString: session.FIX40, SenderCompID: "ACC", TargetCompID: "INI"}
	_, _, l := startAcceptor(t, func(c *session.Config) {
		c.ID = id
		c.Handler = i249Handler{}
	})
	p := dialPeer(t, l)
	p.beginString = session.FIX40
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	p.send("0", 0)
	p.send("1", 0, "112", "T1")
	p.expect("0", "112", "T1")
	p.send("3", 0, "45", "1", "58", "whatever")
	p.send("D", 0, "11", "O1", "21", "1", "55", "X", "54", "1", "38", "1", "40", "1")
	p.expectNothing(100 * time.Millisecond)
	// What it does not handle is rejected: FIX 4.0 has no
	// BusinessMessageReject.
	p.send("8", 0, "37", "E1")
	p.expect("3", "45", "6")
}

// A handler not handling BusinessMessageRejects, as a generated Dispatch
// without a route for them: one received is not answered with another.
func TestIssue249_BusinessRejectNotAnsweredWithBusinessReject(t *testing.T) {
	_, _, l := startAcceptor(t, func(c *session.Config) { c.Handler = i249Unsupported{} })
	p := dialPeer(t, l)
	p.send("A", 0, "98", "0", "108", "30")
	p.expect("A")
	p.send("D", 0, "11", "O1")
	p.expect("j", "45", "2", "372", "D", "380", "3")
	p.send("j", 0, "45", "2", "372", "8", "380", "3", "58", "Unsupported Message Type")
	p.expectNothing(100 * time.Millisecond)
}

// i249Handler hands application messages to fix40.Dispatch.
type i249Handler struct{ session.BaseHandler }

func (i249Handler) FromApp(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) error {
	return fix40.Dispatch(m, i249Orders{})
}

// i249Orders handles NewOrderSingle only.
type i249Orders struct{ fix40.UnimplementedHandler }

func (i249Orders) OnNewOrderSingle(*fix40.NewOrderSingle) error { return nil }

// i249Unsupported supports no message type.
type i249Unsupported struct{ session.BaseHandler }

func (i249Unsupported) FromApp(_ context.Context, _ *session.Session, m *fix.Message, _ session.Inbound) error {
	return fmt.Errorf("%w: %s", fix.ErrUnsupportedMessageType, m.MsgType())
}
