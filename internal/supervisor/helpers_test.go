package supervisor

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type recorder struct {
	httptest.ResponseRecorder
	code int
}

func (r *recorder) WriteHeader(c int) { r.code = c; r.ResponseRecorder.WriteHeader(c) }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.ResponseRecorder.Write(b)
}

func mustReq(t *testing.T, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, path, nil)
}
