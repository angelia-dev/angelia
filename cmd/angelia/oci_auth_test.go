package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
)

// 94-C: config check enforces the credential rules of oci (D44) and names
// the key of each refusal.
func TestConfigCheckOCICredentialRules(t *testing.T) {
	basic := "auth: {type: basic, username: robot, password_ref: harbor-pw}"
	harbor := "sources:\n  harbor:\n    kind: oci\n    base_url: https://harbor.example.test\n    "
	for _, c := range []struct{ name, yaml, want string }{
		{
			name: "auth without base_url",
			yaml: "sources:\n  oci:\n    " + basic + "\n",
			want: "sources.oci.auth: an oci source sends a credential only when pinned to one registry; set sources.oci.base_url",
		},
		{
			name: "instance auth without base_url",
			yaml: "sources:\n  harbor:\n    kind: oci\n    " + basic + "\n",
			want: "sources.harbor.auth: an oci source sends a credential only when pinned",
		},
		{name: "bearer", yaml: harbor + "auth: {token_ref: harbor-token}\n", want: "sources.harbor.auth.type: an oci source accepts basic only"},
		{name: "npm_auth_token", yaml: harbor + "auth: {type: npm_auth_token, token_ref: harbor-token}\n", want: "sources.harbor.auth.type: an oci source accepts basic only"},
		{name: "token_env", yaml: harbor + "token_env: HARBOR_TOKEN\n", want: "sources.harbor.token_env: not accepted for an oci source; use auth with type basic"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, errb, err := runCheck(t, "--config", writeCfg(t, c.yaml))
			if err == nil || strings.Contains(out, "configuration OK") || !strings.Contains(errb, c.want) {
				t.Fatalf("out=%q stderr=%q err=%v", out, errb, err)
			}
		})
	}
	// An unknown type gets the generic message only.
	_, errb, _ := runCheck(t, "--config", writeCfg(t, harbor+"auth: {type: digest, token_ref: harbor-token}\n"))
	if !strings.Contains(errb, "sources.harbor.auth.type: want bearer, basic or npm_auth_token") || strings.Contains(errb, "accepts basic only") {
		t.Errorf("unknown type: stderr=%q", errb)
	}
	ok := harbor + basic + "\n  oci:\n    base_url: https://registry-1.docker.io\n    " + basic + "\n"
	cfg, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, ok)})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "sources.harbor") || strings.Contains(w, "sources.oci") {
		t.Errorf("warnings = %q: oci sends a credential now", w)
	}
}

// 94-C: the start-up secret check covers oci instances, bound to the host of
// base_url (the realm host is known only from the registry's challenge).
func TestOCISecretRefs(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["harbor"] = config.Source{Kind: "oci", BaseURL: "https://harbor.example.test", Auth: config.SourceAuth{Type: config.AuthBasic, Username: "robot", PasswordRef: "harbor-pw"}}
	refs := sourceRefs(cfg)
	if len(refs) != 1 || refs[0].Key != "sources.harbor.auth.password_ref" || refs[0].Name != "harbor-pw" || refs[0].URL.Host != "harbor.example.test" {
		t.Fatalf("refs = %+v", refs)
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"harbor-pw": {"other.example.test"}}); err == nil || !strings.Contains(err.Error(), "sources.harbor.auth.password_ref") {
		t.Errorf("a secret bound to another host passed the start-up check: %v", err)
	}
	if err := checkSourceSecrets(cfg, map[string][]string{}); err == nil {
		t.Error("a missing secret passed the start-up check")
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"harbor-pw": {"harbor.example.test", "auth.example.test"}}); err != nil {
		t.Error(err)
	}
}

// ociRealm is a private registry whose token realm is a second host.
type ociRealm struct {
	mu        sync.Mutex
	realmAuth []string
	regAuth   []string
	realm     string
}

// 94-C, end to end through the process wiring (netguard client, stored
// secret, registryauth): the credential reaches the realm only when the
// secret is bound to the realm's host, and never the registry.
func TestOCIInstanceSendsTheCredentialToTheRealmOnly(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("robot:"+regCanary))
	const tok = "CANARY-registry-bearer"
	f := &ociRealm{}
	realm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.realmAuth = append(f.realmAuth, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprintf(w, `{"token":%q}`, tok)
	}))
	defer realm.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.regAuth = append(f.regAuth, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="harbor",scope="repository:team/app:pull"`, f.realm))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"name":"team/app","tags":["1.0.0"]}`))
	}))
	defer reg.Close()
	f.realm = realm.URL + "/token"

	for _, c := range []struct {
		name  string
		hosts []string
	}{
		{"bound to both hosts", []string{hostOf(t, reg.URL), hostOf(t, realm.URL)}},
		{"not bound to the realm", []string{hostOf(t, reg.URL)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f.mu.Lock()
			f.realmAuth, f.regAuth = nil, nil
			f.mu.Unlock()
			e := newRegEnv(t)
			e.put(t, "harbor-pw", regCanary, c.hosts...)
			cfg := config.Default()
			cfg.Sources["harbor"] = config.Source{Kind: "oci", BaseURL: reg.URL, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "robot", PasswordRef: "harbor-pw"}}
			cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8", "::1/128")
			cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1", "::1")
			src, err := e.registry(t, cfg).Get("harbor")
			if err != nil {
				t.Fatal(err)
			}
			res, err := src.List(t.Context(), source.Target{Kind: "oci", Coordinates: map[string]string{"image": strings.TrimPrefix(reg.URL, "http://") + "/team/app"}}, source.FetchState{})
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(c.hosts) == 2 {
				if err != nil || len(res.Versions) != 1 {
					t.Fatalf("%d versions, %v", len(res.Versions), err)
				}
				if len(f.realmAuth) != 1 || f.realmAuth[0] != want {
					t.Errorf("realm Authorization = %q", f.realmAuth)
				}
			} else {
				if !errors.Is(err, secrets.ErrHostMismatch) {
					t.Fatalf("err = %v, want ErrHostMismatch", err)
				}
				if len(f.realmAuth) != 0 {
					t.Errorf("the realm received %d requests", len(f.realmAuth))
				}
				noRegLeak(t, err.Error())
			}
			for i, a := range f.regAuth {
				if a != "" && a != "Bearer "+tok {
					t.Errorf("registry request %d carried %q", i, a)
				}
			}
			noRegLeak(t, e.logs.String()+fmt.Sprint(res.State))
		})
	}
}
