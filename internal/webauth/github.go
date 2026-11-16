package webauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// GitHubName is the GitHub provider's name.
const GitHubName = "github"

// GitHub.com's endpoints; GitHub Enterprise Server sets its own
// (https://HOST and https://HOST/api/v3).
const (
	DefaultGitHubWebURL = "https://github.com"
	DefaultGitHubAPIURL = "https://api.github.com"
)

// maxGitHubBody bounds the responses read from GitHub.
const maxGitHubBody = 64 << 10

// githubLoginRE is a GitHub login as the API returns it.
var githubLoginRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// GitHubConfig configures the GitHub provider: an OAuth App (ADR 0033 §4).
type GitHubConfig struct {
	ClientID     string
	ClientSecret secrets.Value
	// RedirectURL is the callback URL registered with the OAuth App:
	// <public_url>/ui/auth/callback/github.
	RedirectURL string
	// WebURL and APIURL default to GitHub.com's.
	WebURL, APIURL string
	// HTTP sends the requests; `angelia serve` passes netguard's client.
	HTTP *http.Client
}

// GitHub is the GitHub [IdentityProvider]: the OAuth web application flow
// with PKCE (S256) and no scope, then GET /user with the access token, which
// is dropped afterwards. Standard library only.
type GitHub struct {
	cfg GitHubConfig
}

var _ IdentityProvider = (*GitHub)(nil)

// NewGitHub returns the GitHub provider.
func NewGitHub(cfg GitHubConfig) (*GitHub, error) {
	if cfg.ClientID == "" || cfg.ClientSecret.IsZero() || cfg.RedirectURL == "" || cfg.HTTP == nil {
		return nil, errors.New("webauth: github: client id, client secret, redirect URL and HTTP client are required")
	}
	if cfg.WebURL == "" {
		cfg.WebURL = DefaultGitHubWebURL
	}
	if cfg.APIURL == "" {
		cfg.APIURL = DefaultGitHubAPIURL
	}
	// The OAuth requests never follow a redirect: a 3xx is the response,
	// and anything but 200 is an error. The caller's client is copied.
	client := *cfg.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTP = &client
	cfg.WebURL = strings.TrimRight(cfg.WebURL, "/")
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	return &GitHub{cfg: cfg}, nil
}

// Name implements [IdentityProvider].
func (g *GitHub) Name() string { return GitHubName }

// AuthCodeURL implements [IdentityProvider]. It asks for no scope: the
// public profile (id, login, name, avatar) needs none.
func (g *GitHub) AuthCodeURL(_ context.Context, state, codeChallenge, _ string) (string, error) {
	q := url.Values{
		"client_id":             {g.cfg.ClientID},
		"redirect_uri":          {g.cfg.RedirectURL},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	return g.cfg.WebURL + "/login/oauth/authorize?" + q.Encode(), nil
}

// ProviderError is an error of a provider. It never holds the code, the
// verifier, the nonce, the client secret, an access token or an ID token.
type ProviderError struct{ provider, msg string }

func (e *ProviderError) Error() string {
	p := e.provider
	if p == "" {
		p = GitHubName
	}
	return "webauth: " + p + ": " + e.msg
}

func providerErr(format string, a ...any) error {
	return &ProviderError{provider: GitHubName, msg: fmt.Sprintf(format, a...)}
}

// Exchange implements [IdentityProvider]. GitHub has no nonce.
func (g *GitHub) Exchange(ctx context.Context, code, codeVerifier, _ string) (Identity, error) {
	token, err := g.token(ctx, code, codeVerifier)
	if err != nil {
		return Identity{}, err
	}
	return g.user(ctx, token)
}

// token trades the code for an access token.
func (g *GitHub) token(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"client_id":     {g.cfg.ClientID},
		"client_secret": {string(g.cfg.ClientSecret.Reveal())},
		"code":          {code},
		"redirect_uri":  {g.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.WebURL+"/login/oauth/access_token", strings.NewReader(form.Encode())) //nolint:gosec // G704: web_url is the operator's config, and HTTP is the netguard client
	if err != nil {
		return "", providerErr("token request: %s", errors.Unwrap(err))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := g.do(req, "token exchange", &body); err != nil {
		return "", err
	}
	switch {
	case body.Error != "":
		// An OAuth error code such as bad_verification_code: no secret.
		return "", providerErr("token exchange refused: %s", safeWord(body.Error))
	case body.AccessToken == "":
		return "", providerErr("token exchange returned no access token")
	case body.TokenType != "" && !strings.EqualFold(body.TokenType, "bearer"):
		return "", providerErr("token exchange returned a token of type %s", safeWord(body.TokenType))
	}
	return body.AccessToken, nil
}

// user reads the signed-in user with the access token.
func (g *GitHub) user(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.APIURL+"/user", nil) //nolint:gosec // G704: api_url is the operator's config, and HTTP is the netguard client
	if err != nil {
		return Identity{}, providerErr("user request: %s", errors.Unwrap(err))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	var u struct {
		ID        int64   `json:"id"`
		Login     string  `json:"login"`
		Name      *string `json:"name"`
		AvatarURL string  `json:"avatar_url"`
	}
	if err := g.do(req, "user lookup", &u); err != nil {
		return Identity{}, err
	}
	if u.ID <= 0 || !githubLoginRE.MatchString(u.Login) {
		return Identity{}, providerErr("user lookup returned no valid id and login")
	}
	id := Identity{Provider: GitHubName, Subject: strconv.FormatInt(u.ID, 10), Login: u.Login, AvatarURL: safeURL(u.AvatarURL)}
	if u.Name != nil {
		id.Name = cutText(*u.Name, 255)
	}
	return id, nil
}

// do sends a request and decodes a 200 JSON body into v. Errors name the
// step and the status only: GitHub's error bodies are not echoed.
func (g *GitHub) do(req *http.Request, step string, v any) error {
	resp, err := g.cfg.HTTP.Do(req) //nolint:gosec // G704: see token and user
	if err != nil {
		if req.Context().Err() != nil {
			return fmt.Errorf("webauth: github: %s: %w", step, req.Context().Err())
		}
		// The cause names the method and URL, which carry no code, secret
		// or token (those travel in the body and a header), never the body.
		return providerErr("%s: request failed: %v", step, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxGitHubBody))
		return providerErr("%s: HTTP %d", step, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxGitHubBody+1))
	if err != nil || len(data) > maxGitHubBody {
		return providerErr("%s: unreadable response", step)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return providerErr("%s: response is not the expected JSON", step)
	}
	return nil
}

// safeWord keeps an OAuth error code printable: letters, digits and _ only,
// at most 64 characters.
func safeWord(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() == 64 {
			break
		}
		if r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safeURL keeps an https avatar URL of at most 2048 characters, else "".
func safeURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(s) > 2048 {
		return ""
	}
	return s
}

// cutText keeps at most n characters, without control characters.
func cutText(s string, n int) string {
	out := make([]rune, 0, min(len(s), n))
	for _, r := range s {
		if len(out) == n {
			break
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}
