//go:build unix

package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/sink"
)

// The tests run the test binary itself as the command: argv[1] is
// helperArg and argv[2] the helper mode. No shell and no script file.
const helperArg = "angelia-exec-helper"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(helper(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

func helper(mode string, args []string) int {
	switch mode {
	case "args": // args <file> <arg>...: record os.Args
		b, _ := json.Marshal(os.Args)
		record(args[0], b)
	case "stdin": // stdin <file>: record stdin
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(os.Stdin)
		record(args[0], buf.Bytes())
	case "env": // env <file>: record the environment
		record(args[0], []byte(strings.Join(os.Environ(), "\n")))
	case "exit": // exit <code>
		n, _ := strconv.Atoi(args[0])
		return n
	case "markers": // write markers to stdout and stderr, exit 2
		_, _ = os.Stdout.WriteString("STDOUT-MARKER")
		_, _ = os.Stderr.WriteString(strings.Repeat("x", 4096) + "STDERR-MARKER")
		return 2
	case "flood": // 10 MiB on each stream
		chunk := bytes.Repeat([]byte("y"), 1<<20)
		for range 10 {
			_, _ = os.Stdout.Write(chunk)
			_, _ = os.Stderr.Write(chunk)
		}
	case "sleep-ignore-term": // ignore SIGTERM, sleep
		signal.Ignore(syscall.SIGTERM)
		d, _ := time.ParseDuration(args[0])
		time.Sleep(d)
	case "spawn-exit": // spawn-exit <pidfile> <hold>: start a grandchild, exit 0 at once
		self, _ := os.Executable()
		c := osexec.Command(self, helperArg, "sleep-ignore-term", "60s") //nolint:gosec // G204: the test binary itself
		if args[1] == "hold" {
			c.Stdout = os.Stdout
		}
		if err := c.Start(); err != nil {
			return 9
		}
		record(args[0], []byte(strconv.Itoa(c.Process.Pid)))
	case "grandchild": // grandchild <pidfile>: a grandchild holding stdout, then sleep
		self, _ := os.Executable()
		c := osexec.Command(self, helperArg, "sleep-ignore-term", "60s") //nolint:gosec // G204: the test binary itself
		c.Stdout = os.Stdout
		if err := c.Start(); err != nil {
			return 9
		}
		record(args[0], []byte(strconv.Itoa(c.Process.Pid)))
		time.Sleep(60 * time.Second)
	case "record": // record <dir>: write start and end times
		start := time.Now().UnixNano()
		time.Sleep(300 * time.Millisecond)
		end := time.Now().UnixNano()
		record(filepath.Join(args[0], strconv.Itoa(os.Getpid())), fmt.Appendf(nil, "%d %d", start, end))
	}
	return 0
}

// record writes a helper's result to a path the test chose under
// t.TempDir().
func record(path string, b []byte) {
	_ = os.WriteFile(path, b, 0o600) //nolint:gosec // G703: a path under the test's own t.TempDir()
}

// readFile reads a helper's result.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: a path under the test's own t.TempDir()
}

func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func newSink(t *testing.T, c Command, opts ...func(*Options)) *Sink {
	t.Helper()
	o := Options{Commands: map[string]Command{"hook": c}}
	for _, f := range opts {
		f(&o)
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func helperCmd(t *testing.T, mode string, args ...string) Command {
	return Command{Argv: append([]string{self(t), helperArg, mode}, args...)}
}

func request(body string) sink.Request {
	return sink.Request{
		Config: sink.Config(`{"command":"hook"}`),
		Event: sink.ReleaseEvent{Envelope: event.Envelope{
			ID: "01JEVENT", Type: "dev.angelia.release.published",
		}},
		Rendered:       sink.Rendered{Body: []byte(body), ContentType: sink.CloudEventsContentType},
		IdempotencyKey: "01JEVENT:01JSUB",
		DeliveryID:     "01JDELIVERY", SubscriptionID: "01JSUB", SinkID: "01JSINK",
		Attempt: 3,
	}
}

func TestKindAndValidate(t *testing.T) {
	s := newSink(t, Command{Argv: []string{"/bin/true"}})
	if s.Kind() != "exec" {
		t.Fatal(s.Kind())
	}
	if err := s.Validate(sink.Config(`{"command":"hook"}`)); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []string{
		`{"command":"hook","argv":["/bin/sh"]}`, // unknown key
		`{}`,                                    // missing command
		`{"command":"other"}`,                   // unknown name
		`{"command":"hook"} {}`,                 // trailing data
		`[]`,
	} {
		if err := s.Validate(sink.Config(cfg)); err == nil {
			t.Errorf("%s: accepted", cfg)
		}
	}
	err := s.Validate(sink.Config(`{"command":"other"}`))
	if !strings.Contains(err.Error(), "exec.commands") {
		t.Errorf("error does not point to exec.commands: %v", err)
	}
}

func TestNewRefuses(t *testing.T) {
	for name, cmds := range map[string]map[string]Command{
		"none":         nil,
		"empty argv":   {"a": {}},
		"relative":     {"a": {Argv: []string{"true"}}},
		"long timeout": {"a": {Argv: []string{"/bin/true"}, Timeout: 13 * time.Second}},
		"concurrency":  {"a": {Argv: []string{"/bin/true"}, MaxConcurrent: 9}},
		"too many":     {"a": {Argv: append([]string{"/bin/true"}, make([]string, MaxArgs)...)}},
	} {
		if _, err := New(Options{Commands: cmds}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestArgvReachesChildUnchanged(t *testing.T) {
	out := filepath.Join(t.TempDir(), "args")
	tricky := []string{"a;b", "$(id)", "with spaces", "`x`", "{{.Version}}", "--exec=x"}
	s := newSink(t, helperCmd(t, "args", append([]string{out}, tricky...)...))
	if _, err := s.Deliver(t.Context(), request("{}")); err != nil {
		t.Fatal(err)
	}
	var got []string
	b, _ := readFile(out)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got[4:], tricky) {
		t.Fatalf("args %q, want %q", got[4:], tricky)
	}
}

func TestStdinIsBody(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin")
	body := `{"specversion":"1.0","id":"01JEVENT"}` + "\n\x00tail"
	s := newSink(t, helperCmd(t, "stdin", out))
	if _, err := s.Deliver(t.Context(), request(body)); err != nil {
		t.Fatal(err)
	}
	if b, _ := readFile(out); string(b) != body {
		t.Fatalf("stdin %q", b)
	}
}

func TestChildNotReadingLargeStdin(t *testing.T) {
	s := newSink(t, helperCmd(t, "exit", "0"))
	start := time.Now()
	if _, err := s.Deliver(t.Context(), request(strings.Repeat("z", 1<<20))); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestEnvironmentIsClean(t *testing.T) {
	t.Setenv("ANGELIA_SECRETS_KEY", "top-secret-key")
	t.Setenv("UNLISTED_SECRET", "unlisted")
	t.Setenv("DEPLOY_TOKEN", "deploy-value")
	out := filepath.Join(t.TempDir(), "env")
	c := helperCmd(t, "env", out)
	c.Env = []string{"DEPLOY_TOKEN", "NOT_SET_ANYWHERE", "ANGELIA_SECRETS_KEY"}
	s := newSink(t, c)
	if _, err := s.Deliver(t.Context(), request("{}")); err != nil {
		t.Fatal(err)
	}
	b, _ := readFile(out)
	got := strings.Split(string(b), "\n")
	slices.Sort(got)
	want := []string{
		"ANGELIA_ATTEMPT=3",
		"ANGELIA_CONTENT_TYPE=" + sink.CloudEventsContentType,
		"ANGELIA_DELIVERY_ID=01JDELIVERY",
		"ANGELIA_EVENT_ID=01JEVENT",
		"ANGELIA_EVENT_TYPE=dev.angelia.release.published",
		"ANGELIA_IDEMPOTENCY_KEY=01JEVENT:01JSUB",
		"DEPLOY_TOKEN=deploy-value",
		"PATH=" + Path,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("environment\n%q\nwant\n%q", got, want)
	}
}

func TestEnvLookupAndPath(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env")
	c := helperCmd(t, "env", out)
	c.Env = []string{"PATH", "X"}
	env := map[string]string{"PATH": "/opt/bin", "X": "1"}
	s := newSink(t, c, func(o *Options) {
		o.LookupEnv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	})
	if _, err := s.Deliver(t.Context(), request("{}")); err != nil {
		t.Fatal(err)
	}
	b, _ := readFile(out)
	if !strings.Contains(string(b), "PATH=/opt/bin\n") || !strings.Contains(string(b), "X=1\n") || strings.Count(string(b), "PATH=") != 1 {
		t.Fatalf("environment %q", b)
	}
}

func TestOutcomes(t *testing.T) {
	res, err := newSink(t, helperCmd(t, "exit", "0")).Deliver(t.Context(), request("{}"))
	if err != nil || res.StatusCode != 0 {
		t.Fatalf("exit 0: %v %v", res, err)
	}
	_, err = newSink(t, helperCmd(t, "exit", "3")).Deliver(t.Context(), request("{}"))
	if err == nil || err.Error() != "exec hook: exit status 3" || sink.IsPermanent(err) {
		t.Fatalf("exit 3: %v (permanent %v)", err, sink.IsPermanent(err))
	}
	_, err = newSink(t, Command{Argv: []string{"/nonexistent/angelia-hook"}}).Deliver(t.Context(), request("{}"))
	if !sink.IsPermanent(err) || err.Error() != "exec hook: could not start: no such file or directory" {
		t.Fatalf("missing: %v", err)
	}
	// No x bit: execve refuses it, for root too.
	f := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = newSink(t, Command{Argv: []string{f}}).Deliver(t.Context(), request("{}"))
	if !sink.IsPermanent(err) || err.Error() != "exec hook: could not start: permission denied" {
		t.Fatalf("not executable: %v", err)
	}
	// A command this node does not define is retried; a malformed
	// configuration is permanent.
	_, err = newSink(t, helperCmd(t, "exit", "0")).Deliver(t.Context(), sink.Request{Config: sink.Config(`{"command":"nope"}`)})
	if err == nil || sink.IsPermanent(err) || !strings.Contains(err.Error(), `command "nope" is not defined`) {
		t.Fatalf("unknown command: %v", err)
	}
	for _, cfg := range []string{`{"command":"hook","x":1}`, `{}`, `nope`} {
		_, err = newSink(t, helperCmd(t, "exit", "0")).Deliver(t.Context(), sink.Request{Config: sink.Config(cfg)})
		if !sink.IsPermanent(err) {
			t.Fatalf("%s: %v", cfg, err)
		}
	}
}

func TestKilledBySignal(t *testing.T) {
	// A non-zero signal exit is reported by number. A helper that kills
	// itself with SIGKILL is not portable, so drive exitError directly.
	c := osexec.Command(self(t), helperArg, "sleep-ignore-term", "30s") //nolint:gosec // G204: the test binary itself
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	_ = c.Process.Kill()
	err := exitError("hook", c.Wait())
	if err.Error() != "exec hook: killed by signal 9" {
		t.Fatal(err)
	}
	if exitError("hook", errors.New("x")).Error() != "exec hook: failed" {
		t.Fatal("generic")
	}
	if startError("hook", errors.New("x")).Error() != "exec hook: could not start" {
		t.Fatal("generic start")
	}
	if e := startError("hook", syscall.EAGAIN); sink.IsPermanent(e) {
		t.Fatalf("EAGAIN permanent: %v", e)
	}
}

func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		b, err := readFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// pid (comm) S ...: a zombie is dead.
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && len(b) > i+2 && b[i+2] == 'Z' {
			return false
		}
	}
	return true
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTimeoutKillsIgnoringChild(t *testing.T) {
	c := helperCmd(t, "sleep-ignore-term", "60s")
	c.Timeout = time.Second
	s := newSink(t, c)
	start := time.Now()
	_, err := s.Deliver(t.Context(), request("{}"))
	took := time.Since(start)
	if err == nil || sink.IsPermanent(err) || err.Error() != "exec hook: timed out after 1s" {
		t.Fatalf("error %v", err)
	}
	if took > time.Second+KillGrace+500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
}

func TestTimeoutKillsGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	c := helperCmd(t, "grandchild", pidFile)
	c.Timeout = time.Second
	s := newSink(t, c)
	start := time.Now()
	_, err := s.Deliver(t.Context(), request("{}"))
	if err == nil || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error %v", err)
	}
	if took := time.Since(start); took > time.Second+KillGrace+500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	b, err := readFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	waitDead(t, pid)
}

func TestContextCancelKillsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	s := newSink(t, helperCmd(t, "grandchild", pidFile))
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	_, err := s.Deliver(ctx, request("{}"))
	if err == nil || sink.IsPermanent(err) || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("error %v", err)
	}
	if took := time.Since(start); took > 500*time.Millisecond+KillGrace+500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	b, _ := readFile(pidFile)
	pid, _ := strconv.Atoi(string(b))
	if pid > 0 {
		waitDead(t, pid)
	}
}

func TestEffectiveTimeout(t *testing.T) {
	// Less than MinTimeout left after KillGrace: not started.
	out := filepath.Join(t.TempDir(), "args")
	s := newSink(t, helperCmd(t, "args", out))
	ctx, cancel := context.WithTimeout(t.Context(), KillGrace+500*time.Millisecond)
	defer cancel()
	_, err := s.Deliver(ctx, request("{}"))
	if err == nil || sink.IsPermanent(err) || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("error %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("the command ran")
	}

	// The deadline minus KillGrace wins over a longer timeout.
	s = newSink(t, helperCmd(t, "sleep-ignore-term", "60s"))
	ctx, cancel = context.WithTimeout(t.Context(), KillGrace+1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = s.Deliver(ctx, request("{}"))
	if err == nil || !strings.Contains(err.Error(), "timed out after 1.") {
		t.Fatalf("error %v", err)
	}
	if took := time.Since(start); took > 1500*time.Millisecond+KillGrace+500*time.Millisecond {
		t.Fatalf("took %v", took)
	}
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestOutputNeverInError(t *testing.T) {
	var logs logBuf
	s := newSink(t, helperCmd(t, "markers"), func(o *Options) {
		o.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})
	_, err := s.Deliver(t.Context(), request("{}"))
	if err == nil || err.Error() != "exec hook: exit status 2" {
		t.Fatalf("error %v", err)
	}
	out := logs.String()
	if strings.Contains(out, "STDOUT-MARKER") || !strings.Contains(out, "STDERR-MARKER") || !strings.Contains(out, `"level":"WARN"`) {
		t.Fatalf("log %s", out)
	}
	var rec struct{ Stderr string }
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Stderr) > MaxLoggedStderr || !strings.Contains(rec.Stderr, "STDERR-MARKER") {
		t.Fatalf("logged stderr of %d bytes: %q", len(rec.Stderr), rec.Stderr)
	}
}

func TestLargeOutput(t *testing.T) {
	s := newSink(t, helperCmd(t, "flood"))
	if _, err := s.Deliver(t.Context(), request("{}")); err != nil {
		t.Fatal(err)
	}
}

func TestTailIsCapped(t *testing.T) {
	tl := newTail(MaxOutput)
	chunk := bytes.Repeat([]byte("a"), 32<<10)
	for i := range 320 { // 10 MiB
		chunk[len(chunk)-1] = byte('0' + i%10)
		if n, err := tl.Write(chunk); n != len(chunk) || err != nil {
			t.Fatal(n, err)
		}
		if len(tl.Bytes()) > MaxOutput || cap(tl.Bytes()) > MaxOutput {
			t.Fatalf("holds %d (cap %d)", len(tl.Bytes()), cap(tl.Bytes()))
		}
	}
	if tl.n != 10<<20 || len(tl.Bytes()) != MaxOutput || tl.Bytes()[MaxOutput-1] != '9' {
		t.Fatalf("n %d len %d", tl.n, len(tl.Bytes()))
	}
	_, _ = tl.Write(bytes.Repeat([]byte("b"), MaxOutput+10))
	if !bytes.Equal(tl.Bytes(), bytes.Repeat([]byte("b"), MaxOutput)) {
		t.Fatal("big write")
	}
	if got := lastBytes([]byte("abc"), 2); string(got) != "bc" {
		t.Fatal(string(got))
	}
}

func TestMaxConcurrentSerializes(t *testing.T) {
	dir := t.TempDir()
	s := newSink(t, helperCmd(t, "record", dir))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := s.Deliver(t.Context(), request("{}"))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("%d runs", len(entries))
	}
	var spans [][2]int64
	for _, e := range entries {
		b, _ := readFile(filepath.Join(dir, e.Name()))
		var a, z int64
		if _, err := fmt.Sscanf(string(b), "%d %d", &a, &z); err != nil {
			t.Fatal(err)
		}
		spans = append(spans, [2]int64{a, z})
	}
	slices.SortFunc(spans, func(x, y [2]int64) int { return int(x[0] - y[0]) })
	if spans[1][0] < spans[0][1] {
		t.Fatalf("runs overlap: %v", spans)
	}
}

func TestMaxConcurrentWaitCanceled(t *testing.T) {
	c := helperCmd(t, "sleep-ignore-term", "2s")
	s := newSink(t, c)
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		_, _ = s.Deliver(t.Context(), request("{}"))
	}()
	<-started
	for deadline := time.Now().Add(5 * time.Second); len(s.cmds["hook"].sem) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the first run never took the slot")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	_, err := s.Deliver(ctx, request("{}"))
	if err == nil || sink.IsPermanent(err) || !strings.Contains(err.Error(), "free slot") {
		t.Fatalf("error %v", err)
	}
	<-done
}

