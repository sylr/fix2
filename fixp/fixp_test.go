package fixp

import "testing"

func TestSessionID(t *testing.T) {
	seen := map[SessionID]bool{}
	for range 1000 {
		id := NewSessionID()
		if !id.Valid() || seen[id] {
			t.Fatalf("%s: valid %v, seen %v", id, id.Valid(), seen[id])
		}
		seen[id] = true
		back, err := ParseSessionID(id.String())
		if err != nil || back != id {
			t.Fatalf("%s parses as %s, %v", id, back, err)
		}
	}
	// RFC 4122's example layout, and the spike's session.
	id, err := ParseSessionID("7ebd6dbd-ee34-4972-adcf-9bab3ed05ec1")
	if err != nil || !id.Valid() || id[0] != 0x7e || id[15] != 0xc1 {
		t.Fatalf("%x, %v", id, err)
	}
	if (SessionID{}).Valid() {
		t.Error("the nil UUID is valid")
	}
	v1, _ := ParseSessionID("c232ab00-9414-11ec-b3c8-9f6bdeced846")
	if v1.Valid() {
		t.Error("a version 1 UUID is valid")
	}
	ncs, _ := ParseSessionID("7ebd6dbd-ee34-4972-2dcf-9bab3ed05ec1") // variant 0xx, NCS
	if ncs.Valid() {
		t.Error("a version 4 UUID of another variant is valid")
	}
	for _, s := range []string{"", "7ebd6dbd-ee34-4972-adcf-9bab3ed05ec", "7ebd6dbdxee34-4972-adcf-9bab3ed05ec1", "7ebd6dbd-ee34-4972-adcf-9bab3ed05ecg"} {
		if _, err := ParseSessionID(s); err == nil {
			t.Errorf("%q parses", s)
		}
	}
}

func TestFlowType(t *testing.T) {
	for f, want := range map[FlowType]bool{Recoverable: true, Idempotent: true, Unsequenced: false, None: false} {
		if f.Sequenced() != want {
			t.Errorf("%s sequenced %v", f, f.Sequenced())
		}
	}
	if FlowType(9).String() != "FlowType(9)" {
		t.Error(FlowType(9).String())
	}
}

func TestCodes(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{NegotiationDuplicateID.String(), "DuplicateId"},
		{NegotiationRejectCode(4).String(), "NegotiationRejectCode(4)"},
		{EstablishmentCredentials.String(), "Credentials"},
		{EstablishmentRejectCode(6).String(), "EstablishmentRejectCode(6)"},
		{RetransmitRequestLimitExceeded.String(), "RequestLimitExceeded"},
		{TerminationReRequestInProgress.String(), "ReRequestInProgress"},
		{TerminationCode(9).String(), "TerminationCode(9)"},
		{(&NegotiationRejectedError{Code: NegotiationCredentials}).Error(), "fixp: negotiation rejected: Credentials"},
		{(&EstablishmentRejectedError{Code: EstablishmentUnnegotiated, Reason: "gone"}).Error(), "fixp: establishment rejected: Unnegotiated: gone"},
		{(&TerminatedError{Code: TerminationFinished}).Error(), "fixp: terminated by us: Finished"},
		{(&TerminatedError{Code: TerminationUnspecifiedError, Reason: "bad", ByPeer: true}).Error(), "fixp: terminated by the peer: UnspecifiedError: bad"},
	} {
		if c.got != c.want {
			t.Errorf("%q, want %q", c.got, c.want)
		}
	}
}
