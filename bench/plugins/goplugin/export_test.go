package goplugin

// PID returns the plugin process id.
func (s *Source) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc.ReattachConfig().Pid
}

// Exited reports whether the plugin process has exited.
func (s *Source) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pc.Exited()
}
