package fixlatest

import (
	"errors"
	"testing"
	"time"

	fix2 "github.com/sylr/fix2"
	"github.com/sylr/fix2/dictionary"
)

// TestVersion checks the version of a package generated from FIX Latest.
func TestVersion(t *testing.T) {
	if Version.BeginString != "FIXT.1.1" || Version.ApplVerID != string(ApplVerIDFixLatest) {
		t.Errorf("Version = %+v, want FIXT.1.1 and ApplVerID %s", Version, ApplVerIDFixLatest)
	}
	if d := Dictionary(); d.Type != "FIX" || d.Major != "Latest" {
		t.Errorf("Dictionary version = %s %s, want FIX Latest", d.Type, d.Major)
	}
}

// TestNestedGroups round-trips groups nested three levels deep, as FIX
// Latest defines them apart and references them, and validates the message
// against the dictionary.
func TestNestedGroups(t *testing.T) {
	raw := BuildPartyDetailsDefinitionRequest(Version.BeginString).
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(2).
		SetSendingTime(time.Now()).
		SetPartyDetailsListRequestID("REQ-1").
		AddNoPartyUpdatesEntry(NewNoPartyUpdatesEntry().
			SetListUpdateAction(ListUpdateActionAdd).
			AddNoPartyDetailsEntry(NewNoPartyDetailsEntry().
				SetPartyDetailID("FIRM").
				AddNoPartyDetailSubIDsEntry(NewNoPartyDetailSubIDsEntry().SetPartyDetailSubID("A")).
				AddNoPartyDetailSubIDsEntry(NewNoPartyDetailSubIDsEntry().SetPartyDetailSubID("B")))).
		Build()
	msg, err := fix2.ParseAndValidate(raw, Dictionary())
	if err != nil {
		t.Fatalf("ParseAndValidate(%q): %v", raw, err)
	}
	updates := WrapPartyDetailsDefinitionRequest(&msg).NoPartyUpdatesEntries()
	if len(updates) != 1 {
		t.Fatalf("%q: %d updates, want 1", raw, len(updates))
	}
	if a, _ := updates[0].ListUpdateAction(); a != ListUpdateActionAdd {
		t.Errorf("ListUpdateAction = %q, want %q", a, ListUpdateActionAdd)
	}
	details := updates[0].NoPartyDetailsEntries()
	if len(details) != 1 {
		t.Fatalf("%q: %d party details, want 1", raw, len(details))
	}
	var subs []string
	for _, s := range details[0].NoPartyDetailSubIDsEntries() {
		id, _ := s.PartyDetailSubID()
		subs = append(subs, id)
	}
	if len(subs) != 2 || subs[0] != "A" || subs[1] != "B" {
		t.Errorf("PartyDetailSubIDs = %v, want [A B]", subs)
	}
}

// TestRequiredGroup checks that a group the repository marks required is
// required.
func TestRequiredGroup(t *testing.T) {
	raw := BuildPartyDetailsDefinitionRequest(Version.BeginString).
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(2).
		SetSendingTime(time.Now()).
		SetPartyDetailsListRequestID("REQ-1").
		Build()
	_, err := fix2.ParseAndValidate(raw, Dictionary())
	var verr *dictionary.ValidationError
	if !errors.As(err, &verr) || verr.Tag != TagNoPartyUpdates {
		t.Errorf("ParseAndValidate without NoPartyUpdates = %v, want tag %d missing", err, TagNoPartyUpdates)
	}
}

type logons struct {
	UnimplementedHandler
	got []DefaultApplVerID
}

func (l *logons) OnLogon(m *Logon) error {
	v, _ := m.DefaultApplVerID()
	l.got = append(l.got, v)
	return nil
}

// TestLogon checks that the session messages FIX Latest defines with the
// application ones validate and dispatch.
func TestLogon(t *testing.T) {
	raw := BuildLogon(Version.BeginString).
		SetSenderCompID("S").
		SetTargetCompID("T").
		SetMsgSeqNum(1).
		SetSendingTime(time.Now()).
		SetEncryptMethod(EncryptMethodNone).
		SetHeartBtInt(30).
		SetDefaultApplVerID(DefaultApplVerIDFixLatest).
		Build()
	msg, err := fix2.ParseAndValidate(raw, Dictionary())
	if err != nil {
		t.Fatalf("ParseAndValidate(%q): %v", raw, err)
	}
	h := &logons{}
	if err := Dispatch(&msg, h); err != nil || len(h.got) != 1 || h.got[0] != DefaultApplVerIDFixLatest {
		t.Errorf("Dispatch = %v, handled %v", err, h.got)
	}
}
