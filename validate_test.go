package fix2

import (
	"bytes"
	"errors"
	"testing"
)

// mockValidator is a test double for the Validator interface.
type mockValidator struct {
	err error
}

func (m *mockValidator) Validate(msg *Message) error {
	return m.err
}

var errMock = errors.New("mock validation error")

func TestParseAndValidate_OK(t *testing.T) {
	data := buildMsg("FIX.4.4", "35=0\x01")
	msg, err := ParseAndValidate(data, &mockValidator{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(msg.MsgType()) != "0" {
		t.Fatalf("expected MsgType '0', got %q", msg.MsgType())
	}
}

func TestParseAndValidate_ParseError(t *testing.T) {
	_, err := ParseAndValidate([]byte("garbage"), &mockValidator{})
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseAndValidate_ValidationError(t *testing.T) {
	data := buildMsg("FIX.4.4", "35=0\x01")
	_, err := ParseAndValidate(data, &mockValidator{err: errMock})
	if !errors.Is(err, errMock) {
		t.Fatalf("expected mock error, got: %v", err)
	}
}

func TestParseFromReaderAndValidate_OK(t *testing.T) {
	data := buildMsg("FIX.4.4", "35=0\x01")
	msg, err := ParseFromReaderAndValidate(bytes.NewReader(data), &mockValidator{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(msg.MsgType()) != "0" {
		t.Fatalf("expected MsgType '0', got %q", msg.MsgType())
	}
}

func TestParseFromReaderAndValidate_ValidationError(t *testing.T) {
	data := buildMsg("FIX.4.4", "35=0\x01")
	_, err := ParseFromReaderAndValidate(bytes.NewReader(data), &mockValidator{err: errMock})
	if !errors.Is(err, errMock) {
		t.Fatalf("expected mock error, got: %v", err)
	}
}
