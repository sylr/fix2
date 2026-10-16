package session

// RetentionSweeps returns how many evictions s ran, for tests.
func RetentionSweeps(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ret.sweeps
}

// HeldScanned returns how many messages kept ahead of a gap
// SequenceReset-Reset handling looked at, for tests.
func HeldScanned(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldScanned
}

// ErrStoreTimeout is errStoreTimeout, for tests.
var ErrStoreTimeout = errStoreTimeout
