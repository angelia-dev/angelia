package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Sample is one resource measurement of the angelia process.
type Sample struct {
	At         time.Time `json:"at"`
	RSSBytes   int64     `json:"rss_bytes"`
	CPUPercent float64   `json:"cpu_percent"` // of one CPU, since the previous sample
}

// Target runs `angelia serve` and measures it.
type Target interface {
	// Start launches angelia with the config at cfgPath (as angelia sees it).
	Start(ctx context.Context, cfgPath string, args []string) error
	// HealthURL is angelia's base URL as reachable from the harness.
	HealthURL(ctx context.Context) (string, error)
	Sample(ctx context.Context) (Sample, error)
	Stop(ctx context.Context) error
	// Limits describes the CPU and memory limits angelia runs under.
	Limits() (cpu, memory string)
}

// runFunc runs a command and returns its combined output. Tests replace it.
type runFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed tools: docker, sysctl, the VCS
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// processTarget runs angelia as a child process, without limits. It reads
// /proc, so it needs Linux. This is the BENCH_CONTAINER=0 mode.
type processTarget struct {
	bin, listen, logPath string
	readFile             func(string) ([]byte, error)
	clkTck               float64 // USER_HZ; 100 on every mainstream Linux

	cmd      *exec.Cmd
	log      *os.File
	lastCPU  float64
	lastWall time.Time
}

func newProcessTarget(bin, listen, logPath string) *processTarget {
	return &processTarget{bin: bin, listen: listen, logPath: logPath, readFile: os.ReadFile, clkTck: 100}
}

func (p *processTarget) Start(_ context.Context, cfgPath string, args []string) error {
	f, err := os.Create(p.logPath)
	if err != nil {
		return err
	}
	p.log = f
	// Not CommandContext: Stop sends SIGTERM for a graceful drain first.
	p.cmd = exec.Command(p.bin, append([]string{"serve", "--config", cfgPath}, args...)...) //nolint:gosec // the binary under test
	p.cmd.Stdout, p.cmd.Stderr = f, f
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", p.bin, err)
	}
	return nil
}

func (p *processTarget) HealthURL(context.Context) (string, error) { return "http://" + p.listen, nil }

func (p *processTarget) Sample(context.Context) (Sample, error) {
	if p.cmd == nil || p.cmd.Process == nil {
		return Sample{}, errors.New("not started")
	}
	return p.sample(p.cmd.Process.Pid, time.Now())
}

func (p *processTarget) sample(pid int, now time.Time) (Sample, error) {
	status, err := p.readFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return Sample{}, fmt.Errorf("process sampling needs Linux /proc: %w", err)
	}
	s := Sample{At: now}
	for line := range strings.SplitSeq(string(status), "\n") {
		if v, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			if err != nil {
				return Sample{}, fmt.Errorf("parse VmRSS %q: %w", v, err)
			}
			s.RSSBytes = kb * 1024
		}
	}
	stat, err := p.readFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return Sample{}, err
	}
	// Fields after the ")" that ends the command name: state is field 3,
	// utime and stime are fields 14 and 15.
	_, rest, _ := strings.Cut(string(stat), ") ")
	f := strings.Fields(rest)
	if len(f) < 13 {
		return Sample{}, fmt.Errorf("short /proc/%d/stat", pid)
	}
	ut, err1 := strconv.ParseFloat(f[11], 64)
	st, err2 := strconv.ParseFloat(f[12], 64)
	if err := errors.Join(err1, err2); err != nil {
		return Sample{}, fmt.Errorf("parse /proc/%d/stat: %w", pid, err)
	}
	cpu := (ut + st) / p.clkTck
	if !p.lastWall.IsZero() {
		if wall := now.Sub(p.lastWall).Seconds(); wall > 0 {
			s.CPUPercent = (cpu - p.lastCPU) / wall * 100
		}
	}
	p.lastCPU, p.lastWall = cpu, now
	return s, nil
}

