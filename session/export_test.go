package session

// RetentionSweeps returns how many evictions s ran, for tests.
func RetentionSweeps(s *Session) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ret.sweeps
}
