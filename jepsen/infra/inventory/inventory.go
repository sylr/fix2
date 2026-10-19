// Package inventory is what a stack providing machines hands jepsen-run:
// the hosts to set up, and how to reach them.
package inventory

import (
	"encoding/json"
	"fmt"
)

// Control is the name of the host that runs Jepsen. The others are the
// suite's nodes, in the order the test takes them: the first is the
// initiator.
const Control = "control"

// Host is one machine.
type Host struct {
	// Name is the host's name in the suite: control, n1, n2.
	Name string `json:"name"`
	// Address is where Pulumi reaches it over SSH.
	Address string `json:"address"`
	// PrivateAddress is where the other hosts reach it; Address if empty.
	PrivateAddress string `json:"privateAddress,omitempty"`
	// User is the SSH user: root, or one with passwordless sudo.
	User string `json:"user"`
	// ID identifies the machine, if its provider knows it: one made again
	// has another, and is set up again.
	ID string `json:"id,omitempty"`
}

// Peer is the address the other hosts reach h at.
func (h Host) Peer() string {
	if h.PrivateAddress != "" {
		return h.PrivateAddress
	}
	return h.Address
}

// Parse reads hosts from a stack output or config value, and checks there
// is one control host and at least two nodes.
func Parse(v any) ([]Host, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var hosts []Host
	if err := json.Unmarshal(b, &hosts); err != nil {
		return nil, fmt.Errorf("hosts: %w", err)
	}
	return hosts, Check(hosts)
}

// Check checks there is one control host and at least two nodes, each
// with a name, an address and a user, the names distinct.
func Check(hosts []Host) error {
	seen := map[string]bool{}
	controls := 0
	for _, h := range hosts {
		if h.Name == "" || h.Address == "" || h.User == "" {
			return fmt.Errorf("host %+v: name, address and user are required", h)
		}
		if seen[h.Name] {
			return fmt.Errorf("host %q: listed twice", h.Name)
		}
		seen[h.Name] = true
		if h.Name == Control {
			controls++
		}
	}
	if controls != 1 {
		return fmt.Errorf("hosts: want one named %q, have %d", Control, controls)
	}
	if len(hosts)-controls < 2 {
		return fmt.Errorf("hosts: want two nodes at least, have %d", len(hosts)-controls)
	}
	return nil
}

// Split returns the control host and the nodes, in order.
func Split(hosts []Host) (control Host, nodes []Host) {
	for _, h := range hosts {
		if h.Name == Control {
			control = h
		} else {
			nodes = append(nodes, h)
		}
	}
	return control, nodes
}
