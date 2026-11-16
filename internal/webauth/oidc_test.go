package webauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
)

const (
	testClientID     = "angelia-test"
	testClientSecret = "client-secret-value-0123456789abcdef"
)

// testIssuer is an OpenID provider on httptest: discovery, JWKS, and a
// token endpoint that checks the client, the code and the PKCE verifier and
// returns an ID token built by claims and signed by sign.
type testIssuer struct {
	t   *testing.T
	srv *httptest.Server
	key crypto.Signer
	alg jose.SignatureAlgorithm

	mu sync.Mutex
	// codes maps an issued code to the challenge and nonce of its sign-in.
	codes map[string][2]string
	// claims changes the ID token's claims; nil keeps the defaults.
	claims func(c map[string]any)
	// sign replaces the signature of the ID token (alg none, HS256).
	sign func(payload []byte) string
	// discovery changes the discovery document.
	discovery func(d map[string]any)
	// tokenStatus, if set, is the status of the token endpoint with an
	// OAuth error body.
	tokenStatus int
	noIDToken   bool
	// discoveryHits counts the discovery requests.
	discoveryHits int
	// clientAuth is how the last token request authenticated the client.
	clientAuth string
}

func newTestIssuer(t *testing.T, alg jose.SignatureAlgorithm) *testIssuer {
	t.Helper()
	is := &testIssuer{t: t, alg: alg, codes: map[string][2]string{}}
	switch alg {
	case jose.ES256:
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		is.key = k
	default:
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		is.key = k
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", is.serveDiscovery)
	mux.HandleFunc("GET /jwks", is.serveJWKS)
	mux.HandleFunc("POST /token", is.serveToken)
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *testIssuer) serveDiscovery(w http.ResponseWriter, _ *http.Request) {
	is.mu.Lock()
	is.discoveryHits++
	d := map[string]any{
		"issuer":                                is.srv.URL,
		"authorization_endpoint":                is.srv.URL + "/authorize",
		"token_endpoint":                        is.srv.URL + "/token",
		"jwks_uri":                              is.srv.URL + "/jwks",
		"code_challenge_methods_supported":      []string{"S256"},
		"id_token_signing_alg_values_supported": []string{string(is.alg)},
	}
	if is.discovery != nil {
		is.discovery(d)
	}
	is.mu.Unlock()
	if d["status"] != nil {
		w.WriteHeader(d["status"].(int))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

func (is *testIssuer) serveJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: is.key.Public(), KeyID: "k1", Algorithm: string(is.alg), Use: "sig"},
	}})
}

// authorize plays the browser and the provider's sign-in page: it reads
// the authorization URL and issues a code bound to its challenge and nonce.
func (is *testIssuer) authorize(t *testing.T, authURL string) (code string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || !strings.HasPrefix(authURL, is.srv.URL+"/authorize?") {
		t.Fatalf("authorization URL %q", authURL)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != testClientID || q.Get("code_challenge_method") != "S256" ||
		q.Get("nonce") == "" || q.Get("state") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("authorization URL %q", authURL)
	}
	code = "code-" + random(8)
	is.mu.Lock()
	is.codes[code] = [2]string{q.Get("code_challenge"), q.Get("nonce")}
	is.mu.Unlock()
	return code
}

