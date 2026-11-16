package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
)

// The new flags are checked before anything starts; the defaults (SQLite,
// one process) are what `make bench-crash` runs.
func TestFlagsDatabaseAndReplicas(t *testing.T) {
	t.Setenv(dsnVar, "")
	for _, tc := range []struct {
		name    string
		args    []string
		env     string
		wantErr string
	}{
		{"unknown database", []string{"-database", "mysql"}, "", "want sqlite or postgres"},
		{"replicas need postgres", []string{"-replicas", "2"}, "", "needs -database postgres"},
		{"no replicas", []string{"-database", "postgres", "-replicas", "0"}, "x", "at least 1"},
		{"postgres without a DSN", []string{"-database", "postgres"}, "", dsnVar},
		{"a DSN that is not a postgres URL", []string{"-database", "postgres"}, "sqlite:///x", "postgres:// URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(dsnVar, tc.env)
			var stderr bytes.Buffer
			if code := run(context.Background(), tc.args, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.wantErr) {
				t.Errorf("exit %d, stderr %q, want exit 2 and %q", code, stderr.String(), tc.wantErr)
			}
			if strings.Contains(stderr.String(), "s3cret") {
				t.Error("the DSN is in the message")
			}
		})
	}
}

func TestReplicaFile(t *testing.T) {
	for _, tc := range []struct {
		i, n int
		want string
	}{{0, 1, "angelia.log"}, {0, 2, "angelia-1.log"}, {1, 2, "angelia-2.log"}} {
		if got := replicaFile("angelia", ".log", tc.i, tc.n); got != tc.want {
			t.Errorf("replicaFile(%d of %d) = %q, want %q", tc.i, tc.n, got, tc.want)
		}
	}
}

// A replica's configuration on Postgres loads with the DSN from the
// environment, which keeps the password out of the file, and logs at debug,
// which the overlap check needs.
func TestPostgresConfiguration(t *testing.T) {
	dir := t.TempDir()
	cf := filepath.Join(dir, "angelia-1.yaml")
	if err := writePostgresConfig(cf, "127.0.0.1:8081", "http://127.0.0.1:5000", "http://127.0.0.2:4000"); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(context.Background(), config.Options{Path: cf, Environ: []string{dsnEnv + "=postgres://u:pw@localhost:5432/db?sslmode=disable"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Database.DSN, "postgres://") || c.Log.Level != "debug" || c.Listen != "127.0.0.1:8081" || len(c.Roles) != 1 || c.Roles[0] != "all" {
		t.Errorf("database %q, log level %q, listen %q, roles %v", c.Database.DSN, c.Log.Level, c.Listen, c.Roles)
	}
	// The SQLite configuration is unchanged: info level, SQLite in the data directory.
	sf := filepath.Join(dir, "angelia.yaml")
	if err := writeInstanceConfig(sf, "127.0.0.1:8080", dir, "http://127.0.0.1:5000", "http://127.0.0.2:4000"); err != nil {
		t.Fatal(err)
	}
	c, err = config.Load(context.Background(), config.Options{Path: sf, Environ: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.Database.DSN, "sqlite://") || c.Log.Level != "info" {
		t.Errorf("sqlite configuration: database %q, level %q", c.Database.DSN, c.Log.Level)
	}
}

// With replicas the report names the replica of each kill and says what ran.
func TestMarkdownWithReplicas(t *testing.T) {
	kills := busyKills(4)
	for i := range kills {
		kills[i].Replica = i%2 + 1
	}
	r := Report{Verdict: "pass", Started: time.Now(), Kills: kills, Params: Params{Kills: 4, Database: "postgres", Replicas: 2, PostgresVersion: "16.15"}}
	var b bytes.Buffer
	if err := r.Markdown(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Kills per replica | replica 1: 2, replica 2: 2", "| # | Replica | Timed from", "2 × `angelia serve`", "Postgres 16.15"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
	// A single process leaves the new rows out.
	r.Params, r.Kills = Params{Kills: 4, Database: "sqlite", Replicas: 1}, busyKills(4)
	b.Reset()
	if err := r.Markdown(&b); err != nil {
		t.Fatal(err)
	}
	for _, not := range []string{"Kills per replica", "Replica |", "Postgres"} {
		if strings.Contains(b.String(), not) {
			t.Errorf("a one-process report has %q", not)
		}
	}
}

// The registry delay is off by default (checks take milliseconds) and, when
// given, replaces the quick latency.
func TestRegistryDelay(t *testing.T) {
	b := registryConfig(workload{Packages: 3, ReleasesPerH: 1, History: 3}).Ecosystems["pypi"]
	if b.Latency != 2e6 || b.LatencyJitter != 8e6 {
		t.Errorf("default latency %v + %v", b.Latency, b.LatencyJitter)
	}
	b = registryConfig(workload{Packages: 3, ReleasesPerH: 1, History: 3, DelayMin: 500 * time.Millisecond, DelayMax: 2 * time.Second}).Ecosystems["pypi"]
	if b.Latency != 500*time.Millisecond || b.LatencyJitter != 1500*time.Millisecond {
		t.Errorf("delayed latency %v + %v", b.Latency, b.LatencyJitter)
	}
	for _, args := range [][]string{{"-registry-delay-min", "1s"}, {"-registry-delay-min", "2s", "-registry-delay-max", "1s"}} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, io.Discard, &stderr); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
