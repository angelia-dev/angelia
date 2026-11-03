// Package goplugintest starts go-plugin plugins from a test binary: the
// test binary re-executes itself as the plugin, so tests need no separate
// build step. Production starts the angelia-plugin-crates binary instead.
//
// Three plugins are available: the crates source, a probe plugin that
// misbehaves on request (see Probe), and one that never completes the
// handshake.
package goplugintest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/bench/plugins/goplugin"
	"github.com/angelia-dev/angelia/bench/plugins/goplugin/crates"
)

const envServe = "ANGELIA_GOPLUGIN_TEST_SERVE"

// Plugins served by ServeIfChild, for CommandFor.
const (
	Crates = "crates"
	Probe  = "probe"
	Hang   = "hang" // never completes the handshake
)

// ServeIfChild serves the plugin named in the environment and returns true
// when the process was started by Command or CommandFor. Call it first in
// TestMain and return if it returns true.
func ServeIfChild() bool {
	switch os.Getenv(envServe) {
	case Crates:
		crates.Serve()
	case Probe:
		goplugin.Serve(&probe{})
	case Hang:
		time.Sleep(time.Hour) // select{} would trip the deadlock detector
	default:
		return false
	}
	return true
}

// Command returns a command that starts the current test binary as the
// crates plugin. Its environment holds only the switch.
func Command() *exec.Cmd { return CommandFor(Crates) }

// CommandFor returns a command that starts the current test binary as the
// named plugin.
func CommandFor(name string) *exec.Cmd {
	cmd := exec.Command(os.Args[0]) //nolint:gosec // G204: the test binary itself.
	cmd.Env = []string{envServe + "=" + name}
	return cmd
}

// probe is a plugin of kind "probe" whose List does what the target's
// coordinates say:
//
//   - mode=fetch: one Host.HTTP request to url, with header name=value if
//     given; returns one version whose Raw is the error code, or the HTTP
//     status. Errors from the plugin side of the RPC fail the call.
//   - mode=swallow: like fetch, but ignores any error and returns "ok".
//   - mode=late: returns "ok" at once, then sends a request to url after the
//     call has ended; mode=report returns that late request's error code.
type probe struct {
	mu   sync.Mutex
	late string // error code of the late request, once done
}

func (p *probe) Configure(map[string]string) error { return nil }
func (p *probe) Validate(goplugin.Target) string   { return "" }

func (p *probe) List(ctx context.Context, host *goplugin.Host, in *goplugin.ListRequest) (*goplugin.ListResponse, error) {
	c := in.Target.Coordinates
	req := goplugin.HTTPRequest{URL: c["url"]}
	if c["header"] != "" {
		k, v, _ := strings.Cut(c["header"], "=")
		req.Headers = map[string]string{k: v}
	}
	switch c["mode"] {
	case "fetch":
		resp, err := host.HTTP(ctx, req)
		if err != nil {
			return nil, err
		}
		return one(outcome(resp)), nil
	case "swallow":
		_, _ = host.HTTP(ctx, req)
		return one("ok"), nil
	case "late":
		go func() { //nolint:gosec,contextcheck // G118: deliberately outlives the call, to test that the host refuses it
			time.Sleep(100 * time.Millisecond)
			code := "rpc-error"
			if resp, err := host.HTTP(context.Background(), req); err == nil {
				code = outcome(resp)
			}
			p.mu.Lock()
			p.late = code
			p.mu.Unlock()
		}()
		return one("ok"), nil
	case "report":
		for range 100 {
			p.mu.Lock()
			code := p.late
			p.mu.Unlock()
			if code != "" {
				return one(code), nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return nil, errors.New("no late request yet")
	}
	return nil, errors.New("unknown mode")
}

func outcome(r *goplugin.HTTPResponse) string {
	if r.Error != nil {
		return r.Error.Code
	}
	return "status-" + strconv.Itoa(r.Status)
}

func one(raw string) *goplugin.ListResponse {
	return &goplugin.ListResponse{Versions: []goplugin.Version{{Raw: raw}}}
}
