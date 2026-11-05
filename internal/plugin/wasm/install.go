package wasm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Credential is a token the operator configured when installing a plugin,
// bound to one of its allowed hosts (ADR 0011, amendments of #145 and
// #184). The host adds it as the Authorization header (or Header) of every
// hop to that host and port, over https only. It is never in the plugin's
// config map. The host does not hand it to the plugin, but an upstream
// that reflects the header can: redacting such echoes is best effort, so
// bind a credential only to a host that does not reflect it.
type Credential struct {
	// Host is "host" or "host:port": a hostname or IP literal in the merged
	// allowed_hosts, optionally with the port (an IPv6 literal with a port
	// is "[addr]:port"). Without a port the credential is sent on port 443
	// only.
	Host string
	// Header is the request header that carries the token. Empty means
	// Authorization. Otherwise it must be a header the netguard client
	// counts as a credential (PRIVATE-TOKEN, or one of its
	// Config.ExtraAuthHeaders).
	Header string
	// Scheme, if set, is written before the token: "Bearer" gives
	// "Authorization: Bearer <token>". Empty sends the token as is. Only
	// with the Authorization header.
	Scheme string
	// Token returns the token. It is called for every hop to Host; it is
	// the seam for the secret store (#75). A rotated value is only picked
	// up if the function reads it at call time: the instance config reads
	// token_env once, at load. Its error must not contain the token.
	Token func(ctx context.Context) (string, error)
}

// HostGrant is one entry of a plugin's merged allowed_hosts, as Hosts
// shows it when the plugin is installed.
type HostGrant struct {
	Host string `json:"host"`
	// Manifest and Operator say who allowed the host; both may be true.
	Manifest bool `json:"manifest"`
	Operator bool `json:"operator"`
	// CredentialPorts lists the ports on which the host injects a
	// credential for this host, sorted. Empty: none.
	CredentialPorts []string `json:"credential_ports,omitempty"`
}

var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)

// errCredential is wrapped when a credential cannot be attached to a hop.
var errCredential = errors.New("wasm: credential")

// minTokenLen is the shortest token injected. A shorter one is a
// misconfiguration, and redacting it would mangle responses.
const minTokenLen = 8

// redacted replaces an injected token echoed back to the plugin.
const redacted = "[REDACTED]"

// parseCredentialHost splits "host" or "host:port" into the normalised
// host and the port, 443 when absent.
func parseCredentialHost(s string) (host, port string, err error) {
	host, port = s, "443"
	if _, perr := netip.ParseAddr(s); perr != nil && (strings.HasPrefix(s, "[") || strings.Count(s, ":") == 1) {
		if host, port, err = net.SplitHostPort(s); err != nil {
			return "", "", fmt.Errorf("%q is not host or host:port", s)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", "", fmt.Errorf("%q: invalid port", s)
		}
	}
	h, err := normAllowedHost(host)
	if err != nil {
		return "", "", err
	}
	return h, port, nil
}

// install merges the manifest's and the operator's allowed hosts and
// checks the credentials against them.
func (s *Source) install(m Manifest, opts Options) error {
	s.allowed = map[string]bool{}
	s.grants = map[string]*HostGrant{}
	grant := func(h string) *HostGrant {
		g := s.grants[h]
		if g == nil {
			g = &HostGrant{Host: h}
			s.grants[h] = g
			s.allowed[h] = true
		}
		return g
	}
	for _, h := range m.AllowedHosts {
		n, _ := normAllowedHost(h) // checked by Validate
		grant(n).Manifest = true
	}
	for _, h := range opts.AllowedHosts {
		n, err := normAllowedHost(h)
		if err != nil {
			return fmt.Errorf("wasm %s: operator allowed_hosts: %w", m.Kind, err)
		}
		grant(n).Operator = true
	}
	s.creds = make(map[string]Credential, len(opts.Credentials))
	for _, c := range opts.Credentials {
		h, port, err := parseCredentialHost(c.Host)
		if err != nil {
			return fmt.Errorf("wasm %s: credential: %w", m.Kind, err)
		}
		key := net.JoinHostPort(h, port)
		switch {
		case !s.allowed[h]:
			return fmt.Errorf("wasm %s: credential for %q: host is not in allowed_hosts", m.Kind, h)
		case s.creds[key].Token != nil:
			return fmt.Errorf("wasm %s: credential for %q given twice", m.Kind, key)
		case c.Token == nil:
			return fmt.Errorf("wasm %s: credential for %q has no Token", m.Kind, key)
		case c.Scheme != "" && !schemeRE.MatchString(c.Scheme):
			return fmt.Errorf("wasm %s: credential for %q: scheme must match %s", m.Kind, key, schemeRE)
		}
		hdr, err := s.credentialHeader(c.Header)
		if err != nil {
			return fmt.Errorf("wasm %s: credential for %q: %w", m.Kind, key, err)
		}
		if c.Scheme != "" && hdr != "Authorization" {
			return fmt.Errorf("wasm %s: credential for %q: scheme applies to the Authorization header only", m.Kind, key)
		}
		c.Header = hdr
		s.creds[key] = c
		g := s.grants[h]
		g.CredentialPorts = append(g.CredentialPorts, port)
		sort.Strings(g.CredentialPorts)
	}
	return nil
}