func TestCheckCommand(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckCommand(self(t)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "missing"), dir, plain} {
		if err := CheckCommand(p); err == nil {
			t.Errorf("%s: accepted", p)
		}
	}
}

// The leader exits 0 and leaves a background child, detached from the
// pipes or holding stdout: the delivery succeeds at once and nothing of the
// group is left alive.
func TestExitZeroKillsLingeringGrandchild(t *testing.T) {
	for _, hold := range []string{"detached", "hold"} {
		t.Run(hold, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			s := newSink(t, helperCmd(t, "spawn-exit", pidFile, hold))
			start := time.Now()
			if _, err := s.Deliver(t.Context(), request("{}")); err != nil {
				t.Fatal(err)
			}
			if took := time.Since(start); took > KillGrace {
				t.Fatalf("took %v", took)
			}
			b, err := readFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := strconv.Atoi(string(b))
			waitDead(t, pid)
		})
	}
}

// Only a fired Cancel makes a run a timeout or a cancellation: a run that
// exited 0 on its own is delivered even if the deadline passed meanwhile.
func TestOutcome(t *testing.T) {
	if err := outcome("hook", time.Second, nil, false, errTimedOut, context.DeadlineExceeded); err != nil {
		t.Fatalf("exit 0, Cancel not fired: %v", err)
	}
	if err := outcome("hook", time.Second, osexec.ErrWaitDelay, false, nil, nil); err != nil {
		t.Fatalf("ErrWaitDelay: %v", err)
	}
	if err := outcome("hook", time.Second, nil, true, errTimedOut, nil); err == nil || err.Error() != "exec hook: timed out after 1s" {
		t.Fatalf("fired, timeout: %v", err)
	}
	if err := outcome("hook", time.Second, nil, true, context.Canceled, nil); err == nil || err.Error() != "exec hook: canceled: context canceled" {
		t.Fatalf("fired, canceled: %v", err)
	}
	if err := outcome("hook", time.Second, errors.New("x"), false, nil, nil); err == nil || err.Error() != "exec hook: failed" {
		t.Fatalf("failed: %v", err)
	}
}

// A Cancel after stopKill schedules nothing.
func TestRunStateStopped(t *testing.T) {
	var r runState
	r.stopKill()
	fired := make(chan struct{})
	r.armKill(func() { close(fired) })
	if r.kill != nil {
		t.Fatal("armed after stop")
	}
}