func (is *testIssuer) serveToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, secret, ok := r.BasicAuth()
	auth := "client_secret_basic"
	if !ok {
		id, secret, auth = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret"), "client_secret_post"
	}
	is.mu.Lock()
	defer is.mu.Unlock()
	is.clientAuth = auth
	oauthErr := func(status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		// The description echoes the verifier: the error must not.
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "nope " + r.PostForm.Get("code_verifier")})
	}
	if is.tokenStatus != 0 {
		oauthErr(is.tokenStatus, "invalid_grant")
		return
	}
	got, known := is.codes[r.PostForm.Get("code")]
	delete(is.codes, r.PostForm.Get("code"))
	switch {
	case id != testClientID || secret != testClientSecret:
		oauthErr(http.StatusUnauthorized, "invalid_client")
		return
	case !known || r.PostForm.Get("grant_type") != "authorization_code" || challengeS256(r.PostForm.Get("code_verifier")) != got[0]:
		oauthErr(http.StatusBadRequest, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss": is.srv.URL, "sub": "user-1", "aud": testClientID, "exp": now.Add(5 * time.Minute).Unix(),
		"iat": now.Unix(), "nonce": got[1], "email": "Alice@Example.com", "email_verified": true,
		"name": "Alice A.", "preferred_username": "alice", "picture": "https://idp.example/alice.png",
	}
	if is.claims != nil {
		is.claims(claims)
	}
	payload, _ := json.Marshal(claims)
	var idToken string
	if is.sign != nil {
		idToken = is.sign(payload)
	} else {
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: is.alg, Key: is.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
		if err != nil {
			is.t.Error(err)
			return
		}
		jws, err := s.Sign(payload)
		if err != nil {
			is.t.Error(err)
			return
		}
		idToken, _ = jws.CompactSerialize()
	}
	resp := map[string]any{"access_token": "at-secret", "token_type": "Bearer", "expires_in": 300}
	if !is.noIDToken {
		resp["id_token"] = idToken
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (is *testIssuer) set(f func(is *testIssuer)) {
	is.mu.Lock()
	defer is.mu.Unlock()
	f(is)
}

func (is *testIssuer) provider(t *testing.T, name string) *OIDC {
	t.Helper()
	p, err := NewOIDC(OIDCConfig{
		Name: name, Issuer: is.srv.URL, ClientID: testClientID, ClientSecret: secrets.NewValueString(testClientSecret),
		RedirectURL: "https://angelia.example.com/ui/auth/callback/" + name,
		Scopes:      config.DefaultOIDCScopes, Algs: config.OIDCAlgs(), HTTP: is.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// signIn runs AuthCodeURL, the provider's page and Exchange.
func (is *testIssuer) signIn(t *testing.T, p *OIDC) (Identity, error) {
	t.Helper()
	verifier, nonce := random(32), random(32)
	authURL, err := p.AuthCodeURL(t.Context(), random(32), challengeS256(verifier), nonce)
	if err != nil {
		t.Fatal(err)
	}
	return p.Exchange(t.Context(), is.authorize(t, authURL), verifier, nonce)
}

func TestOIDCSignIn(t *testing.T) {
	for _, alg := range []jose.SignatureAlgorithm{jose.RS256, jose.ES256} {
		t.Run(string(alg), func(t *testing.T) {
			is := newTestIssuer(t, alg)
			p := is.provider(t, "authentik")
			verifier, nonce := random(32), random(32)
			authURL, err := p.AuthCodeURL(t.Context(), "st", challengeS256(verifier), nonce)
			if err != nil {
				t.Fatal(err)
			}
			q, _ := url.Parse(authURL)
			if got := q.Query(); got.Get("scope") != "openid email profile" || got.Get("state") != "st" ||
				got.Get("redirect_uri") != "https://angelia.example.com/ui/auth/callback/authentik" || got.Get("nonce") != nonce {
				t.Fatal(authURL)
			}
			id, err := p.Exchange(t.Context(), is.authorize(t, authURL), verifier, nonce)
			if err != nil {
				t.Fatal(err)
			}
			want := Identity{
				Provider: "authentik", Subject: "user-1", Login: "alice", Name: "Alice A.",
				AvatarURL: "https://idp.example/alice.png", Email: "Alice@Example.com", EmailVerified: true,
			}
			if id != want || id.PrincipalID() != "authentik:user-1" || id.verifiedEmail() != "Alice@Example.com" {
				t.Fatalf("%+v", id)
			}
			// A second sign-in reuses the discovery.
			if _, err := is.signIn(t, p); err != nil || is.discoveryHits != 1 {
				t.Fatal(err, is.discoveryHits)
			}
		})
	}
}

// The client authenticates with client_secret_basic, or client_secret_post
// when the provider lists only that.
func TestOIDCClientAuth(t *testing.T) {
	for methods, want := range map[string]string{"": "client_secret_basic", "client_secret_post": "client_secret_post", "client_secret_basic client_secret_post": "client_secret_basic"} {
		is := newTestIssuer(t, jose.RS256)
		if methods != "" {
			is.discovery = func(d map[string]any) { d["token_endpoint_auth_methods_supported"] = strings.Fields(methods) }
		}
		if _, err := is.signIn(t, is.provider(t, "kc")); err != nil || is.clientAuth != want {
			t.Fatalf("%q: %v %s", methods, err, is.clientAuth)
		}
	}
}

func TestOIDCClaims(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	for name, tc := range map[string]struct {
		claims func(map[string]any)
		want   Identity
	}{
		"verified as a string": {func(c map[string]any) { c["email_verified"] = "true" }, Identity{Login: "alice", EmailVerified: true}},
		"unverified":           {func(c map[string]any) { c["email_verified"] = false }, Identity{Login: "alice"}},
		"no verified claim":    {func(c map[string]any) { delete(c, "email_verified") }, Identity{Login: "alice"}},
		"login from email": {
			func(c map[string]any) { delete(c, "preferred_username"); c["email_verified"] = "yes" },
			Identity{Login: "Alice@Example.com"},
		},
		"login from subject": {func(c map[string]any) {
			delete(c, "preferred_username")
			delete(c, "email")
			c["picture"] = "http://insecure.example/a.png"
		}, Identity{Login: "user-1", EmailVerified: true}},
	} {
		t.Run(name, func(t *testing.T) {
			is.set(func(is *testIssuer) { is.claims = tc.claims })
			id, err := is.signIn(t, p)
			if err != nil {
				t.Fatal(err)
			}
			if id.Login != tc.want.Login || id.EmailVerified != tc.want.EmailVerified {
				t.Fatalf("%+v", id)
			}
			if !id.EmailVerified && id.verifiedEmail() != "" {
				t.Fatal("an unverified email is used")
			}
			if name == "login from subject" && id.AvatarURL != "" {
				t.Fatal("an http picture is kept")
			}
		})
	}
}

// Every ID token that fails a check is refused, as a ProviderError that
// holds no token, code, verifier or secret.
func TestOIDCTokenRefused(t *testing.T) {
	hs256 := func(payload []byte) string {
		// The classic confusion: an HMAC with the client secret.
		s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(testClientSecret)}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		if err != nil {
			t.Error(err) // the server's goroutine: no Fatal
			return ""
		}
		jws, _ := s.Sign(payload)
		out, _ := jws.CompactSerialize()
		return out
	}
	none := func(payload []byte) string {
		enc := base64.RawURLEncoding.EncodeToString
		return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + "."
	}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	foreign := func(payload []byte) string {
		s, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: otherKey}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		jws, _ := s.Sign(payload)
		out, _ := jws.CompactSerialize()
		return out
	}
	for name, tc := range map[string]struct {
		claims func(map[string]any)
		sign   func([]byte) string
		want   string
	}{
		"bad nonce":      {claims: func(c map[string]any) { c["nonce"] = "another" }, want: "nonce"},
		"no nonce":       {claims: func(c map[string]any) { delete(c, "nonce") }, want: "nonce"},
		"bad audience":   {claims: func(c map[string]any) { c["aud"] = "someone-else" }, want: "audience"},
		"other azp":      {claims: func(c map[string]any) { c["aud"] = []string{testClientID, "other"}; c["azp"] = "other" }, want: "azp"},
		"several aud":    {claims: func(c map[string]any) { c["aud"] = []string{testClientID, "other"} }, want: "azp"},
		"expired":        {claims: func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, want: "expired"},
		"other issuer":   {claims: func(c map[string]any) { c["iss"] = "https://evil.example" }, want: "different provider"},
		"bad subject":    {claims: func(c map[string]any) { c["sub"] = "has space" }, want: "subject"},
		"empty subject":  {claims: func(c map[string]any) { c["sub"] = "" }, want: "subject"},
		"long subject":   {claims: func(c map[string]any) { c["sub"] = strings.Repeat("s", 256) }, want: "subject"},
		"alg none":       {sign: none, want: "ID token refused"},
		"alg HS256":      {sign: hs256, want: "ID token refused"},
		"foreign key":    {sign: foreign, want: "ID token refused"},
		"not a jwt":      {sign: func([]byte) string { return "garbage" }, want: "ID token refused"},
		"no id token":    {want: "no ID token"},
		"token rejected": {want: "invalid_grant"},
	} {
		t.Run(name, func(t *testing.T) {
			is := newTestIssuer(t, jose.RS256)
			is.claims, is.sign = tc.claims, tc.sign
			is.noIDToken = name == "no id token"
			if name == "token rejected" {
				is.tokenStatus = http.StatusBadRequest
			}
			p := is.provider(t, "kc")
			verifier, nonce := random(32), random(32)
			authURL, err := p.AuthCodeURL(t.Context(), random(32), challengeS256(verifier), nonce)
			if err != nil {
				t.Fatal(err)
			}
			code := is.authorize(t, authURL)
			_, err = p.Exchange(t.Context(), code, verifier, nonce)
			var pe *ProviderError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "webauth: kc: ") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, secret := range []string{code, verifier, testClientSecret, "at-secret", "eyJ"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("the error holds %q: %v", secret, err)
				}
			}
		})
	}
}

// A wrong PKCE verifier or client secret is refused by the token endpoint.
func TestOIDCExchangeRefused(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	verifier, nonce := random(32), random(32)
	authURL, _ := p.AuthCodeURL(t.Context(), "s", challengeS256(verifier), nonce)
	if _, err := p.Exchange(t.Context(), is.authorize(t, authURL), random(32), nonce); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Exchange(ctx, is.authorize(t, authURL), verifier, nonce); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The issuer goes away: a request failure.
	is.srv.Close()
	if _, err := p.Exchange(t.Context(), "c", verifier, nonce); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatal(err)
	}
}

