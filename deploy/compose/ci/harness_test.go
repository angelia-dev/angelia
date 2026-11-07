package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

var testSecret = "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("0123456789abcdef"), 2))

func pypiVersions(t *testing.T, base string) []string {
	t.Helper()
	resp, err := http.Get(base + "/pypi/pypi/pkg-00000/json") //nolint:noctx // httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Releases map[string]json.RawMessage `json:"releases"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var vs []string
	for v := range doc.Releases {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

func publish(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Post(base+"/_ci/publish", "", nil) //nolint:noctx // httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct{ Version string }
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Version
}

// AC 3 ("fake registry publishes a new version"): the registry starts with its
// history, and every publish adds exactly one version and names it.
func TestRegistryPublishAddsExactlyOneVersionPerCall(t *testing.T) {
	h, err := newRegistry()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	if got, want := strings.Join(pypiVersions(t, srv.URL), " "), "1.0.0 1.1.0"; got != want {
		t.Fatalf("versions at start = %q, want %q", got, want)
	}
	for i, want := range []string{"1.2.0", "1.3.0"} {
		if got := publish(t, srv.URL); got != want {
			t.Fatalf("publish %d = %q, want %q", i+1, got, want)
		}
		vs := pypiVersions(t, srv.URL)
		if len(vs) != 3+i || vs[len(vs)-1] != want {
			t.Fatalf("versions after publish %d = %v, want %d ending in %s", i+1, vs, 3+i, want)
		}
	}
}

func signedPost(t *testing.T, url, secret, id string, ts time.Time, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/hook", strings.NewReader(body)) //nolint:noctx // httptest server
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		stamp := strconv.FormatInt(ts.Unix(), 10)
		sig, err := webhook.Sign([]string{secret}, id, stamp, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(webhook.HeaderID, id)
		req.Header.Set(webhook.HeaderTimestamp, stamp)
		req.Header.Set(webhook.HeaderSignature, sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

const releaseBody = `{"specversion":"1.0","type":"dev.angelia.release.published.v1","data":{"package":"pkg-00000","version":"1.2.0"}}`

// AC 3 ("webhook receiver gets an event"): a correctly signed delivery is kept
// and listed; unsigned, wrongly signed and stale ones are refused and not kept.
func TestReceiverKeepsOnlyVerifiedDeliveries(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(newReceiver([]string{testSecret}, func() time.Time { return now }))
	defer srv.Close()

	if got := signedPost(t, srv.URL, "", "x", now, releaseBody); got != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", got)
	}
	other := "whsec_" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("fedcba9876543210"), 2))
	if got := signedPost(t, srv.URL, other, "x", now, releaseBody); got != http.StatusUnauthorized {
		t.Errorf("wrong secret = %d, want 401", got)
	}
	if got := signedPost(t, srv.URL, testSecret, "x", now.Add(-time.Hour), releaseBody); got != http.StatusUnauthorized {
		t.Errorf("stale timestamp = %d, want 401", got)
	}
	if got := signedPost(t, srv.URL, testSecret, "e1:s1", now, "not json"); got != http.StatusBadRequest {
		t.Errorf("signed non-JSON = %d, want 400", got)
	}
	if got := signedPost(t, srv.URL, testSecret, "e1:s1", now, releaseBody); got != http.StatusNoContent {
		t.Fatalf("valid = %d, want 204", got)
	}
	got, err := fetch(context.Background(), http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WebhookID != "e1:s1" || got[0].Version != "1.2.0" || got[0].Package != "pkg-00000" {
		t.Fatalf("received = %+v, want exactly the valid delivery", got)
	}
}

// AC 3 (poll with a deadline): await returns when the delivery for the version
// shows up, ignores other versions, and fails at the deadline.
func TestAwaitReturnsOnTheAwaitedVersionAndFailsAtTheDeadline(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(newReceiver([]string{testSecret}, func() time.Time { return now }))
	defer srv.Close()
	old := strings.Replace(releaseBody, "1.2.0", "1.1.0", 1)
	if got := signedPost(t, srv.URL, testSecret, "old", now, old); got != http.StatusNoContent {
		t.Fatal(got)
	}

	short, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := await(short, http.DefaultClient, srv.URL, "1.2.0", 10*time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "1 other deliveries seen") {
		t.Fatalf("await without the version: err = %v, want a deadline error naming what was seen", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		signedPost(t, srv.URL, testSecret, "new", now, releaseBody)
	}()
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	d, err := await(ctx, http.DefaultClient, srv.URL, "1.2.0", 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if d.WebhookID != "new" {
		t.Errorf("delivery = %+v, want the one for 1.2.0", d)
	}
}

func TestAwaitReportsAnUnreachableReceiver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := await(ctx, http.DefaultClient, "http://127.0.0.1:1", "1.2.0", 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "last error") {
		t.Fatalf("err = %v, want the last request error", err)
	}
}

func TestRunRejectsBadArguments(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		args, env []string
	}{
		"no command":         {nil, nil},
		"unknown command":    {[]string{"nope"}, nil},
		"await no version":   {[]string{"await"}, nil},
		"receiver no secret": {[]string{"receiver"}, nil},
		"bad flag":           {[]string{"registry", "-nope"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(ctx, tc.args, tc.env, &bytes.Buffer{}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestSplitSecretsAndLookup(t *testing.T) {
	if got := splitSecrets(" a  b\nc "); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("splitSecrets = %v", got)
	}
	if got := splitSecrets(""); len(got) != 0 {
		t.Errorf("splitSecrets(empty) = %v", got)
	}
	env := []string{"CI_WEBHOOK_SECRET_X=no", "CI_WEBHOOK_SECRET=yes"}
	if got := lookup(env, "CI_WEBHOOK_SECRET"); got != "yes" {
		t.Errorf("lookup = %q", got)
	}
}

// The version publish names is the one the registry serves, with a fixed clock
// (so no wall-clock time passes between the calls).
func TestPublishNamesTheVersionTheRegistryServes(t *testing.T) {
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := newRegistryAt(func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	for range 3 {
		got := publish(t, srv.URL)
		vs := pypiVersions(t, srv.URL)
		if vs[len(vs)-1] != got {
			t.Fatalf("publish named %s, registry serves %v", got, vs)
		}
	}
}
