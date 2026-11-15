package api

// Hooks for the tests of package api_test, which can import
// internal/apitoken (it imports this package, so the tests in package api
// cannot).

// OpInfo is what an external test needs of an operation.
type OpInfo struct {
	ID, Method, Path string
	Scope            Scope
	HasBody          bool
	Statuses         map[int]bool
}

// Operations lists the operations of the server's specification.
func Operations(s *Server) []OpInfo {
	out := make([]OpInfo, 0, len(s.spec.ops))
	for _, op := range s.spec.ops {
		out = append(out, OpInfo{ID: op.ID, Method: op.Method, Path: op.Path, Scope: op.Scope, HasBody: op.Body != nil, Statuses: op.Statuses})
	}
	return out
}

// StopStreams closes the open event streams and the shared poller without
// draining, so that a stream request ends at once.
func StopStreams(s *Server) {
	s.Drain()
	s.draining.Store(false)
}
