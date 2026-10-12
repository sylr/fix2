package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sylr/fix2/session/store"
)

// Logout logs the session out with text, waiting for the counterparty's
// Logout until ctx is done or LogoutTimeout passes, then disconnects. It
// returns at once if the session is not connected.
func (s *Session) Logout(ctx context.Context, text string) error {
	s.mu.Lock()
	c := s.c
	if c == nil {
		s.mu.Unlock()
		return nil
	}
	switch s.phase {
	case phaseEstablished:
		s.logoutLocked(c, text)
	case phaseLogoutSent:
	default:
		s.disconnectLocked(c, &LogoutError{Text: text})
	}
	s.mu.Unlock()
	select {
	case <-c.done():
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		s.disconnectLocked(c, &LogoutError{Text: text, Err: ctx.Err()})
		s.mu.Unlock()
		return ctx.Err()
	}
}

// Stop logs the session out, if connected, and stops it for good: its
// delivery goroutine finishes delivering what it received and its last
// connection winds down, then the messages it stored are made durable (see
// store.Syncer) and its store is closed. ctx bounds the waits and the Sync.
// If it ends first, as when a handler callback is stuck, Stop returns
// without waiting: the callbacks may still run, and once they returned the
// messages are synced again, bounded by WriteTimeout, and the store is
// closed, failures then being only logged; Done tells when. Stop called
// again, or concurrently, waits for Done as well, until its ctx ends.
// Called from a callback of the delivery goroutine (OnLogon, FromApp,
// OnLogout, OnRecoveryLoss), Stop waits for that callback to return: its
// ctx must end. Initiators and acceptors stop the sessions they remove.
func (s *Session) Stop(ctx context.Context) error {
	return s.stop(ctx, "session stopped")
}

// Done returns a channel closed once the session is stopped and its
// callbacks returned: its delivery goroutine exited, its last connection
// wound down, and its store is closed. No handler callback of the session
// runs after. It is closed by the time Stop returns, unless Stop's ctx
// ended first; the errors of the store's Sync and Close made then are only
// logged.
func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) stop(ctx context.Context, text string) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	err := s.Logout(ctx, text)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		select {
		case <-s.done:
		case <-ctx.Done():
		}
		return err
	}
	s.stopped = true
	if s.schedCancel != nil {
		s.schedCancel()
	}
	if s.ret.timer != nil {
		s.ret.timer.Stop()
	}
	d, prev := s.deliv, s.prev
	s.receipts.notify()
	s.mu.Unlock()
	// Nothing uses the store, nor calls the handler, once the callbacks for
	// connections not attached are done, the last connection is finished,
	// its writer joined and its reset on disconnection made, and then the
	// delivery goroutine, which delivers the OnLogout it queued.
	idle := make(chan struct{})
	go func() {
		defer close(idle)
		s.detached.Wait()
		if prev != nil {
			<-prev.finished
		}
		if d != nil {
			d.close()
			<-d.done
		}
	}()
	select {
	case <-idle:
	case <-ctx.Done():
		s.log.LogAttrs(ctx, slog.LevelWarn, "stopping without waiting for the connection to wind down or a stuck handler callback: the store is closed once they are done")
	}
	serr := s.syncStored(ctx)
	select {
	case <-idle:
		err = errors.Join(err, serr, s.store.Close())
		close(s.done)
		return err
	default:
	}
	go func() {
		<-idle
		// What was stored since, or not synced as ctx ended, is synced
		// again.
		sctx, cancel := context.WithTimeout(context.Background(), s.cfg.WriteTimeout)
		if err := s.syncStored(sctx); err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "stopped late: making the messages sent durable failed", slog.Any("error", err))
		}
		cancel()
		if err := s.store.Close(); err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "stopped late: closing the store failed", slog.Any("error", err))
		}
		close(s.done)
	}()
	return errors.Join(err, serr)
}

// syncStored makes the messages stored durable (see store.Syncer).
func (s *Session) syncStored(ctx context.Context) error {
	s.mu.Lock()
	gen, last := s.st.Generation, s.st.NextSender-1
	s.mu.Unlock()
	if s.syncer == nil || last == 0 {
		return nil
	}
	err := s.syncer.Sync(ctx, gen, last)
	s.mu.Lock()
	moved := s.st.Generation != gen
	s.mu.Unlock()
	switch {
	case moved && errors.Is(err, store.ErrGenerationGone):
		// A reset, not waited for, dropped those messages meanwhile.
		return nil
	case err != nil:
		return fmt.Errorf("session %s: making the messages sent durable: %w", s.id, err)
	}
	return nil
}

// startDetached reports whether a handler callback may run for a connection
// not yet attached, as an acceptor's ToAdmin answering its Logon: not once
// the session stops. If so, the caller calls s.detached.Done once the
// callback returned, which Stop waits for.
func (s *Session) startDetached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.stopping {
		return false
	}
	s.detached.Add(1)
	return true
}

// stoppingSessions holds the Done of the sessions an Initiator or an
// Acceptor removed: until it is closed, a removed session may still use
// its store, so its ID cannot be added again.
type stoppingSessions map[SessionID]<-chan struct{}

// check fails if session id was removed and is still stopping.
func (r stoppingSessions) check(id SessionID) error {
	done, ok := r[id]
	if !ok {
		return nil
	}
	select {
	case <-done:
		delete(r, id)
		return nil
	default:
		return fmt.Errorf("session %s: still stopping, a callback of the session removed has not returned", id)
	}
}

// isStopped reports whether the session is stopped, or stopping.
func (s *Session) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped || s.stopping
}

// SetNextSenderSeqNum moves the MsgSeqNum of the next message sent forward
// to next, as agreed with the counterparty out of band. The numbers skipped
// are gap-filled if the counterparty asks for them. A store failure leaving
// its NextSender uncertain halts the session (see Err).
func (s *Session) SetNextSenderSeqNum(next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next < s.st.NextSender {
		return fmt.Errorf("session %s: NextSenderSeqNum can only move forward: %d is below %d", s.id, next, s.st.NextSender)
	}
	if err := s.store.SetNextSender(context.Background(), s.st.Generation, next); err != nil {
		if uncertain(err) {
			// The store's NextSender may be ahead: the numbers below it may
			// be taken.
			s.haltLocked(fmt.Errorf("setting NextSenderSeqNum to %d: %w", next, err))
		}
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	s.st.NextSender = next
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "NextSenderSeqNum set by hand", slog.Uint64("next", next))
	return nil
}

// SetNextTargetSeqNum sets the MsgSeqNum expected of the next message
// received, as agreed with the counterparty out of band. Messages received
// meanwhile, not yet processed, are still delivered; those held ahead of a
// gap are dropped, their numbers belonging to the sequence replaced.
func (s *Session) SetNextTargetSeqNum(next uint64) error {
	if next == 0 {
		return fmt.Errorf("session %s: NextTargetSeqNum must be at least 1", s.id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.SetNextTarget(context.Background(), s.st.Generation, next); err != nil {
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	s.st.NextTarget = next
	s.committed = next - 1
	s.ret.ackFrom = 0 // the counterparty's MsgSeqNum may go back
	s.recov = recovery{}
	clear(s.stash)
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "NextTargetSeqNum set by hand", slog.Uint64("next", next))
	return nil
}
