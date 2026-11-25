package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/detect"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Docker Hub spellings in a watch file are stored in canonical form, which is
// data.package, and the purl is versionless until a digest is known.
func TestOCIWatchIsStoredCanonicalAndNamesThePackage(t *testing.T) {
	e := newRegEnv(t)
	cfg := config.Default()
	f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", "version: 1\nwatches:\n  - {name: node, source: oci, coordinates: {image: Index.Docker.io/node}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), "oci", map[string]string{"image": "docker.io/library/node"})
		return err
	})
	if err != nil {
		t.Fatalf("target is not stored canonically: %v", err)
	}
	pkg := event.Package("oci", tgt.Coordinates)
	if pkg != "docker.io/library/node" {
		t.Fatalf("package = %q", pkg)
	}
	p := event.PURL("oci", pkg, "22.11.0", nil, "")
	if p == nil || *p != "pkg:oci/node?repository_url=docker.io/library/node&tag=22.11.0" {
		t.Errorf("purl = %v", p)
	}
}

// A named oci instance pinned to a local registry: the first check is the
// baseline, a new tag then makes an event with data.source oci.
func TestOCIPinnedInstanceDetectsANewTag(t *testing.T) {
	var second atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tags := `"1.0.0","latest"`
		if second.Load() {
			tags += `,"1.1.0"`
		}
		_, _ = fmt.Fprintf(w, `{"name":"team/app","tags":[%s]}`, tags)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	e := newRegEnv(t)
	cfg := config.Default()
	cfg.Sources["registry-local"] = config.Source{Kind: "oci", BaseURL: srv.URL}
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	reg := e.registry(t, cfg)
	f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", fmt.Sprintf(`version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: HOOK_SECRET}}
subscriptions:
  - {name: all, sink: hook}
watches:
  - {name: app, source: registry-local, coordinates: {image: '%s/team/app'}}
`, host)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), "registry-local", map[string]string{"image": host + "/team/app"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := &detect.Checker{
		Store: e.st, Sources: reg, Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache(),
		Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
	}
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	second.Store(true)
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	evs, err := e.st.ListEventsAfter(t.Context(), 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	var p struct {
		Data struct {
			Source  string  `json:"source"`
			Package string  `json:"package"`
			PURL    *string `json:"purl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Data.Source != "oci" || p.Data.Package != host+"/team/app" {
		t.Errorf("source %q package %q", p.Data.Source, p.Data.Package)
	}
	if p.Data.PURL == nil || !strings.HasPrefix(*p.Data.PURL, "pkg:oci/app?") || !strings.HasSuffix(*p.Data.PURL, "&tag=1.1.0") {
		t.Errorf("purl = %v", p.Data.PURL)
	}
}
