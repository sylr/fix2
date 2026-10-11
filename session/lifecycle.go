package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// delivery goroutine finishes delivering what it received, then its store is
// closed. Initiators and acceptors stop the sessions they remove.
func (s *Session) Stop(ctx context.Context) error {
	return s.stop(ctx, "session stopped")
}

func (s *Session) stop(ctx context.Context, text string) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	err := s.Logout(ctx, text)
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return err
	}
	s.stopped = true
	if s.schedCancel != nil {
		s.schedCancel()
	}
	d := s.deliv
	s.receipts.notify()
	s.mu.Unlock()
	if d != nil {
		d.close()
		select {
		case <-d.done:
		case <-ctx.Done():
			s.log.LogAttrs(ctx, slog.LevelWarn, "stopping without waiting for a stuck handler callback")
		}
	}
	return errors.Join(err, s.store.Close())
}

func (s *Session) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// SetNextSenderSeqNum moves the MsgSeqNum of the next message sent forward
// to next, as agreed with the counterparty out of band. The numbers skipped
// are gap-filled if the counterparty asks for them.
func (s *Session) SetNextSenderSeqNum(next uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if next < s.st.NextSender {
		return fmt.Errorf("session %s: NextSenderSeqNum can only move forward: %d is below %d", s.id, next, s.st.NextSender)
	}
	if err := s.store.SetNextSender(context.Background(), s.st.Generation, next); err != nil {
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	s.st.NextSender = next
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "NextSenderSeqNum set by hand", slog.Uint64("next", next))
	return nil
}

// SetNextTargetSeqNum sets the MsgSeqNum expected of the next message
// received, as agreed with the counterparty out of band. Messages received
// meanwhile, not yet processed, are still delivered.
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
	s.recov = recovery{}
	clear(s.stash)
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "NextTargetSeqNum set by hand", slog.Uint64("next", next))
	return nil
}
