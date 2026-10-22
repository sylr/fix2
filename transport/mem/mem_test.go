package mem

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// An expired deadline fails a Read with data buffered and a Write with
// room, as it does blocked ones; a future or cleared one lets them through.
func TestDeadlineExpired(t *testing.T) {
	past, future := time.Unix(1, 0), time.Now().Add(time.Hour)
	for _, ready := range []bool{false, true} {
		a, b := Pipe()
		if ready {
			if _, err := b.Write([]byte("X")); err != nil {
				t.Fatal(err)
			}
		} else if _, err := a.Write(make([]byte, maxBuffered)); err != nil {
			t.Fatal(err)
		}
		a.SetReadDeadline(past)
		a.SetWriteDeadline(past)
		var dst [1]byte
		if n, err := a.Read(dst[:]); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("ready=%v: expired Read = %d, %v", ready, n, err)
		}
		if n, err := a.Write([]byte("X")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("ready=%v: expired Write = %d, %v", ready, n, err)
		}
		if ready {
			for _, d := range []time.Time{future, {}} {
				a.SetWriteDeadline(d)
				if _, err := a.Write([]byte("X")); err != nil {
					t.Errorf("Write with deadline %v: %v", d, err)
				}
			}
			// Written again: a test against a Read consuming the byte
			// despite the deadline fails here rather than hangs.
			b.Write([]byte("Z"))
			a.SetReadDeadline(time.Time{})
			if n, err := a.Read(dst[:]); n != 1 || err != nil || dst[0] != 'X' {
				t.Errorf("Read after clearing = %q, %v; want X", dst[:n], err)
			}
		}
		a.Close()
		b.Close()
	}
}

// A Write expiring once it has written part of its bytes reports them.
func TestDeadlinePartialWrite(t *testing.T) {
	a, b := Pipe()
	defer a.Close()
	defer b.Close()
	a.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
	n, err := a.Write(make([]byte, maxBuffered+10))
	if n != maxBuffered || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Write = %d, %v; want %d, deadline exceeded", n, err, maxBuffered)
	}
}

// Moving a blocked Read's deadline into the past wakes it.
func TestDeadlineChangeWakesBlocked(t *testing.T) {
	a, b := Pipe()
	defer a.Close()
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		var dst [1]byte
		_, err := a.Read(dst[:])
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	a.SetReadDeadline(time.Unix(1, 0))
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Read not woken")
	}
	// Closed: a read after its peer closes drains, then ends.
	b.Write([]byte("Y"))
	b.Close()
	a.SetReadDeadline(time.Time{})
	if data, err := io.ReadAll(a); string(data) != "Y" || err != nil {
		t.Fatalf("ReadAll = %q, %v", data, err)
	}
}
