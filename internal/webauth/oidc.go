package webauth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// discoveryTimeout bounds the discovery request of a sign-in.
const discoveryTimeout = 15 * time.Second

// oidcSubjectRE is a subject that fits the API's PrincipalId
// ("<provider>:" then 1 to 255 printable ASCII characters without space).
var oidcSubjectRE = regexp.MustCompile(`^[!-~]{1,255}$`)

// OIDCConfig configures an OpenID Connect provider (#268).
type OIDCConfig struct {
	// Name is the provider's name (config ui.oidc[].name).
	Name string
	// Issuer is the issuer URL; the discovery document is read from
	// <Issuer>/.well-known/openid-configuration and must state the same
	// issuer.
	Issuer       string
	ClientID     string
	ClientSecret secrets.Value
	// RedirectURL is <public_url>/ui/auth/callback/<Name>.
	RedirectURL string
	// Scopes must contain openid.
	Scopes []string
	// Algs are the ID token signature algorithms accepted: asymmetric
	// only (config.OIDCAlgs). none and HS* are refused here too.
	Algs []string
	// HTTP sends the requests; `angelia serve` passes netguard's client.
	HTTP *http.Client
	// Now is the clock of the ID token's expiry check (tests).
	Now func() time.Time
}

// OIDC is a generic OpenID Connect [IdentityProvider]: the authorization
// code flow with PKCE (S256), state and nonce. The provider's metadata is
// discovered at the first sign-in (and again at the next one after a
// failure), so an issuer that is down does not stop the api role. The ID
// token is verified with go-oidc against the issuer's JWKS: signature (an
// allowed asymmetric algorithm), iss, aud, azp, exp and nonce. The access
// token is not used.
type OIDC struct {
	cfg    OIDCConfig
	client *http.Client
	local  bool // the issuer is a loopback http:// URL

	mu   sync.Mutex
	disc *oidcDiscovered
}

// oidcDiscovered is what the discovery gave.
type oidcDiscovered struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

var _ IdentityProvider = (*OIDC)(nil)

// NewOIDC returns an OIDC provider. It sends nothing.
func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if cfg.Name == "" || cfg.Name == GitHubName || !providerNameRE.MatchString(cfg.Name) {
		return nil, fmt.Errorf("webauth: oidc: invalid provider name %q", cfg.Name)
	}
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.ClientSecret.IsZero() || cfg.RedirectURL == "" || cfg.HTTP == nil {
		return nil, fmt.Errorf("webauth: oidc %s: issuer, client id, client secret, redirect URL and HTTP client are required", cfg.Name)
	}
	iss, err := url.Parse(cfg.Issuer)
	if err != nil || iss.Host == "" {
		return nil, fmt.Errorf("webauth: oidc %s: the issuer is not an absolute URL", cfg.Name)
	}
	local := iss.Scheme == "http" && isLoopback(iss.Hostname())
	if iss.Scheme != "https" && !local {
		return nil, fmt.Errorf("webauth: oidc %s: the issuer must be an https URL (http only on a loopback host)", cfg.Name)
	}
	if !slices.Contains(cfg.Scopes, oidc.ScopeOpenID) {
		return nil, fmt.Errorf("webauth: oidc %s: the scopes must contain openid", cfg.Name)
	}
	if len(cfg.Algs) == 0 {
		return nil, fmt.Errorf("webauth: oidc %s: no signature algorithm is allowed", cfg.Name)
	}
	for _, a := range cfg.Algs {
		if !asymmetricAlg(a) {
			return nil, fmt.Errorf("webauth: oidc %s: signature algorithm %q is refused (asymmetric algorithms only)", cfg.Name, a)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// No request follows a redirect: the discovery document, the keys and
	// the token come from the URLs the issuer states. The caller's client
	// is copied.
	client := *cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.Scopes = slices.Clone(cfg.Scopes)
	cfg.Algs = slices.Clone(cfg.Algs)
	return &OIDC{cfg: cfg, client: &client, local: local}, nil
}

// asymmetricAlg reports whether a is an asymmetric JWS algorithm go-oidc
// verifies.
func asymmetricAlg(a string) bool {
	switch a {
	case oidc.RS256, oidc.RS384, oidc.RS512, oidc.PS256, oidc.PS384, oidc.PS512,
		oidc.ES256, oidc.ES384, oidc.ES512, oidc.EdDSA:
		return true
	}
	return false
}

func isLoopback(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Name implements [IdentityProvider].
func (o *OIDC) Name() string { return o.cfg.Name }

func (o *OIDC) err(format string, a ...any) error {
	return &ProviderError{provider: o.cfg.Name, msg: fmt.Sprintf(format, a...)}
}

// discover reads the provider's metadata once; a failure is retried at the
// next sign-in.
func (o *OIDC) discover(ctx context.Context) (*oidcDiscovered, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.disc != nil {
		return o.disc, nil
	}
	dctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	p, err := oidc.NewProvider(oidc.ClientContext(dctx, o.client), o.cfg.Issuer)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("webauth: %s: discovery: %w", o.cfg.Name, ctx.Err())
		}
		return nil, o.err("discovery failed: %s", cutText(err.Error(), 300))
	}
	var meta struct {
		AuthURL  string   `json:"authorization_endpoint"`
		TokenURL string   `json:"token_endpoint"`
		JWKSURL  string   `json:"jwks_uri"`
		PKCE     []string `json:"code_challenge_methods_supported"`
		Auth     []string `json:"token_endpoint_auth_methods_supported"`
	}
	if err := p.Claims(&meta); err != nil {
		return nil, o.err("discovery document is not the expected JSON")
	}
	for _, e := range []struct{ name, v string }{
		{"authorization_endpoint", meta.AuthURL}, {"token_endpoint", meta.TokenURL}, {"jwks_uri", meta.JWKSURL},
	} {
		if !o.endpointOK(e.v) {
			return nil, o.err("discovery: %s is not an https URL", e.name)
		}
	}
	if len(meta.PKCE) > 0 && !slices.Contains(meta.PKCE, "S256") {
		return nil, o.err("discovery: the provider does not support PKCE S256")
	}
	// client_secret_basic, the default of OIDC Discovery, unless the
	// provider lists client_secret_post only: one attempt, no guessing.
	endpoint := p.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	if len(meta.Auth) > 0 && !slices.Contains(meta.Auth, "client_secret_basic") {
		if !slices.Contains(meta.Auth, "client_secret_post") {
			return nil, o.err("discovery: the token endpoint accepts neither client_secret_basic nor client_secret_post")
		}
		endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	d := &oidcDiscovered{
		oauth: oauth2.Config{
			ClientID: o.cfg.ClientID, ClientSecret: string(o.cfg.ClientSecret.Reveal()),
			Endpoint: endpoint, RedirectURL: o.cfg.RedirectURL, Scopes: o.cfg.Scopes,
		},
		verifier: p.Verifier(&oidc.Config{ClientID: o.cfg.ClientID, SupportedSigningAlgs: o.cfg.Algs, Now: o.cfg.Now}),
	}
	o.disc = d
	return d, nil
}

// endpointOK accepts an https URL, or an http URL on a loopback host when
// the issuer is one too (local development).
func (o *OIDC) endpointOK(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	return u.Scheme == "https" || (o.local && u.Scheme == "http" && isLoopback(u.Hostname()))
}

// AuthCodeURL implements [IdentityProvider].
func (o *OIDC) AuthCodeURL(ctx context.Context, state, codeChallenge, nonce string) (string, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	return d.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oidc.Nonce(nonce),
	), nil
}

