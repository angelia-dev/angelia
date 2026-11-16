package webauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// Synthetic credentials of the fake GitHub.
const (
	fakeClientID     = "Ov23liTESTCLIENT"
	fakeClientSecret = "test-client-secret-0001"
	fakeCode         = "test-code-0001"
	fakeAccessToken  = "gho_testaccesstoken0001"
)

// fakeGitHub is an httptest GitHub: the OAuth token endpoint and GET /user.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	verifier string // the code_verifier of the last exchange
	// tokenBody and userBody, when set, replace the answers; *Status too.
	tokenStatus, userStatus int
	tokenBody, userBody     string
	userCalls               int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/login/oauth/access_token", f.token)
	mux.HandleFunc("/api/v3/user", f.user)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodPost || r.Header.Get("Accept") != "application/json" ||
		r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		f.t.Errorf("token request: %s %v", r.Method, r.Header)
	}
	if err := r.ParseForm(); err != nil {
		f.t.Error(err)
	}
	if r.URL.RawQuery != "" {
		f.t.Errorf("token request has a query: %s", r.URL.RawQuery)
	}
	f.verifier = r.PostForm.Get("code_verifier")
	if f.tokenStatus != 0 {
		w.WriteHeader(f.tokenStatus)
	}
	if f.tokenBody != "" {
		_, _ = w.Write([]byte(f.tokenBody))
		return
	}
	if r.PostForm.Get("client_id") != fakeClientID || r.PostForm.Get("client_secret") != fakeClientSecret ||
		r.PostForm.Get("redirect_uri") != "https://angelia.example.com/ui/auth/callback/github" {
		_, _ = w.Write([]byte(`{"error":"incorrect_client_credentials"}`))
		return
	}
	if r.PostForm.Get("code") != fakeCode || len(f.verifier) != 43 {
		_, _ = w.Write([]byte(`{"error":"bad_verification_code","error_description":"The code passed is incorrect"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fakeAccessToken, "token_type": "bearer", "scope": ""})
}

func (f *fakeGitHub) user(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls++
	if r.Header.Get("Authorization") != "Bearer "+fakeAccessToken || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.userStatus != 0 {
		w.WriteHeader(f.userStatus)
	}
	if f.userBody != "" {
		_, _ = w.Write([]byte(f.userBody))
		return
	}
	_, _ = w.Write([]byte(`{"login":"MMornati","id":139323,"name":"Marco Mornati","avatar_url":"https://avatars.githubusercontent.com/u/139323?v=4","email":null}`))
}

func (f *fakeGitHub) provider(t *testing.T) *GitHub {
	t.Helper()
	g, err := NewGitHub(GitHubConfig{
		ClientID: fakeClientID, ClientSecret: secrets.NewValueString(fakeClientSecret),
		RedirectURL: "https://angelia.example.com/ui/auth/callback/github",
		WebURL:      f.srv.URL + "/", APIURL: f.srv.URL + "/api/v3", HTTP: f.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGitHubAuthCodeURL(t *testing.T) {
	g, err := NewGitHub(GitHubConfig{
		ClientID: fakeClientID, ClientSecret: secrets.NewValueString(fakeClientSecret),
		RedirectURL: "https://angelia.example.com/ui/auth/callback/github", HTTP: http.DefaultClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(g.AuthCodeURL("st", "ch"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "https://github.com/login/oauth/authorize" || q.Get("client_id") != fakeClientID ||
		q.Get("state") != "st" || q.Get("code_challenge") != "ch" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "https://angelia.example.com/ui/auth/callback/github" || q.Has("scope") ||
		strings.Contains(u.RawQuery, fakeClientSecret) {
		t.Fatalf("%s", u)
	}
	if g.Name() != "github" {
		t.Fatal(g.Name())
	}
	if _, err := NewGitHub(GitHubConfig{ClientID: "x"}); err == nil {
		t.Fatal("incomplete config accepted")
	}
}

func TestGitHubExchange(t *testing.T) {
	f := newFakeGitHub(t)
	g := f.provider(t)
	verifier := strings.Repeat("v", 43)
	id, err := g.Exchange(t.Context(), fakeCode, verifier)
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Provider: "github", Subject: "139323", Login: "MMornati", Name: "Marco Mornati", AvatarURL: "https://avatars.githubusercontent.com/u/139323?v=4"}
	if id != want || f.verifier != verifier || id.PrincipalID() != "github:139323" {
		t.Fatalf("%+v (verifier %q)", id, f.verifier)
	}
}

// Every failure is a ProviderError (or the context's), and no message holds
// the code, the verifier, the client secret or the access token.
func TestGitHubExchangeFailures(t *testing.T) {
	verifier := strings.Repeat("v", 43)
	for name, tc := range map[string]struct {
		code                    string
		tokenStatus, userStatus int
		tokenBody, userBody     string
		want                    string
	}{
		"bad code":         {code: "other", want: "bad_verification_code"},
		"token 500":        {code: fakeCode, tokenStatus: 500, tokenBody: `{"access_token":"` + fakeAccessToken + `"}`, want: "HTTP 500"},
		"token not json":   {code: fakeCode, tokenBody: "<html>" + fakeAccessToken, want: "not the expected JSON"},
		"no token":         {code: fakeCode, tokenBody: `{"token_type":"bearer"}`, want: "no access token"}, //nolint:gosec // G101: a synthetic fixture
		"token type":       {code: fakeCode, tokenBody: `{"access_token":"x","token_type":"mac"}`, want: "type mac"},
		"error with junk":  {code: fakeCode, tokenBody: `{"error":"bad <b>` + fakeAccessToken + `"}`, want: "refused: badbgho_"},
		"user 401":         {code: fakeCode, userStatus: 401, userBody: `{}`, want: "user lookup: HTTP 401"},
		"user no id":       {code: fakeCode, userBody: `{"login":"x"}`, want: "no valid id"},
		"user bad login":   {code: fakeCode, userBody: `{"login":"a b","id":5}`, want: "no valid id"},
		"user big body":    {code: fakeCode, userBody: `{"login":"` + strings.Repeat("a", 70000) + `"}`, want: "unreadable"},
		"user not json":    {code: fakeCode, userBody: `nope`, want: "not the expected JSON"},
		"token huge":       {code: fakeCode, tokenBody: strings.Repeat(" ", 70000), want: "unreadable"},
		"token 404 secret": {code: fakeCode, tokenStatus: 404, tokenBody: fakeClientSecret, want: "HTTP 404"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.tokenStatus, f.userStatus, f.tokenBody, f.userBody = tc.tokenStatus, tc.userStatus, tc.tokenBody, tc.userBody
			_, err := f.provider(t).Exchange(t.Context(), tc.code, verifier)
			var pe *ProviderError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, s := range []string{fakeCode, verifier, fakeClientSecret, fakeAccessToken} {
				if strings.Contains(err.Error(), s) && (name != "error with junk" || s != fakeAccessToken) {
					t.Errorf("the error holds %q", s)
				}
			}
		})
	}
	// A closed server and a cancelled context.
	f := newFakeGitHub(t)
	g := f.provider(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := g.Exchange(ctx, fakeCode, verifier); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	f.srv.Close()
	var pe *ProviderError
	_, err := g.Exchange(t.Context(), fakeCode, verifier)
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("closed server: %v", err)
	}
	// The transport error keeps its cause, the URL and never a credential.
	if !strings.Contains(err.Error(), f.srv.URL+"/login/oauth/access_token") || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("closed server: the cause is lost: %v", err)
	}
	for _, s := range []string{fakeCode, verifier, fakeClientSecret, fakeAccessToken} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("the error holds %q", s)
		}
	}
}

// The OAuth requests never follow a redirect: a 307 would resend the form,
// client secret and code included, to wherever it points.
func TestGitHubNoRedirect(t *testing.T) {
	var mu sync.Mutex
	followed := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		followed++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"bearer"}`))
	}))
	t.Cleanup(target.Close)
	for _, path := range []string{"/login/oauth/access_token", "/api/v3/user"} {
		t.Run(path, func(t *testing.T) {
			f := newFakeGitHub(t)
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					http.Redirect(w, r, target.URL+path, http.StatusTemporaryRedirect)
					return
				}
				f.srv.Config.Handler.ServeHTTP(w, r)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			client := srv.Client()
			g, err := NewGitHub(GitHubConfig{
				ClientID: fakeClientID, ClientSecret: secrets.NewValueString(fakeClientSecret),
				RedirectURL: "https://angelia.example.com/ui/auth/callback/github",
				WebURL:      srv.URL, APIURL: srv.URL + "/api/v3", HTTP: client,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = g.Exchange(t.Context(), fakeCode, strings.Repeat("v", 43))
			var pe *ProviderError
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), "HTTP 307") {
				t.Fatalf("err = %v, want HTTP 307", err)
			}
			if client.CheckRedirect != nil {
				t.Fatal("the caller's client was changed")
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if followed != 0 {
		t.Fatalf("followed %d redirects", followed)
	}
}

func TestGitHubUserFieldsAreCleaned(t *testing.T) {
	f := newFakeGitHub(t)
	f.userBody = `{"login":"octocat","id":583231,"name":"Octo\u0000cat` + strings.Repeat("x", 300) + `","avatar_url":"javascript:alert(1)"}`
	id, err := f.provider(t).Exchange(t.Context(), fakeCode, strings.Repeat("v", 43))
	if err != nil {
		t.Fatal(err)
	}
	if id.AvatarURL != "" || len([]rune(id.Name)) != 255 || strings.ContainsRune(id.Name, 0) {
		t.Fatalf("%+v", id)
	}
	f.userBody = `{"login":"octocat","id":583231,"name":null,"avatar_url":"https://avatars.example/u"}`
	id, err = f.provider(t).Exchange(t.Context(), fakeCode, strings.Repeat("v", 43))
	if err != nil || id.Name != "" || id.AvatarURL != "https://avatars.example/u" {
		t.Fatalf("%+v %v", id, err)
	}
}