func (p *processTarget) Stop(context.Context) error {
	defer func() { _ = p.log.Close() }()
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait() // the exit status after SIGTERM is not interesting
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
	return nil
}

func (p *processTarget) Limits() (string, string) {
	cpu, mem := "none (no container)", "none (no container)"
	if b, err := p.readFile("/sys/fs/cgroup/cpu.max"); err == nil {
		cpu = "cgroup cpu.max " + strings.TrimSpace(string(b))
	}
	if b, err := p.readFile("/sys/fs/cgroup/memory.max"); err == nil {
		mem = "cgroup memory.max " + strings.TrimSpace(string(b))
	}
	return cpu, mem
}

// dockerTarget runs angelia in a container limited with --memory and
// --cpus. The binary is mounted into a static base image, since the
// repository has no Dockerfile yet. This is the default mode.
type dockerTarget struct {
	run                       runFunc
	name, image, bin, workdir string
	memory, cpus, logPath     string
}

func (d *dockerTarget) Start(ctx context.Context, cfgPath string, args []string) error {
	_, _ = d.run(ctx, "docker", "rm", "-f", d.name) // leftover of an aborted run
	a := []string{
		"run", "-d", "--name", d.name,
		"--memory", d.memory, "--memory-swap", d.memory, "--cpus", d.cpus,
		"--add-host", "host.docker.internal:host-gateway",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-v", d.bin + ":/angelia:ro", "-v", d.workdir + ":/work",
		"-p", "127.0.0.1::8080",
		d.image, "/angelia", "serve", "--config", cfgPath,
	}
	_, err := d.run(ctx, "docker", append(a, args...)...)
	return err
}

func (d *dockerTarget) HealthURL(ctx context.Context) (string, error) {
	out, err := d.run(ctx, "docker", "port", d.name, "8080/tcp")
	if err != nil {
		return "", err
	}
	addr, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return "http://" + addr, nil
}

func (d *dockerTarget) Sample(ctx context.Context) (Sample, error) {
	out, err := d.run(ctx, "docker", "stats", "--no-stream", "--format", "{{.MemUsage}}|{{.CPUPerc}}", d.name)
	if err != nil {
		return Sample{}, err
	}
	mem, cpu, ok := strings.Cut(strings.TrimSpace(string(out)), "|")
	if !ok {
		return Sample{}, fmt.Errorf("docker stats: unexpected output %q", out)
	}
	used, _, _ := strings.Cut(mem, "/")
	rss, err := parseBytes(strings.TrimSpace(used))
	if err != nil {
		return Sample{}, err
	}
	pct, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(cpu), "%"), 64)
	if err != nil {
		return Sample{}, fmt.Errorf("docker stats CPU %q: %w", cpu, err)
	}
	return Sample{At: time.Now(), RSSBytes: rss, CPUPercent: pct}, nil
}

func (d *dockerTarget) Stop(ctx context.Context) error {
	if logs, err := d.run(ctx, "docker", "logs", d.name); err == nil {
		_ = os.WriteFile(d.logPath, logs, 0o600)
	}
	_, err := d.run(ctx, "docker", "rm", "-f", d.name)
	return err
}

func (d *dockerTarget) Limits() (string, string) {
	return d.cpus + " CPU (docker --cpus)", d.memory + " (docker --memory, no swap)"
}

// parseBytes reads docker's sizes: "12.5MiB", "800KiB", "1.2GB", "512B".
func parseBytes(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   float64
	}{
		{"KiB", 1 << 10},
		{"MiB", 1 << 20},
		{"GiB", 1 << 30},
		{"kB", 1e3},
		{"KB", 1e3},
		{"MB", 1e6},
		{"GB", 1e9},
		{"B", 1},
	}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0, fmt.Errorf("size %q: %w", s, err)
			}
			return int64(f * u.mult), nil
		}
	}
	return 0, fmt.Errorf("size %q: unknown unit", s)
}
