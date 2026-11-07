package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSourceAuthParses(t *testing.T) {
	cfg, err := load(t, writeFile(t, `
sources:
  npm:
    base_url: https://nexus.example.com/repository/npm-group
    auth: {type: npm_auth_token, token_ref: nexus-npm}
  pypi:
    base_url: https://artifactory.example.com/artifactory/api/pypi/pypi-remote
    auth: {type: basic, username: ci-bot, password_ref: art-pw}
  github-release:
    auth: {token_ref: ghes-token}
`))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Sources["npm"].Auth; a.EffectiveType() != AuthNPMAuthToken || a.TokenRef != "nexus-npm" {
		t.Errorf("npm auth = %+v", a)
	}
	if a := cfg.Sources["pypi"].Auth; a.EffectiveType() != AuthBasic || a.Username != "ci-bot" || a.PasswordRef != "art-pw" {
		t.Errorf("pypi auth = %+v", a)
	}
	if a := cfg.Sources["github-release"].Auth; a.EffectiveType() != AuthBearer {
		t.Errorf("default type = %q, want bearer", a.EffectiveType())
	}
}

func TestSourceAuthRejects(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"bearer needs a ref":       {"auth: {type: bearer}", "token_ref: required"},
		"basic needs a user":       {"auth: {type: basic, password_ref: pw}", "username: required"},
		"basic needs a password":   {"auth: {type: basic, username: u}", "password_ref: required"},
		"basic rejects token_ref":  {"auth: {type: basic, username: u, password_ref: pw, token_ref: t}", "not token_ref"},
		"bearer rejects password":  {"auth: {token_ref: t, password_ref: p}", "token_ref only"},
		"unknown type":             {"auth: {type: digest, token_ref: t}", "want bearer, basic or npm_auth_token"},
		"token_env and auth":       {"token_env: SOME_TOKEN\n    auth: {token_ref: t}", "not both"},
		"a value in token_ref":     {"auth: {token_ref: Sup3rSecretValue}", "not a value"},
		"a token as user name":     {"auth: {type: basic, username: ghp_abcdef, password_ref: pw}", "looks like a token"},
		"unknown auth key (value)": {"auth: {token: abc}", "line"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, writeFile(t, "sources:\n  npm:\n    "+tc.yaml+"\n"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, leak := range []string{"Sup3rSecretValue", "ghp_abcdef"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the error echoes %q", leak)
				}
			}
		})
	}
}

func selfSignedPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "angelia test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCAFile(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(good, selfSignedPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := load(t, writeFile(t, "http:\n  ca_file: "+empty+"\n"), "ANGELIA_HTTP_CA_FILE="+good)
	if err != nil || cfg.HTTP.CAFile != good {
		t.Fatalf("the env must override the file: %q, %v", cfg.HTTP.CAFile, err)
	}
	pool, err := cfg.HTTP.LoadCAPool()
	if err != nil || pool == nil {
		t.Fatalf("LoadCAPool: %v", err)
	}
	if _, err := (HTTP{CAFile: empty}).LoadCAPool(); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("a file without certificates: %v", err)
	}
	if _, err := (HTTP{CAFile: filepath.Join(dir, "missing.pem")}).LoadCAPool(); err == nil {
		t.Error("a missing file must fail")
	}
	if p, err := (HTTP{}).LoadCAPool(); p != nil || err != nil {
		t.Errorf("no ca_file: %v, %v; want nil, nil", p, err)
	}
}

func TestBasicAuthRefusedForGitHubTagAndGitLab(t *testing.T) {
	for _, kind := range []string{"github-tag", "gitlab-release"} {
		_, err := load(t, writeFile(t, "sources:\n  "+kind+":\n    auth: {type: basic, username: u, password_ref: pw}\n"))
		if err == nil || !strings.Contains(err.Error(), "basic is not accepted") {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
}

// Named source instances (#205): sources.<name>.kind selects the
// implementation; auth and token_env belong to the instance.
func TestNamedSourceInstanceParses(t *testing.T) {
	cfg, err := load(t, writeFile(t, `
sources:
  npm: {base_url: https://registry.npmjs.org}
  npm-internal:
    kind: npm
    base_url: https://nexus.example.com/repository/npm-group
    auth: {token_ref: nexus-npm-token}
  npm-legacy:
    kind: npm
    token_env: NEXUS_TOKEN
`), "NEXUS_TOKEN=t0ken")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"npm": "npm", "npm-internal": "npm", "npm-legacy": "npm", "unlisted": "unlisted"} {
		if got := cfg.SourceKind(name); got != want {
			t.Errorf("SourceKind(%s) = %q, want %q", name, got, want)
		}
	}
	if got := cfg.Sources["npm-internal"]; got.BaseURL == "" || got.Auth.TokenRef != "nexus-npm-token" {
		t.Errorf("npm-internal = %+v", got)
	}
	if cfg.SourceToken("npm-legacy") != "t0ken" || cfg.SourceToken("npm") != "" {
		t.Error("token_env is read per instance name")
	}
	for _, w := range cfg.Warnings() {
		t.Errorf("unexpected warning: %s", w)
	}
}

func TestNamedSourceInstanceRejectsABadKind(t *testing.T) {
	_, err := load(t, writeFile(t, "sources:\n  mine:\n    kind: Not_A_Kind\n"))
	if err == nil || !strings.Contains(err.Error(), "sources.mine.kind: invalid kind") {
		t.Fatalf("err = %v", err)
	}
}

// The credential warning looks at the instance's kind, not its name.
func TestNamedSourceCredentialWarningUsesTheKind(t *testing.T) {
	cfg, err := load(t, writeFile(t, "sources:\n  internal-oci:\n    kind: oci\n    auth: {token_ref: t}\n  internal-npm:\n    kind: npm\n    auth: {token_ref: t}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if !strings.Contains(w, "sources.internal-oci:") || strings.Contains(w, "sources.internal-npm:") {
		t.Errorf("warnings = %q", w)
	}
}