func TestOIDCDiscovery(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(map[string]any)
		want   string
	}{
		"status":           {func(d map[string]any) { d["status"] = http.StatusInternalServerError }, "discovery failed"},
		"issuer mismatch":  {func(d map[string]any) { d["issuer"] = "https://other.example" }, "discovery failed"},
		"http endpoint":    {func(d map[string]any) { d["token_endpoint"] = "http://idp.example/token" }, "token_endpoint is not an https URL"},
		"no jwks":          {func(d map[string]any) { delete(d, "jwks_uri") }, "jwks_uri"},
		"no S256":          {func(d map[string]any) { d["code_challenge_methods_supported"] = []string{"plain"} }, "PKCE S256"},
		"bad auth url":     {func(d map[string]any) { d["authorization_endpoint"] = "://x" }, "authorization_endpoint"},
		"creds in the url": {func(d map[string]any) { d["authorization_endpoint"] = "https://u:p@idp.example/a" }, "authorization_endpoint"},
		"no client auth":   {func(d map[string]any) { d["token_endpoint_auth_methods_supported"] = []string{"private_key_jwt"} }, "client_secret_basic"},
	} {
		t.Run(name, func(t *testing.T) {
			is := newTestIssuer(t, jose.RS256)
			is.discovery = tc.change
			p := is.provider(t, "kc")
			_, err := p.AuthCodeURL(t.Context(), "s", "c", "n")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			// A failed discovery is retried at the next sign-in.
			is.set(func(is *testIssuer) { is.discovery = nil })
			if _, err := p.AuthCodeURL(t.Context(), "s", "c", "n"); err != nil {
				t.Fatal(err)
			}
			if _, err := is.signIn(t, p); err != nil {
				t.Fatal(err)
			}
		})
	}
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.AuthCodeURL(ctx, "s", "c", "n"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.Exchange(ctx, "c", "v", "n"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNewOIDC(t *testing.T) {
	ok := OIDCConfig{
		Name: "kc", Issuer: "https://idp.example/realms/x", ClientID: "c", ClientSecret: secrets.NewValueString("s"),
		RedirectURL: "https://a.example/ui/auth/callback/kc", Scopes: []string{"openid"}, Algs: []string{"RS256"}, HTTP: http.DefaultClient,
	}
	if p, err := NewOIDC(ok); err != nil || p.Name() != "kc" {
		t.Fatal(err)
	}
	for name, change := range map[string]func(c *OIDCConfig){
		"github name":    func(c *OIDCConfig) { c.Name = GitHubName },
		"bad name":       func(c *OIDCConfig) { c.Name = "Bad Name" },
		"no secret":      func(c *OIDCConfig) { c.ClientSecret = secrets.Value{} },
		"no client":      func(c *OIDCConfig) { c.HTTP = nil },
		"http issuer":    func(c *OIDCConfig) { c.Issuer = "http://idp.example" },
		"not a url":      func(c *OIDCConfig) { c.Issuer = "idp" },
		"no openid":      func(c *OIDCConfig) { c.Scopes = []string{"email"} },
		"no algs":        func(c *OIDCConfig) { c.Algs = nil },
		"HS256":          func(c *OIDCConfig) { c.Algs = []string{"RS256", "HS256"} },
		"none":           func(c *OIDCConfig) { c.Algs = []string{"none"} },
		"no redirect":    func(c *OIDCConfig) { c.RedirectURL = "" },
		"no issuer":      func(c *OIDCConfig) { c.Issuer = "" },
		"no client id":   func(c *OIDCConfig) { c.ClientID = "" },
		"empty name":     func(c *OIDCConfig) { c.Name = "" },
		"bad issuer url": func(c *OIDCConfig) { c.Issuer = "https://%zz" },
	} {
		c := ok
		change(&c)
		if _, err := NewOIDC(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c := ok
	c.Issuer = "http://localhost:8080/realms/x"
	if _, err := NewOIDC(c); err != nil {
		t.Fatal("loopback http issuer refused:", err)
	}
	// The algorithm list is the caller's: HS256 cannot be slipped in.
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	p.cfg.Algs[0] = "changed"
	if config.OIDCAlgs()[0] != "RS256" {
		t.Fatal("NewOIDC shares the algorithm list")
	}
}

// oidcUI is a UI with an OIDC provider "idp": user-1 is an admin by
// subject, alice@example.com an editor by email, bob@example.com a viewer.
func oidcUI() config.UI {
	return config.UI{
		Enabled: true, PublicURL: "https://angelia.example.com",
		OIDC: []config.UIOIDC{{Name: "idp"}},
		Access: []config.UIAccess{
			{Provider: "idp", Subject: "admin-sub", Role: config.UIRoleAdmin},
			{Provider: "idp", Email: "Alice@example.com", Role: config.UIRoleEditor},
			{Provider: "idp", Email: "bob@example.com", Role: config.UIRoleViewer},
			{Provider: "other", Subject: "user-1", Role: config.UIRoleAdmin},
			{GitHubID: 1, Role: config.UIRoleAdmin},
		},
		Session: config.UISession{IdleTimeout: time.Hour, MaxLifetime: 24 * time.Hour},
	}
}

// The handler signs in through a real OIDC provider: the nonce of the
// cookie reaches the ID token check, and email entries pin their subject.
func TestHandlerOIDC(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	s, st, c, logs := newTestSessions(t, oidcUI())
	m := &fakeMetrics{}
	h, err := NewHandler(HandlerOptions{
		Providers: []IdentityProvider{is.provider(t, "idp")}, Sessions: s, UI: oidcUI(), Metrics: m,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &handlerEnv{t: t, h: h, s: s, st: st, c: c, m: m, logs: logs, session: SessionCookie, oauth: oauthCookieName}
	signIn := func(t *testing.T, claims func(map[string]any)) result {
		t.Helper()
		is.set(func(is *testIssuer) { is.claims = claims })
		resp := e.do(httptest.NewRequest(http.MethodGet, "/ui/auth/login/idp", nil))
		if resp.StatusCode != http.StatusFound {
			t.Fatal(resp.Status, resp.Body)
		}
		oc := cookieOf(resp, e.oauth)
		loc := resp.Header.Get("Location")
		u, _ := url.Parse(loc)
		if oc == nil || len(strings.Split(oc.Value, ".")) != 5 || !strings.Contains(oc.Value, u.Query().Get("nonce")) {
			t.Fatalf("oauth cookie %+v", oc)
		}
		code := is.authorize(t, loc)
		r := httptest.NewRequest(http.MethodGet, "/ui/auth/callback/idp?code="+code+"&state="+u.Query().Get("state"), nil)
		addCookie(r, oc.Name, oc.Value)
		return e.do(r)
	}
	principal := func(t *testing.T, resp result) (string, string) {
		t.Helper()
		sc := cookieOf(resp, SessionCookie)
		if resp.StatusCode != http.StatusSeeOther || sc == nil {
			t.Fatal(resp.Status, resp.Body)
		}
		p, err := s.AuthenticateSession(t.Context(), sc.Value)
		if err != nil {
			t.Fatal(err)
		}
		return p.ID, p.Session.Role
	}

	// By subject.
	if id, role := principal(t, signIn(t, func(c map[string]any) { c["sub"] = "admin-sub"; c["email"] = "root@example.com" })); id != "idp:admin-sub" || role != config.UIRoleAdmin {
		t.Fatal(id, role)
	}
	// By verified email: the address is pinned to user-1.
	resp := signIn(t, func(c map[string]any) { c["email"] = "ALICE@example.com" })
	if id, role := principal(t, resp); id != "idp:user-1" || role != config.UIRoleEditor {
		t.Fatal(id, role)
	}
	aliceCookie := cookieOf(resp, SessionCookie).Value
	if got, _ := st.UIIdentityPin(t.Context(), "idp", "email:alice@example.com"); got != "user-1" {
		t.Fatal("pin:", got)
	}
	// An unverified email matches nothing, and pins nothing.
	resp = signIn(t, func(c map[string]any) {
		c["sub"] = "user-2"
		c["email"] = "bob@example.com"
		c["email_verified"] = false
	})
	if resp.StatusCode != http.StatusForbidden || e.m.get("idp/not_authorised") != 1 {
		t.Fatal(resp.Status)
	}
	if _, err := st.UIIdentityPin(t.Context(), "idp", "email:bob@example.com"); err == nil {
		t.Fatal("an unverified email was pinned")
	}
	// Another account with alice's address: refused, identity_mismatch.
	resp = signIn(t, func(c map[string]any) { c["sub"] = "user-3"; c["email"] = "alice@example.com" })
	if resp.StatusCode != http.StatusForbidden || e.m.get("idp/identity_mismatch") != 1 {
		t.Fatal(resp.Status, e.m.n)
	}
	if !strings.Contains(logs.String(), "pinned to another account id") {
		t.Fatal(logs.String())
	}
	// user-1 changes their address at the provider: the pinned entry still
	// follows the account, on later requests and at the next sign-in.
	if id, role := principal(t, signIn(t, func(c map[string]any) { c["email"] = "alice@new.example" })); id != "idp:user-1" || role != config.UIRoleEditor {
		t.Fatal(id, role)
	}
	// A session re-evaluates the role on each request: from the pin.
	// The first session (its cookie was not sent again) is still active.
	if p, err := s.AuthenticateSession(t.Context(), aliceCookie); err != nil || p.Session.Role != config.UIRoleEditor {
		t.Fatal(p, err)
	}
	// A bad nonce at the provider: provider_error, no session.
	resp = signIn(t, func(c map[string]any) { c["nonce"] = "forged" })
	if resp.StatusCode != http.StatusBadGateway || cookieOf(resp, SessionCookie) != nil || e.m.get("idp/provider_error") != 1 {
		t.Fatal(resp.Status)
	}
	// Unknown providers are not found.
	for _, path := range []string{"/ui/auth/login/nope", "/ui/auth/callback/nope?code=x&state=y", "/ui/auth/login/other"} {
		if resp := e.do(httptest.NewRequest(http.MethodGet, path, nil)); resp.StatusCode != http.StatusNotFound {
			t.Fatal(path, resp.Status)
		}
	}
	logs2 := logs.String()
	for _, secret := range []string{testClientSecret, "at-secret", aliceCookie} {
		if strings.Contains(logs2, secret) {
			t.Fatalf("the log holds %q", secret)
		}
	}
}

// The issuer cannot be reached: the sign-in shows the provider error page
// and sets no cookie.
func TestHandlerOIDCDiscoveryDown(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "idp")
	is.srv.Close()
	s, _, c, logs := newTestSessions(t, oidcUI())
	m := &fakeMetrics{}
	h, err := NewHandler(HandlerOptions{
		Providers: []IdentityProvider{p}, Sessions: s, UI: oidcUI(), Metrics: m,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/auth/login/idp", nil))
	if w.Code != http.StatusBadGateway || w.Header().Get("Set-Cookie") != "" || m.get("idp/provider_error") != 1 {
		t.Fatal(w.Code, w.Header())
	}
}

// Email and subject entries list their subjects for the orphan tokens
// check, and an email entry only through its pin.
func TestOIDCListsSubject(t *testing.T) {
	s, st, _, _ := newTestSessions(t, oidcUI())
	for _, tc := range []struct {
		provider, subject string
		want              bool
	}{
		{"idp", "admin-sub", true},
		{"idp", "user-1", false}, // alice@ is not pinned yet
		{"gone", "user-1", false},
		{GitHubName, "1", true},
	} {
		if got, err := s.access.listsSubject(t.Context(), tc.provider, tc.subject); err != nil || got != tc.want {
			t.Errorf("%s:%s = %v %v", tc.provider, tc.subject, got, err)
		}
	}
	if _, err := st.PinUIIdentity(t.Context(), "idp", "email:alice@example.com", "user-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, err := s.access.listsSubject(t.Context(), "idp", "user-1"); err != nil || !got {
		t.Fatal(got, err)
	}
	_ = st.Close()
	s2 := NewSessions(st, oidcUI(), nil)
	if _, err := s2.access.listsSubject(t.Context(), "idp", "nobody"); err == nil {
		t.Fatal("a store failure is not reported")
	}
	if _, err := s2.access.role(t.Context(), "idp", "nobody", "x", ""); err == nil {
		t.Fatal("a store failure is not reported")
	}
	if _, err := s2.access.role(t.Context(), "idp", "nobody", "x", "alice@example.com"); err == nil {
		t.Fatal("a store failure is not reported")
	}
}
