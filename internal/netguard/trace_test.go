package netguard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

// Story #118: a request is a child span of the caller's span with the method,
// the host class and the status, and no URL, query or header (tokens travel
// there). A host that is not in the limits table (a sink, a mirror) is
// reported as "other", like in the metrics.
func TestRoundTripSpanHasNoURLOrHeaders(t *testing.T) {
	t.Parallel()
	rec, tp := telemetrytest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	c, _ := newTestClient(t, nil)

	ctx, parent := tp.Tracer("test").Start(t.Context(), "angelia.check")
	req := get(t, srv.URL+"/hook/PATH-SECRET?token=QUERY-SECRET").WithContext(ctx)
	req.Header.Set("Authorization", "Bearer HEADER-SECRET")
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := rec.Ended()
	if len(spans) != 2 || spans[0].Name() != "angelia.http" {
		t.Fatalf("spans = %v, want the request span then the check span", spans)
	}
	if spans[0].Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("the request span is not a child of the caller's span")
	}
	dump := telemetrytest.Dump(spans)
	for _, want := range []string{"GET", "other", "204"} {
		if !strings.Contains(dump, want) {
			t.Errorf("span export lacks %q: %s", want, dump)
		}
	}
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	for _, secret := range []string{"PATH-SECRET", "QUERY-SECRET", "HEADER-SECRET", "/hook", host} {
		if strings.Contains(dump, secret) {
			t.Errorf("span export contains %q: %s", secret, dump)
		}
	}
}

// A host with an entry in the limits table is reported by name.
func TestRoundTripSpanNamesKnownHosts(t *testing.T) {
	t.Parallel()
	rec, tp := telemetrytest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.HostLimits = map[string]HostLimit{host: {Anonymous: Limit{Rate: 1000, Burst: 1000}, Authenticated: Limit{Rate: 1000, Burst: 1000}}}
	})
	ctx, parent := tp.Tracer("test").Start(t.Context(), "angelia.check")
	resp, err := c.HTTPClient().Do(get(t, srv.URL).WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	parent.End()
	if dump := telemetrytest.Dump(rec.Ended()); !strings.Contains(dump, host) {
		t.Errorf("a host in the limits table is not named: %s", dump)
	}
}
