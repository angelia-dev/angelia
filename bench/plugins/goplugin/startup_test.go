//go:build bench

// Start-up cost and resident memory of one go-plugin process, for ADR 0013:
//
//	go test -tags bench -run TestStartup -v ./bench/plugins/goplugin

package goplugin_test

import (
	"bufio"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/plugin/wasm/wasmtest"
	"github.com/angelia-dev/angelia/internal/source"
)

func TestStartup(t *testing.T) {
	up := wasmtest.NewCratesUpstream(100, map[string]int{"serde": 300})
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	const n = 20
	starts := make([]time.Duration, n)
	for i := range n {
		begin := time.Now()
		s := newCrates(t, srv.URL, srv.Client().Transport, nil, nil)
		starts[i] = time.Since(begin)
		if i == n-1 {
			for range 50 {
				if _, err := s.List(t.Context(), crate("serde"), source.FetchState{}); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("plugin RSS after 50 calls of 300 versions: %s", rss(t, s.PID()))
		}
		s.Close()
	}
	slices.Sort(starts)
	t.Logf("start (New: exec, handshake, mTLS, Configure) p50 %v, max %v over %d", starts[n/2].Round(time.Millisecond), starts[n-1].Round(time.Millisecond), n)
}

func rss(t *testing.T, pid int) string {
	t.Helper()
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Skip("no /proc:", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}
