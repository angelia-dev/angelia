package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// wantScopes is the scope table of spec docs/api.md ("Resources" and "Auth").
// The server reads the scopes from the specification; this table checks that
// it read them right and that every operation is covered.
var wantScopes = map[string]Scope{
	"listWatches": ScopeRead, "getWatch": ScopeRead, "putWatch": ScopeWrite, "deleteWatch": ScopeWrite,
	"listWatchVersions": ScopeRead,
	"listEvents":        ScopeRead, "streamEvents": ScopeRead, "getEvent": ScopeRead,
	"listSinks": ScopeRead, "getSink": ScopeRead, "putSink": ScopeAdmin, "deleteSink": ScopeAdmin,
	"listSubscriptions": ScopeRead, "getSubscription": ScopeRead, "putSubscription": ScopeWrite, "deleteSubscription": ScopeWrite,
	"listDeliveries": ScopeRead, "getDelivery": ScopeRead, "replayDelivery": ScopeWrite, "createReplay": ScopeWrite,
	"listPlugins": ScopeRead, "getPlugin": ScopeRead,
	"listSecrets": ScopeAdmin, "getSecret": ScopeAdmin, "putSecret": ScopeAdmin, "deleteSecret": ScopeAdmin,
}

// concretePath fills the path parameters of an operation with valid values
// that name no existing resource.
func concretePath(op *operation) string {
	r := strings.NewReplacer("{name}", "does-not-exist", "{id}", "01JBX2A7C8D9E0F1G2H3J4K5M6",
		"{interface}", "source", "{kind}", "nope")
	return r.Replace(op.Path)
}

func TestScopesFromSpec(t *testing.T) {
	e := newEnv(t)
	if len(e.srv.spec.ops) != len(wantScopes) {
		t.Fatalf("the spec has %d operations, the table %d", len(e.srv.spec.ops), len(wantScopes))
	}
	for _, op := range e.srv.spec.ops {
		want, ok := wantScopes[op.ID]
		if !ok {
			t.Errorf("operation %s is not in the scope table", op.ID)
			continue
		}
		if op.Scope != want {
			t.Errorf("%s: scope %s, want %s", op.ID, op.Scope, want)
		}
	}
}

