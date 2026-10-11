package fix40

import (
	"errors"
	"testing"

	fix2 "github.com/sylr/fix2"
)

type orders struct {
	UnimplementedHandler
	got []string
}

func (o *orders) OnNewOrderSingle(m *NewOrderSingle) error {
	id, _ := m.ClOrdID()
	o.got = append(o.got, id)
	return nil
}

func TestDispatch(t *testing.T) {
	h := &orders{}
	order := BuildNewOrderSingle(Version.BeginString).SetClOrdID("ORD-1").Build()
	m, err := fix2.Parse(order)
	if err != nil {
		t.Fatal(err)
	}
	if err := Dispatch(&m, h); err != nil || len(h.got) != 1 || h.got[0] != "ORD-1" {
		t.Fatalf("Dispatch = %v, handled %v", err, h.got)
	}
	report, _ := fix2.Parse(BuildExecutionReport(Version.BeginString).Build())
	if err := Dispatch(&report, h); !errors.Is(err, fix2.ErrUnsupportedMessageType) {
		t.Errorf("unhandled type: Dispatch = %v, want ErrUnsupportedMessageType", err)
	}
	unknown, _ := fix2.Parse(fix2.NewBuilder("FIX.4.0", "ZZ").Build())
	if err := Dispatch(&unknown, h); !errors.Is(err, fix2.ErrUnsupportedMessageType) {
		t.Errorf("unknown type: Dispatch = %v, want ErrUnsupportedMessageType", err)
	}
	if Version.BeginString != "FIX.4.0" || Version.ApplVerID != "2" {
		t.Errorf("Version = %+v", Version)
	}
}
