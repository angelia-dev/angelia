//go:build integration

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
)

// Container tests of #111 (PRD F16): the npm source against a real Verdaccio
// and the pypi source's credentials against a real pypiserver. They need a
// Docker daemon and the images, so they sit behind the integration tag:
//
//	go test -tags integration -run Integration ./cmd/angelia
//
// See docs/private-registries.md. The default tests cover the same wiring
// with httptest fakes that mimic these servers' auth.

func docker(t *testing.T, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Skipf("docker %s: %v: %s", args[0], err, out)
	}
	return strings.TrimSpace(string(out))
}

// startContainer starts image detached with its port published on loopback and returns
// the base URL; the container is removed at cleanup.
func startContainer(t *testing.T, image string, port int, dockerArgs []string, cmd ...string) string {
	t.Helper()
	full := append([]string{"run", "-d", "--rm", "-p", fmt.Sprintf("127.0.0.1::%d", port)}, dockerArgs...)
	id := docker(t, append(append(full, image), cmd...)...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	hostPort := docker(t, "port", id, fmt.Sprint(port))
	hostPort = hostPort[strings.LastIndex(hostPort, ":")+1:]
	base := "http://127.0.0.1:" + hostPort
	for range 60 {
		if resp, err := http.Get(base + "/-/ping"); err == nil {
			_ = resp.Body.Close()
			return base
		}
		if resp, err := http.Get(base + "/"); err == nil {
			_ = resp.Body.Close()
			return base
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s did not start", image)
	return ""
}

func send(t *testing.T, method, url, auth string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestIntegrationVerdaccio(t *testing.T) {
	dir := t.TempDir()
	conf := "storage: /verdaccio/storage/data\nauth:\n  htpasswd:\n    file: /verdaccio/storage/htpasswd\npackages:\n  '**':\n    access: $authenticated\n    publish: $authenticated\nlog: {type: stdout, format: pretty, level: warn}\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(conf), 0o644); err != nil { //nolint:gosec // G306: read by the container user.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: read by the container user.
		t.Fatal(err)
	}
	base := startContainer(t, "verdaccio/verdaccio:5", 4873, []string{"-v", dir + ":/verdaccio/conf:ro"})

	// Create a user and read the token Verdaccio issues (npm adduser).
	user, _ := json.Marshal(map[string]string{"name": "ci", "password": "s3cret-pass-ci", "email": "ci@example.com"})
	code, body := send(t, http.MethodPut, base+"/-/user/org.couchdb.user:ci", "", user)
	var added struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &added); err != nil || code >= 300 || added.Token == "" {
		t.Fatalf("adduser: %d %s", code, body)
	}
	t.Setenv("VERDACCIO_TEST_TOKEN", added.Token)

	// Publish @acme/widget 1.0.0.
	tgz := base64.StdEncoding.EncodeToString([]byte("not really a tarball"))
	pub := fmt.Sprintf(`{"_id":"@acme/widget","name":"@acme/widget","dist-tags":{"latest":"1.0.0"},
"versions":{"1.0.0":{"name":"@acme/widget","version":"1.0.0","dist":{}}},
"_attachments":{"@acme/widget-1.0.0.tgz":{"content_type":"application/octet-stream","data":%q,"length":20}}}`, tgz)
	if code, body := send(t, http.MethodPut, base+"/@acme%2fwidget", "Bearer "+added.Token, []byte(pub)); code >= 300 {
		t.Fatalf("publish: %d %s", code, body)
	}

	// Without a credential the registry refuses; with token_env it is read.
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: base}
	e := newRegEnv(t)
	if _, err := e.list(t, cfg, npmTarget()); err == nil || !strings.Contains(err.Error(), "401") && !strings.Contains(err.Error(), "403") {
		t.Fatalf("without a token: %v", err)
	}
	e.put(t, "verdaccio-token", added.Token, hostOf(t, base))
	cfg.Sources["npm"] = config.Source{BaseURL: base, Auth: config.SourceAuth{Type: config.AuthNPMAuthToken, TokenRef: "verdaccio-token"}}
	res, err := e.list(t, cfg, source.Target{Kind: "npm", Coordinates: map[string]string{"name": "@acme/widget"}})
	if err != nil || len(res.Versions) != 1 || res.Versions[0].Raw != "1.0.0" {
		t.Fatalf("with a token: %+v, %v", res.Versions, err)
	}
	noRegLeak2(t, added.Token, e.logs.String())
}

func noRegLeak2(t *testing.T, secret, out string) {
	t.Helper()
	if strings.Contains(out, secret) {
		t.Fatalf("the credential leaks into the logs: %s", out)
	}
}

// pypiserver serves the Simple API only, not the JSON API the pypi source
// reads, so this checks the credential (basic auth, htpasswd) through the
// source's client and expects the source to get past authentication to a 404.
func TestIntegrationPypiserver(t *testing.T) {
	dir := t.TempDir()
	ht := docker(t, "run", "--rm", "httpd:2.4-alpine", "htpasswd", "-nbB", "ci", "s3cret-pass-ci")
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(ht+"\n"), 0o644); err != nil { //nolint:gosec // G306: read by the container user.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: read by the container user.
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "packages"), 0o755); err != nil { //nolint:gosec // G301: read by the container user.
		t.Fatal(err)
	}
	base := startContainer(t, "pypiserver/pypiserver:latest", 8080, []string{"-v", dir + ":/data:ro"},
		"run", "-P", "/data/htpasswd", "-a", "list,download,update", "/data/packages")
	if code, _ := send(t, http.MethodGet, base+"/simple/", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("without credentials: %d, want 401", code)
	}
	e := newRegEnv(t)
	e.put(t, "pypiserver-password", "s3cret-pass-ci", hostOf(t, base))
	cfg := config.Default()
	cfg.Sources["pypi"] = config.Source{BaseURL: base, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: "pypiserver-password"}}
	_, err := e.list(t, cfg, source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "demo"}})
	if err == nil || strings.Contains(err.Error(), "401") {
		t.Fatalf("with credentials: err = %v, want an error that is not a 401 (the JSON API is absent)", err)
	}
	noRegLeak2(t, "s3cret-pass-ci", e.logs.String())
}
