package api

import (
	"net/http"
	"strings"
	"testing"
)

// ADR 0023 section 3: the API carries the reference (secret_env), never the
// value, even when the variable is set in the server's environment.
func TestSinkSecretValueNeverReturned(t *testing.T) {
	const value = "whsec_QXBJTmV2ZXJSZXR1cm5zVGhpc1NlY3JldFZhbHVlIQ=="
	t.Setenv("CI_WEBHOOK_SECRET", value)
	e := newEnv(t)
	put := e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-webhook", tokAdmin, sinkPut))
	get := e.must(http.StatusOK, e.do("GET", "/v1/sinks/ci-webhook", tokRead, nil))
	list := e.must(http.StatusOK, e.do("GET", "/v1/sinks", tokRead, nil))
	for name, r := range map[string]result{"put": put, "get": get, "list": list} {
		if strings.Contains(string(r.Body), strings.TrimPrefix(value, "whsec_")) {
			t.Errorf("%s returns the secret value", name)
		}
		if !strings.Contains(string(r.Body), `"secret_env":"CI_WEBHOOK_SECRET"`) {
			t.Errorf("%s: the reference is not returned: %s", name, r.Body)
		}
	}
	if strings.Contains(e.log.String(), strings.TrimPrefix(value, "whsec_")) {
		t.Error("the API log holds the secret value")
	}
}

// ADR 0023 decided question 4: http:// only for http.allow_http_hosts.
func TestSinkHTTPAllowlist(t *testing.T) {
	k := clone(sinkPut)
	k["config"].(map[string]any)["url"] = "http://hooks.lan/x"
	e := newEnv(t)
	if r := e.do("PUT", "/v1/sinks/lan", tokAdmin, k); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("http:// without allowlist: %d %s", r.Status, r.Body)
	}
	e = newEnv(t, func(o *Options) { o.AllowHTTPHosts = []string{"hooks.lan"} })
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/lan", tokAdmin, k))
}
