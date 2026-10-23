package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/sylr/fix2/fix"
	"github.com/sylr/fix2/fix/session/store"
)

// Logout logs the session out with text, waiting for the counterparty's
// Logout until ctx is done or LogoutTimeout passes, then disconnects. It
// returns at once if the session is not connected.
func (s *Session) Logout(ctx context.Context, text string) error {
	return s.logout(ctx, logoutReq{text: text})
}

// LogoutOptions are what a Logout says, beyond the fields the session
// writes.
type LogoutOptions struct {
	// Text is the Logout's Text (58), the reason logging out; none if
	// empty.
	Text string
	// Fields, if not nil, replace on this Logout the fields Handler.ToAdmin
	// added to the connection's Logouts: written in order, the fields of a
	// repeating group as the group is. Empty, the Logout has none. Neither
	// the fields the session writes nor Text may be set, and a value may
	// hold SOH only in a DATA field after its length, as for ToAdmin.
	Fields []fix.Field
}

// LogoutWithOptions logs the session out as Logout does, its Logout saying
// what o says. A Logout already sent, as when the counterparty logged out
// first, is not sent again. It fails without logging out if o's Fields
// could not be sent.
func (s *Session) LogoutWithOptions(ctx context.Context, o LogoutOptions) error {
	r, err := s.logoutReq(o)
	if err != nil {
		return err
	}
	return s.logout(ctx, r)
}

// logoutReq is a Logout to send: its text and, if replace, the fields
// replacing those the handler added to the connection's Logouts; status is
// its SessionStatus (1409), set where the session dictionary allows it.
type logoutReq struct {
	text    string
	extra   []byte
	replace bool
	status  string
}

// logoutReq returns the Logout o asks for, its Fields encoded as ToAdmin's
// are, or why they cannot be sent.
func (s *Session) logoutReq(o LogoutOptions) (logoutReq, error) {
	r := logoutReq{text: o.Text}
	if o.Fields == nil {
		return r, nil
	}
	b := fix.NewBuilder(s.id.BeginString, "5")
	for _, f := range o.Fields {
		// Checked before Append, which panics for BodyLength and CheckSum.
		if s.logoutReserved(f.Tag) {
			return r, fmt.Errorf("session %s: Logout: %w", s.id, &fix.ReservedTagError{Tag: f.Tag})
		}
		b.Append(f.Tag, f.Value)
	}
	extra, err := b.AppendBody(nil, s.dicts.adminProfile(), s.logoutReserved)
	if err != nil {
		return r, fmt.Errorf("session %s: Logout: %w", s.id, err)
	}
	r.extra, r.replace = extra, true
	return r, nil
}