// oidcClaims are the ID token claims read. email_verified is a boolean,
// or the string "true" for a few providers.
type oidcClaims struct {
	Email             string          `json:"email"`
	EmailVerified     json.RawMessage `json:"email_verified"`
	Name              string          `json:"name"`
	PreferredUsername string          `json:"preferred_username"`
	Picture           string          `json:"picture"`
	AZP               string          `json:"azp"`
}

func (c oidcClaims) verified() bool {
	s := string(c.EmailVerified)
	return s == "true" || s == `"true"`
}

// Exchange implements [IdentityProvider]. It trades the code for the
// tokens, verifies the ID token and reads the identity from its claims; the
// access token and the refresh token, if any, are dropped.
func (o *OIDC) Exchange(ctx context.Context, code, codeVerifier, nonce string) (Identity, error) {
	d, err := o.discover(ctx)
	if err != nil {
		return Identity{}, err
	}
	tok, err := d.oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, o.client), code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return Identity{}, o.exchangeErr(ctx, err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, o.err("token exchange returned no ID token")
	}
	idt, err := d.verifier.Verify(ctx, raw)
	if err != nil {
		if ctx.Err() != nil {
			return Identity{}, fmt.Errorf("webauth: %s: ID token: %w", o.cfg.Name, ctx.Err())
		}
		return Identity{}, o.err("ID token refused: %s", cutText(err.Error(), 300))
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return Identity{}, o.err("ID token refused: the nonce does not match the sign-in")
	}
	var c oidcClaims
	if err := idt.Claims(&c); err != nil {
		return Identity{}, o.err("ID token claims are not the expected JSON")
	}
	// OIDC Core §3.1.3.7: with several audiences, azp names the client.
	if (len(idt.Audience) > 1 || c.AZP != "") && c.AZP != o.cfg.ClientID {
		return Identity{}, o.err("ID token refused: it was issued to another party (azp)")
	}
	if !oidcSubjectRE.MatchString(idt.Subject) {
		return Identity{}, o.err("ID token refused: its subject is empty, longer than 255 or not printable ASCII")
	}
	id := Identity{
		Provider: o.cfg.Name, Subject: idt.Subject, Name: cutText(c.Name, 255), AvatarURL: safeURL(c.Picture),
		Email: cutText(c.Email, 254), EmailVerified: c.verified(),
	}
	for _, l := range []string{c.PreferredUsername, c.Email, idt.Subject} {
		if id.Login = strings.TrimSpace(cutText(l, 255)); id.Login != "" {
			break
		}
	}
	return id, nil
}

// exchangeErr describes a failed token exchange without its response body.
func (o *OIDC) exchangeErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("webauth: %s: token exchange: %w", o.cfg.Name, ctx.Err())
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorCode != "" {
			return o.err("token exchange refused: %s", safeWord(re.ErrorCode))
		}
		if re.Response != nil {
			return o.err("token exchange: HTTP %d", re.Response.StatusCode)
		}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// The token endpoint's URL carries no code or secret (those travel
		// in the body and a header).
		return o.err("token exchange: request failed: %v", ue)
	}
	return o.err("token exchange failed")
}
