package fix2

import "testing"

func TestGroupsBasic(t *testing.T) {
	// Build a message with two NoAllocs group entries:
	// 78=2|79=ACC1|80=500|79=ACC2|80=300|
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		SetInt(78, 2).
		AppendString(79, "ACC1").
		AppendFloat(80, 500).
		AppendString(79, "ACC2").
		AppendFloat(80, 300).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// First entry
	if got := string(groups[0].Get(79)); got != "ACC1" {
		t.Errorf("group[0] tag 79 = %q, want %q", got, "ACC1")
	}
	if got := string(groups[0].Get(80)); got != "500" {
		t.Errorf("group[0] tag 80 = %q, want %q", got, "500")
	}

	// Second entry
	if got := string(groups[1].Get(79)); got != "ACC2" {
		t.Errorf("group[1] tag 79 = %q, want %q", got, "ACC2")
	}
	if got := string(groups[1].Get(80)); got != "300" {
		t.Errorf("group[1] tag 80 = %q, want %q", got, "300")
	}
}

func TestGroupsZeroCount(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		SetInt(78, 0).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(groups))
	}
}

func TestGroupsMissingCounter(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetString(11, "ORD-001").
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if groups != nil {
		t.Fatalf("expected nil groups, got %v", groups)
	}
}

func TestGroupFields(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetInt(78, 1).
		AppendString(79, "ACC1").
		AppendFloat(80, 100).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, []int{79, 80})
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}

	fields := groups[0].Fields()
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}
}

func TestGroupGetMissing(t *testing.T) {
	g := Group{fields: []Field{{Tag: 79, Value: []byte("ACC1")}}}
	if got := g.Get(999); got != nil {
		t.Errorf("expected nil for missing tag, got %q", got)
	}
}

func TestGroupsNilChildTags(t *testing.T) {
	raw := NewBuilder("FIX.4.4", "D").
		SetInt(78, 1).
		Build()

	msg, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	groups := msg.Groups(78, nil)
	if groups != nil {
		t.Fatalf("expected nil groups, got %v", groups)
	}
}