// logout logs the session out with r, as Logout does.
func (s *Session) logout(ctx context.Context, r logoutReq) error {
	s.mu.Lock()
	c := s.c
	if c == nil {
		s.mu.Unlock()
		return nil
	}
	switch s.phase {
	case phaseEstablished:
		s.logoutReqLocked(c, r)
	case phaseLogoutSent:
	default:
		s.disconnectLocked(c, &LogoutError{Text: r.text})
	}
	s.mu.Unlock()
	select {
	case <-c.done():
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		s.disconnectLocked(c, &LogoutError{Text: r.text, Err: ctx.Err()})
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
// Called, while it runs, with the ctx of one of the session's handler
// callbacks or one derived from it, whichever goroutine runs the callback
// (Authenticate, ToAdmin, SignLogon, FromAdmin, OnResend, OnLogon, FromApp,
// OnLogout, OnRecoveryLoss), Stop returns nil at once and the session stops
// in the background, as if ctx ended: it logs out, within LogoutTimeout,
// and the rest is done once the callback returned, failures being only
// logged; Done tells when. Once the callback returned, its ctx is an
// ordinary one, except while a callback of the same kind runs, as FromApp
// for a later message: Stop called with it then returns at once as well.
// A worker that needs Stop to complete before it returns uses a ctx of its
// own, not derived from a callback's, or waits for Done. Initiators and
// acceptors stop the sessions they remove.
func (s *Session) Stop(ctx context.Context) error {
	return s.stop(ctx, logoutReq{text: "session stopped"})
}

// Done returns a channel closed once the session is stopped and its
// callbacks returned: its delivery goroutine exited, its last connection
// wound down, and its store is closed. No handler callback of the session
// runs after. It is closed by the time Stop returns, unless Stop's ctx
// ended first or Stop was called from a callback, or with the ctx of a
// callback that returned while one of the same kind runs (see Stop), or a
// store write hangs (see Config.StoreTimeout), the session stopping once it
// returns; the errors of the store's Sync and Close made then are only
// logged.
func (s *Session) Done() <-chan struct{} { return s.done }

// stop stops the session as Stop does, logging out with r.
func (s *Session) stop(ctx context.Context, r logoutReq) error {
	if s.ownCallback(ctx) {
		// One stop, however many Stops are called, once a store write
		// hangs (see below).
		if s.stuck.Load() != nil && !s.stopQueued.CompareAndSwap(false, true) {
			return nil
		}
		// What stopping waits for includes the callback calling it.
		go func() {
			s.markStopping()
			s.logout(context.Background(), r)
			if idle := s.markStopped(); idle != nil {
				<-idle
				s.closeLate()
			}
		}()
		return nil
	}
	// A store write that hangs holds the lock, and may be stopping's own,
	// its Logout or its store's Close: Stop returns once it is found stuck
	// (see storeStuck), and the stop goes on once the write returns, one
	// stop however many Stops are called then.
	if err := s.stuck.Load(); err != nil {
		if s.stopQueued.CompareAndSwap(false, true) {
			go s.stopNow(context.Background(), r)
		}
		return *err
	}
	stopped := make(chan error, 1)
	go func() { stopped <- s.stopNow(ctx, r) }()
	select {
	case err := <-stopped:
		return err
	case <-s.stuckCh:
		s.stopQueued.Store(true) // the stop started goes on
		return *s.stuck.Load()
	}
}

// markStopping records that the session is stopping.
func (s *Session) markStopping() {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
}

// stopNow stops the session, as Stop does from no callback.
func (s *Session) stopNow(ctx context.Context, r logoutReq) error {
	s.markStopping()
	err := s.logout(ctx, r)
	idle := s.markStopped()
	if idle == nil {
		select {
		case <-s.done:
		case <-ctx.Done():
		}
		return err
	}
	select {
	case <-idle:
	case <-ctx.Done():
		s.log.LogAttrs(ctx, slog.LevelWarn, "stopping without waiting for the connection to wind down or a stuck handler callback: the store is closed once they are done")
	}
	serr := s.syncStored(ctx)
	select {
	case <-idle:
		err = errors.Join(err, serr, s.storeWrite(&s.closeWrites, s.store.Close))
		close(s.done)
		return err
	default:
	}
	go func() {
		<-idle
		s.closeLate()
	}()
	return errors.Join(err, serr)
}

// ownCallback reports whether ctx is, or derives from, the ctx of one of the
// session's handler callbacks while a callback of that kind runs.
func (s *Session) ownCallback(ctx context.Context) bool {
	return callbackSession(ctx) == s
}

// callbackSession returns the session whose handler callback ctx is, or
// derives from, while a callback of that kind runs; nil if none.
func callbackSession(ctx context.Context) *Session {
	if ctx == nil {
		return nil
	}
	if c, _ := ctx.Value(callKey{}).(*callCtx); c != nil && c.running.Load() > 0 {
		return c.s
	}
	return nil
}

// callKey keys a callCtx in the ctx it is.
type callKey struct{}

// callCtx is the ctx of a kind of handler callback, as FromApp: it tells
// whether one of that kind runs.
type callCtx struct {
	context.Context // the session's callback ctx
	s               *Session
	running         atomic.Int32 // calls in progress
}

func (c *callCtx) Value(key any) any {
	if key == (callKey{}) {
		return c
	}
	return c.Context.Value(key)
}

func (c *callCtx) end() { c.running.Add(-1) }

// The kinds of handler callbacks.
const (
	cbAuthenticate = iota
	cbOnLogon
	cbOnLogout
	cbToAdmin
	cbFromAdmin
	cbFromApp
	cbOnResend
	cbOnRecoveryLoss
	cbSignLogon
	numCallbacks
)

// callbacks is a session's handler as the session calls it: each kind of
// callback gets a callCtx of its own, which tells Stop it is called from
// within it, without allocating.
type callbacks struct {
	h   Handler
	ctx [numCallbacks]callCtx
}

// signingCallbacks is callbacks for a handler that is a LogonSigner.
type signingCallbacks struct {
	*callbacks
	signer LogonSigner
}

// trackCallbacks returns h as session s calls it, once s.cbCtx is set.
func trackCallbacks(s *Session, h Handler) Handler {
	cb := &callbacks{h: h}
	for i := range cb.ctx {
		cb.ctx[i].Context, cb.ctx[i].s = s.cbCtx, s
	}
	if signer, ok := h.(LogonSigner); ok {
		return &signingCallbacks{cb, signer}
	}
	return cb
}

// begin returns the ctx of a callback of kind k starting; the caller ends
// it once the callback returned.
func (cb *callbacks) begin(k int) *callCtx {
	c := &cb.ctx[k]
	c.running.Add(1)
	return c
}

func (cb *callbacks) Authenticate(_ context.Context, req *LogonRequest) error {
	c := cb.begin(cbAuthenticate)
	defer c.end()
	return cb.h.Authenticate(c, req)
}

func (cb *callbacks) OnLogon(_ context.Context, s *Session) {
	c := cb.begin(cbOnLogon)
	defer c.end()
	cb.h.OnLogon(c, s)
}

func (cb *callbacks) OnLogout(_ context.Context, s *Session, reason error) {
	c := cb.begin(cbOnLogout)
	defer c.end()
	cb.h.OnLogout(c, s, reason)
}

func (cb *callbacks) ToAdmin(_ context.Context, s *Session, msgType string, b *fix.Builder) error {
	c := cb.begin(cbToAdmin)
	defer c.end()
	return cb.h.ToAdmin(c, s, msgType, b)
}

func (cb *callbacks) FromAdmin(_ context.Context, s *Session, m *fix.Message, in Inbound) {
	c := cb.begin(cbFromAdmin)
	defer c.end()
	cb.h.FromAdmin(c, s, m, in)
}

func (cb *callbacks) FromApp(_ context.Context, s *Session, m *fix.Message, in Inbound) error {
	c := cb.begin(cbFromApp)
	defer c.end()
	return cb.h.FromApp(c, s, m, in)
}

func (cb *callbacks) OnResend(_ context.Context, s *Session, seq uint64, m *fix.Message) bool {
	c := cb.begin(cbOnResend)
	defer c.end()
	return cb.h.OnResend(c, s, seq, m)
}

func (cb *callbacks) OnRecoveryLoss(_ context.Context, s *Session, begin, end uint64, cause error) {
	c := cb.begin(cbOnRecoveryLoss)
	defer c.end()
	cb.h.OnRecoveryLoss(c, s, begin, end, cause)
}

// OnLoss tells the handler of l: by OnLoss if it is a RecoveryLossHandler,
// else by OnRecoveryLoss.
func (cb *callbacks) OnLoss(_ context.Context, s *Session, l RecoveryLoss) {
	c := cb.begin(cbOnRecoveryLoss)
	defer c.end()
	if lh, ok := cb.h.(RecoveryLossHandler); ok {
		lh.OnLoss(c, s, l)
		return
	}
	cb.h.OnRecoveryLoss(c, s, l.Begin, l.End, l.Cause)
}

func (sc *signingCallbacks) SignLogon(_ context.Context, s *Session, h LogonHeader, b *fix.Builder) error {
	c := sc.begin(cbSignLogon)
	defer c.end()
	return sc.signer.SignLogon(c, s, h, b)
}

// markStopped marks the session stopped and returns a channel closed once
// nothing uses its store nor calls its handler any more, or nil if it was
// stopped already.
func (s *Session) markStopped() <-chan struct{} {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
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
	return idle
}

// closeLate makes the messages stored durable, bounded by WriteTimeout,
// closes the store and then Done, once a stop that did not wait is idle:
// failures are only logged.
func (s *Session) closeLate() {
	// What was stored since, or not synced as the stop's ctx ended, is
	// synced again.
	sctx, cancel := context.WithTimeout(context.Background(), s.cfg.WriteTimeout)
	if err := s.syncStored(sctx); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "stopped late: making the messages sent durable failed", slog.Any("error", err))
	}
	cancel()
	if err := s.storeWrite(&s.closeWrites, s.store.Close); err != nil {
		s.log.LogAttrs(context.Background(), slog.LevelError, "stopped late: closing the store failed", slog.Any("error", err))
	}
	close(s.done)
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
	if err := s.storeLocked(func() error { return s.store.SetNextSender(context.Background(), s.st.Generation, next) }); err != nil {
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
// gap are dropped, their numbers belonging to the sequence replaced. A store
// failure telling the store moved on without the session halts it (see Err).
func (s *Session) SetNextTargetSeqNum(next uint64) error {
	if next == 0 {
		return fmt.Errorf("session %s: NextTargetSeqNum must be at least 1", s.id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.storeLocked(func() error { return s.store.SetNextTarget(context.Background(), s.st.Generation, next) }); err != nil {
		if diverged(err) {
			// The store moved on without the session. An outcome unknown
			// alone fails the call only: the next SetNextTarget settles it.
			s.haltLocked(fmt.Errorf("setting NextTargetSeqNum to %d: %w", next, err))
		}
		return fmt.Errorf("session %s: %w", s.id, err)
	}
	s.st.NextTarget = next
	s.committed = next - 1
	s.ret.ackFrom = 0 // the counterparty's MsgSeqNum may go back
	s.recov = recovery{}
	s.clearStashLocked()
	s.log.LogAttrs(context.Background(), slog.LevelWarn, "NextTargetSeqNum set by hand", slog.Uint64("next", next))
	return nil
}
