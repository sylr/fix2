package inventory

import (
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	control := Host{Name: Control, Address: "10.0.0.1", User: "root"}
	n1 := Host{Name: "n1", Address: "10.0.0.2", User: "root"}
	n2 := Host{Name: "n2", Address: "10.0.0.3", User: "ubuntu", PrivateAddress: "192.168.0.3"}
	for _, tc := range []struct {
		name  string
		hosts []Host
		err   string
	}{
		{"ok", []Host{control, n1, n2}, ""},
		{"no control", []Host{n1, n2}, `want one named "control"`},
		{"two controls", []Host{control, control, n1, n2}, "listed twice"},
		{"one node", []Host{control, n1}, "want two nodes"},
		{"no user", []Host{control, n1, {Name: "n2", Address: "10.0.0.3"}}, "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.hosts)
			if tc.err == "" && err != nil {
				t.Fatalf("Check: %v", err)
			}
			if tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Fatalf("Check: %v, want an error saying %q", err, tc.err)
			}
		})
	}
}

func TestParseSplit(t *testing.T) {
	// As a stack output or a config value comes: decoded JSON.
	raw := []any{
		map[string]any{"name": "n1", "address": "10.0.0.2", "user": "root"},
		map[string]any{"name": "control", "address": "10.0.0.1", "user": "root"},
		map[string]any{"name": "n2", "address": "10.0.0.3", "user": "root", "privateAddress": "192.168.0.3", "id": "vm-n2"},
	}
	hosts, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	control, nodes := Split(hosts)
	if control.Address != "10.0.0.1" {
		t.Errorf("control: %+v", control)
	}
	if len(nodes) != 2 || nodes[0].Name != "n1" || nodes[1].Name != "n2" {
		t.Fatalf("nodes, in the order given: %+v", nodes)
	}
	if nodes[0].Peer() != "10.0.0.2" || nodes[1].Peer() != "192.168.0.3" || nodes[1].ID != "vm-n2" {
		t.Errorf("nodes: %+v", nodes)
	}
}
