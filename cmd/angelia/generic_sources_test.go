package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/detect"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Generic HTTP sources (#97, ADR 0038): a watch-file watch on a loopback
// fixture records a baseline, then a new value produces one event whose
// package is the name coordinate; without http.allow_networks the guard
// blocks the fetch.

type genericCase struct {
	kind   string
	coords string // YAML flow map without the url
	first  string // body before the change
	second string // body after the change
	want   string // the new version
}

var genericCases = []genericCase{
	{
		kind:   "http-regex",
		coords: `name: tool, regex: 'tool-([0-9.]+)\.tar\.gz'`,
		first:  `<a href="tool-1.0.0.tar.gz">1.0.0</a>`,
		second: `<a href="tool-1.1.0.tar.gz">1.1.0</a> <a href="tool-1.0.0.tar.gz">1.0.0</a>`,
		want:   "1.1.0",
	},
}

func checkGeneric(t *testing.T, c genericCase, allowNetworks bool) (*regEnv, store.Target, *detect.Checker, *atomic.Bool) {
	t.Helper()
	var second atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if second.Load() {
			_, _ = w.Write([]byte(c.second))
			return
		}
		_, _ = w.Write([]byte(c.first))
	}))
	t.Cleanup(srv.Close)
	e := newRegEnv(t)
	cfg := config.Default()
	cfg.HTTP.AllowHTTPHosts = []string{"127.0.0.1"}
	if allowNetworks {
		cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	}
	file := writeFile(t, "w.yaml", `version: 1
watches:
  - name: generic
    source: `+c.kind+`
    coordinates: {`+c.coords+`, url: '`+srv.URL+`/page'}
`)
	f, err := parseWatchFile(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), c.kind, f.Watches[0].Coordinates)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := &detect.Checker{
		Store: e.st, Sources: e.registry(t, cfg), Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache(),
		Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
	}
	return e, tgt, chk, &second
}

func TestGenericSourceEndToEnd(t *testing.T) {
	for _, c := range genericCases {
		t.Run(c.kind, func(t *testing.T) {
			e, tgt, chk, second := checkGeneric(t, c, true)
			if _, err := chk.Check(t.Context(), tgt); err != nil {
				t.Fatal(err)
			}
			second.Store(true)
			if _, err := chk.Check(t.Context(), tgt); err != nil {
				t.Fatal(err)
			}
			evs, err := e.st.ListEventsAfter(t.Context(), 0, 10)
			if err != nil || len(evs) != 1 {
				t.Fatalf("events = %d, %v; want one (the baseline emits none)", len(evs), err)
			}
			var p struct {
				Type    string `json:"type"`
				Subject string `json:"subject"`
				Data    struct {
					Source  string  `json:"source"`
					Package string  `json:"package"`
					Version string  `json:"version"`
					Scheme  string  `json:"scheme"`
					PURL    *string `json:"purl"`
				} `json:"data"`
			}
			if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Type != event.TypePublished || p.Data.Source != c.kind || p.Data.Package != "tool" || p.Data.Version != c.want ||
				p.Data.PURL != nil || p.Data.Scheme != "lexical" || p.Subject != c.kind+":tool@"+c.want {
				t.Errorf("event = %+v", p)
			}
		})
	}
}

func TestGenericSourceBlockedWithoutAllowNetworks(t *testing.T) {
	for _, c := range genericCases {
		t.Run(c.kind, func(t *testing.T) {
			_, tgt, chk, _ := checkGeneric(t, c, false)
			_, err := chk.Check(t.Context(), tgt)
			if !errors.Is(err, netguard.ErrBlocked) {
				t.Fatalf("err = %v, want a netguard blocked error", err)
			}
		})
	}
}

func TestBuiltinSourcesRefuseURLKindInstances(t *testing.T) {
	for _, kind := range urlKinds {
		cfg := config.Default()
		cfg.Sources = map[string]config.Source{"mine": {Kind: kind}}
		if _, err := builtinSources(cfg, &http.Client{}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "has no named instances") {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
}
