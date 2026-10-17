package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A store call's context keeps context.Context's contract, its timer
// started only by Done.
func TestStoreCtx(t *testing.T) {
	s := &Session{cfg: Config{StoreTimeout: 30 * time.Millisecond}}

	t.Run("deadline", func(t *testing.T) {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		defer cancel()
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > 30*time.Millisecond {
			t.Fatalf("deadline %v, %v", d, ok)
		}
		if ctx.Err() != nil || ctx.Value("x") != nil {
			t.Fatal(ctx.Err())
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("Done not closed at the deadline")
		}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	})
	t.Run("deadline without Done", func(t *testing.T) {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		defer cancel()
		time.Sleep(40 * time.Millisecond)
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
		<-ctx.Done()
	})
	t.Run("canceled, Done asked before", func(t *testing.T) {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		done := ctx.Done()
		cancel()
		cancel()
		<-done
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal(ctx.Err())
		}
	})
	t.Run("canceled, Done asked after", func(t *testing.T) {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		cancel()
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal(ctx.Err())
		}
	})
	t.Run("deadline, then canceled", func(t *testing.T) {
		ctx := s.storeCtx()
		time.Sleep(40 * time.Millisecond)
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
		ctx.cancel()
		<-ctx.Done()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("%v after the deadline, then cancel", ctx.Err())
		}
	})
	t.Run("deadline unobserved, then canceled", func(t *testing.T) {
		ctx := s.storeCtx()
		time.Sleep(40 * time.Millisecond)
		ctx.cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	})
	t.Run("deadline by the timer, then canceled", func(t *testing.T) {
		ctx := s.storeCtx()
		<-ctx.Done()
		ctx.cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	})
	t.Run("derived", func(t *testing.T) {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		child, stop := context.WithCancel(ctx)
		defer stop()
		cancel()
		select {
		case <-child.Done():
		case <-time.After(time.Second):
			t.Fatal("a child not canceled")
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		for range 100 {
			ctx := s.storeCtx()
			cancel := ctx.cancel
			go cancel()
			<-ctx.Done()
		}
	})
}

func BenchmarkStoreCtx(b *testing.B) {
	s := &Session{cfg: Config{StoreTimeout: 10 * time.Second}}
	b.ReportAllocs()
	for b.Loop() {
		ctx := s.storeCtx()
		cancel := ctx.cancel
		if ctx.Err() != nil {
			b.Fatal(ctx.Err())
		}
		cancel()
	}
}