// TestAuthEveryRoute runs every operation with no token, an unknown token,
// each scope below its own (403 before anything else, so before 404), and its
// own scope (neither 401 nor 403).
func TestAuthEveryRoute(t *testing.T) {
	tokens := map[Scope]string{ScopeRead: tokRead, ScopeWrite: tokWrite, ScopeAdmin: tokAdmin}
	for _, op := range newEnv(t).srv.spec.ops {
		t.Run(op.ID, func(t *testing.T) {
			e := newEnv(t)
			e.srv.Drain() // nothing else may run: the stream would block
			e.srv.draining.Store(false)
			path := concretePath(op)
			body := any(nil)
			if op.Body != nil {
				body = map[string]any{}
			}

			r := e.do(op.Method, path, "", body)
			if r.Status != http.StatusUnauthorized || r.problemType(t) != SlugUnauthorized {
				t.Fatalf("no token: %d %s", r.Status, r.Body)
			}
			if got := r.Header.Get("WWW-Authenticate"); got != `Bearer realm="angelia"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			r = e.do(op.Method, path, "not-a-known-token", body)
			if r.Status != http.StatusUnauthorized || !strings.Contains(r.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
				t.Fatalf("unknown token: %d %v", r.Status, r.Header)
			}
			r = e.do(op.Method, path, "", body, "Authorization", "Basic dXNlcjpwYXNz")
			if r.Status != http.StatusUnauthorized {
				t.Fatalf("basic auth: %d", r.Status)
			}
			for sc := ScopeRead; sc < op.Scope; sc++ {
				r := e.do(op.Method, path, tokens[sc], body)
				if r.Status != http.StatusForbidden || r.problemType(t) != SlugForbidden {
					t.Fatalf("scope %s: %d %s", sc, r.Status, r.Body)
				}
				if got := r.json(t)["required_scope"]; got != op.Scope.String() {
					t.Errorf("required_scope = %v, want %s", got, op.Scope)
				}
			}
			for sc := op.Scope; sc <= ScopeAdmin; sc++ {
				r := e.do(op.Method, path, tokens[sc], body)
				if r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden {
					t.Fatalf("scope %s: %d %s", sc, r.Status, r.Body)
				}
				if !op.Statuses[r.Status] && r.Status != http.StatusBadRequest {
					t.Errorf("scope %s: status %d is not declared for %s: %s", sc, r.Status, op.ID, r.Body)
				}
			}
		})
	}
}

func TestDenyAll(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Auth = DenyAll{} })
	for _, tok := range []string{"", tokAdmin} {
		r := e.do("GET", "/v1/watches", tok, nil)
		if r.Status != http.StatusUnauthorized || r.problemType(t) != SlugUnauthorized {
			t.Fatalf("%q: %d %s", tok, r.Status, r.Body)
		}
	}
	if _, err := (DenyAll{}).Authenticate(context.Background(), "x"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
}

type failingAuth struct{}

func (failingAuth) Authenticate(context.Context, string) (Principal, error) {
	return Principal{}, errors.New("token store down")
}

type noScopeAuth struct{}

func (noScopeAuth) Authenticate(context.Context, string) (Principal, error) {
	return Principal{ID: "x"}, nil
}

func TestAuthenticatorFailures(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Auth = failingAuth{} })
	r := e.do("GET", "/v1/watches", tokRead, nil)
	if r.Status != http.StatusInternalServerError || r.problemType(t) != SlugInternalError {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if strings.Contains(string(r.Body), "token store down") {
		t.Error("the detail leaks the internal error")
	}
	e = newEnv(t, func(o *Options) { o.Auth = noScopeAuth{} })
	if r := e.do("GET", "/v1/watches", tokRead, nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("a principal without a scope: %d", r.Status)
	}
}

// Unknown paths need a token too, so that a 404 never answers an anonymous
// probe.
func TestUnknownRoute(t *testing.T) {
	e := newEnv(t)
	if r := e.do("GET", "/v1/nothing", "", nil); r.Status != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.Status)
	}
	if r := e.do("GET", "/v1/nothing", tokRead, nil); r.Status != http.StatusNotFound || r.problemType(t) != SlugNotFound {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if r := e.do("GET", "/v1/watches/", tokRead, nil); r.Status != http.StatusNotFound {
		t.Fatalf("trailing slash: %d", r.Status)
	}
	r := e.do("POST", "/v1/watches", tokAdmin, nil)
	if r.Status != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET" {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
}

func TestBearerParsing(t *testing.T) {
	for _, tc := range []struct {
		h    []string
		tok  string
		sent bool
	}{
		{nil, "", false},
		{[]string{"Bearer abc"}, "abc", true},
		{[]string{"bearer  abc "}, "abc", true},
		{[]string{"Bearer a b"}, "", true},
		{[]string{"Bearer"}, "", true},
		{[]string{"Token abc"}, "", true},
		{[]string{"Bearer a", "Bearer b"}, "", true},
	} {
		r, _ := http.NewRequest("GET", "/", nil)
		for _, h := range tc.h {
			r.Header.Add("Authorization", h)
		}
		tok, sent := bearerToken(r)
		if tok != tc.tok || sent != tc.sent {
			t.Errorf("%q: got %q %v", tc.h, tok, sent)
		}
	}
	for _, s := range []string{"read", "write", "admin"} {
		sc, ok := ParseScope(s)
		if !ok || sc.String() != s {
			t.Errorf("%s: %v %v", s, sc, ok)
		}
	}
	if _, ok := ParseScope("root"); ok || ScopeNone.String() != "none" {
		t.Error("unknown scope")
	}
}