// credentialHeader returns the canonical name of the header a credential
// is injected in: Authorization by default, or a header netguard counts as
// a credential.
func (s *Source) credentialHeader(h string) (string, error) {
	if h == "" {
		return "Authorization", nil
	}
	if !validHeader(h, "") {
		return "", fmt.Errorf("header %q is not a valid header name", h)
	}
	if !s.isCredentialHeader(h) {
		return "", fmt.Errorf("header %q is not a credential header for the netguard client (Authorization, PRIVATE-TOKEN or one of its ExtraAuthHeaders)", h)
	}
	return http.CanonicalHeaderKey(h), nil
}

// isCredentialHeader reports whether the netguard client counts the header
// name as a credential, which makes a request authenticated for its
// limiter. These headers are reserved for the credentials the host
// injects.
func (s *Source) isCredentialHeader(name string) bool {
	return s.credHdrs.IsCredentialHeader(name)
}

// Hosts returns the merged allowed_hosts, sorted, with where each entry
// comes from and the ports a credential is bound to. It never holds a
// token.
func (s *Source) Hosts() []HostGrant {
	out := make([]HostGrant, 0, len(s.grants))
	for _, g := range s.grants {
		c := *g
		c.CredentialPorts = append([]string(nil), g.CredentialPorts...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// hopKey is the credential key of a hop: host and effective port.
func hopKey(r *http.Request) string {
	port := r.URL.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(r.URL.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(normHost(r.URL.Hostname()), port)
}

// inject returns the request to send for one hop and whether it carries a
// credential. On a hop to a bound host and port, the host adds the
// credential header (the plugin cannot set a credential header: newRequest
// refuses it). Other hops are left as the plugin built them, so a
// credential never follows a redirect to another host or port. The token is recorded in the call's state so that
// echoes are scrubbed.
//
// On a credentialed hop, the plugin's Accept-Encoding is removed, so that
// net/http negotiates compression and decompresses the body before it is
// scrubbed, and a Range or If-Range header is refused: partial responses
// would let a plugin read an echoed token in pieces that each escape
// redaction. Refusing rather than stripping keeps the plugin from
// misreading a full response as the range it asked for.
func (t *allowlistTransport) inject(r *http.Request) (*http.Request, bool, error) {
	key := hopKey(r)
	c, ok := t.creds[key]
	if !ok {
		return r, false, nil
	}
	if !strings.EqualFold(r.URL.Scheme, "https") {
		return nil, false, fmt.Errorf("%w for %q is only sent over https", errCredential, key)
	}
	if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
		return nil, false, fmt.Errorf("%w for %q: Range requests are not allowed with a credential", errCredential, key)
	}
	tok, err := c.Token(r.Context())
	if err != nil {
		return nil, false, fmt.Errorf("%w for %q is unavailable: %w", errCredential, key, err)
	}
	v := tok
	if c.Scheme != "" {
		v = c.Scheme + " " + tok
	}
	if len(strings.TrimSpace(tok)) < minTokenLen || !validHeader(c.Header, v) {
		return nil, false, fmt.Errorf("%w for %q is shorter than %d bytes or not a valid header value", errCredential, key, minTokenLen)
	}
	if cs, ok := r.Context().Value(callKey{}).(*callState); ok {
		cs.addSecret(tok)
	}
	out := r.Clone(r.Context())
	out.Header.Del("Accept-Encoding")
	out.Header.Set(c.Header, v)
	return out, true, nil
}

// checkEncoding refuses a credentialed response whose body is still
// encoded (a transport that does not decompress, or a server that ignored
// the negotiation): its bytes cannot be scrubbed. Every Content-Encoding
// value is checked, since a server can send several headers and the
// transport only acts on the first; net/http deletes the header after it
// decompresses, so a decoded response has none.
func checkEncoding(resp *http.Response) error {
	for _, v := range resp.Header.Values("Content-Encoding") {
		for ce := range strings.SplitSeq(v, ",") {
			if ce = strings.TrimSpace(ce); ce != "" && !strings.EqualFold(ce, "identity") {
				_ = resp.Body.Close()
				return fmt.Errorf("%w: response with Content-Encoding %q cannot be redacted", errCredential, ce)
			}
		}
	}
	return nil
}

// addSecret records a token injected during the call. Secrets are per
// call: a token the bound host stores and another host reflects in a later
// call is not scrubbed.
func (c *callState) addSecret(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.secrets {
		if s == tok {
			return
		}
	}
	c.secrets = append(c.secrets, tok)
}

// scrub replaces every token injected during the call in a response or an
// error message before the plugin sees it, drops headers whose name holds
// one, and drops Content-Length once the body changed. It is best effort:
// it does not catch a transformed echo (encoded, split, case-changed).
func (c *callState) scrub(resp *wireHTTPResponse) {
	c.mu.Lock()
	secrets := c.secrets
	c.mu.Unlock()
	rep := func(s, sec string) string { return strings.ReplaceAll(s, sec, redacted) }
	for _, sec := range secrets {
		if b := bytes.ReplaceAll(resp.Body, []byte(sec), []byte(redacted)); !bytes.Equal(b, resp.Body) {
			resp.Body = b
			delete(resp.Headers, "content-length")
		}
		lower := strings.ToLower(sec)
		for k, vs := range resp.Headers {
			if strings.Contains(k, lower) {
				delete(resp.Headers, k)
				continue
			}
			for i, v := range vs {
				vs[i] = rep(v, sec)
			}
		}
		if resp.Error != nil {
			resp.Error.Message = rep(resp.Error.Message, sec)
		}
		resp.Validators.ETag = rep(resp.Validators.ETag, sec)
		resp.Validators.LastModified = rep(resp.Validators.LastModified, sec)
	}
}
