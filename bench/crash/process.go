package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// errExited means the process ended before it was killed or stopped.
var errExited = errors.New("angelia exited on its own")

// proc is one run of `angelia serve`.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when the process has exited
	base string        // http://listen
}

// launcher starts angelia processes that share a configuration and a log.
type launcher struct {
	bin, cfg, listen, log string
	env                   []string // the whole environment of the child
}

// childEnv is the harness's environment without proxy settings (every
// address in a run is loopback) plus extra.
func childEnv(base, extra []string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// start launches `angelia serve --config cfg args...`, appending its
// output to the log.
func (l launcher) start(args ...string) (*proc, error) {
	f, err := os.OpenFile(l.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(l.bin, append([]string{"serve", "--config", l.cfg}, args...)...) //nolint:gosec // the binary under test
	cmd.Stdout, cmd.Stderr, cmd.Env = f, f, l.env
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("start %s: %w", l.bin, err)
	}
	p := &proc{cmd: cmd, done: make(chan struct{}), base: "http://" + l.listen}
	go func() {
		_ = cmd.Wait() // the status is read from ProcessState
		_ = f.Close()
		close(p.done)
	}()
	return p, nil
}

// exited reports whether the process has ended.
func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// waitReady polls /readyz until it answers 200.
func (p *proc) waitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if p.exited() {
			return fmt.Errorf("before it was ready: %w (%s)", errExited, p.cmd.ProcessState)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/readyz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil { //nolint:gosec // loopback URL of the instance under test
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("angelia not ready after %v (see angelia.log): %w", timeout, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// kill sends SIGKILL and waits for the process to end. It fails with
// errExited if the process had already ended, and if it ended for another
// reason than the signal.
func (p *proc) kill() error {
	if p.exited() {
		return fmt.Errorf("before the kill: %w (%s)", errExited, p.cmd.ProcessState)
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("SIGKILL: %w", err)
	}
	<-p.done
	ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return fmt.Errorf("not ended by SIGKILL: %w (%s)", errExited, p.cmd.ProcessState)
	}
	return nil
}

// stop sends SIGTERM and waits for the graceful drain, killing the
// process after timeout. It fails if the process does not exit 0.
func (p *proc) stop(timeout time.Duration) error {
	if p.exited() {
		return fmt.Errorf("before the stop: %w (%s)", errExited, p.cmd.ProcessState)
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
		return fmt.Errorf("angelia did not stop within %v after SIGTERM", timeout)
	}
	if !p.cmd.ProcessState.Success() {
		return fmt.Errorf("angelia stopped with %s", p.cmd.ProcessState)
	}
	return nil
}
