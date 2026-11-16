package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/angelia-dev/angelia/internal/supervisor"
)

// Options.UI mounts the admin UI at /ui/ and redirects / to it; without it
// both stay unrouted.
func TestMountUI(t *testing.T) {
	ui := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ui")) })
	for _, enabled := range []bool{true, false} {
		e := newEnv(t, func(o *Options) {
			if enabled {
				o.UI = ui
			}
		})
		mux := http.NewServeMux()
		e.srv.Role().(supervisor.HTTPRole).Mount(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui/watches", nil))
		root := httptest.NewRecorder()
		mux.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
		if enabled {
			if rec.Code != 200 || rec.Body.String() != "ui" {
				t.Errorf("enabled /ui/watches: %d %q", rec.Code, rec.Body.String())
			}
			if root.Code != http.StatusFound || root.Header().Get("Location") != "/ui/" {
				t.Errorf("enabled /: %d %q", root.Code, root.Header().Get("Location"))
			}
		} else if rec.Code != 404 || root.Code != 404 {
			t.Errorf("disabled: %d %d", rec.Code, root.Code)
		}
	}
}
