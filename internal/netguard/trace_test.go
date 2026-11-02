package netguard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/telemetry"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

// Story #118: a request is a child span of the caller's span with the method,
// host and status, and no URL, query or header (tokens travel there).
func TestRoundTripSpanHasNoURLOrHeaders(t *testing.T) {
	rec := telemetrytest.Install(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	c, _ := newTestClient(t, nil)

	ctx, parent := telemetry.Start(t.Context(), "angelia.check")
	req := get(t, srv.URL+"/hook/PATH-SECRET?token=QUERY-SECRET")
	req = req.WithContext(ctx)
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
	for _, want := range []string{"GET", "127.0.0.1", "204"} {
		if !strings.Contains(dump, want) {
			t.Errorf("span export lacks %q: %s", want, dump)
		}
	}
	for _, secret := range []string{"PATH-SECRET", "QUERY-SECRET", "HEADER-SECRET", "/hook"} {
		if strings.Contains(dump, secret) {
			t.Errorf("span export contains %q: %s", secret, dump)
		}
	}
}
