package config

import (
	"strings"
	"testing"
)

func TestSQLitePath(t *testing.T) {
	for _, tc := range []struct {
		dsn, want, errHas string
	}{
		{"sqlite:///var/lib/angelia/a.db", "/var/lib/angelia/a.db", ""},
		{"SQLite:///tmp/a.db", "/tmp/a.db", ""},
		{"sqlite://./data/a.db", "", "no host"},
		{"sqlite://localhost/tmp/a.db", "", "no host"},
		{"sqlite:data/a.db", "", "needs a file path"},
		{"postgres://h/db", "", "not a sqlite DSN"},
	} {
		got, err := Database{DSN: tc.dsn}.SQLitePath()
		if tc.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("%s: err = %v, want %q", tc.dsn, err, tc.errHas)
			}
			if err != nil && tc.errHas == "no host" && !strings.Contains(err.Error(), "sqlite:///") {
				t.Errorf("%s: message does not suggest sqlite:///: %v", tc.dsn, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.dsn, got, err, tc.want)
		}
	}
}
