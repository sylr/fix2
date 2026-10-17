package session

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sylr/fix2/sofh"
)

type encoderFunc func([]byte) ([]byte, error)

func (f encoderFunc) AppendTo(dst []byte) ([]byte, error) { return f(dst) }

// A frame is its own, and an encoder failing, returning anything, leaves
// the pooled buffers usable.
func TestFrame(t *testing.T) {
	for range 100 {
		if _, err := frame(encoderFunc(func([]byte) ([]byte, error) { return nil, errors.New("no") })); err == nil {
			t.Fatal("no error")
		}
	}
	a, err := frame(appMsg{1, "first"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := frame(appMsg{1, "second"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(a, []byte("first")) || len(a) != cap(a) || len(a) != sofh.HeaderLen+8+len("first") {
		t.Fatalf("% x, cap %d", a, cap(a))
	}
	b[len(b)-1] = 'X'
	if !bytes.HasSuffix(a, []byte("first")) {
		t.Fatal("frames share their bytes")
	}
	big, err := frame(appMsg{1, string(make([]byte, 2*maxScratch))})
	if err != nil || len(big) != sofh.HeaderLen+8+2*maxScratch {
		t.Fatalf("%d bytes, %v", len(big), err)
	}
}
