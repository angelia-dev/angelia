package netguard

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"testing"
)

// fakeResolver answers from a fixed table and never touches the network.
type fakeResolver map[string][]netip.Addr

func (r fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := r[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

// AC: requests to 127.0.0.1, 10.0.0.0/8, 169.254.0.0/16, ::1, fc00::/7 are
// refused unless allowlisted; tests cover each range.
func TestGuardDefaultDenyRanges(t *testing.T) {
	cases := []struct {
		name, host string
	}{
		{"loopback 127.0.0.1", "127.0.0.1"},
		{"loopback 127/8", "127.1.2.3"},
		{"private 10/8 low", "10.0.0.1"},
		{"private 10/8 high", "10.255.255.254"},
		{"link-local 169.254/16 metadata", "169.254.169.254"},
		{"link-local 169.254/16", "169.254.0.1"},
		{"loopback ::1", "[::1]"},
		{"unique-local fc00::/7 (fc)", "[fc00::1]"},
		{"unique-local fc00::/7 (fd)", "[fdff:ffff::1]"},
		{"private 172.16/12", "172.16.0.1"},
		{"private 192.168/16", "192.168.1.1"},
		{"link-local fe80::/10", "[fe80::1]"},
		{"ipv4-mapped loopback", "[::ffff:127.0.0.1]"},
		{"unspecified", "0.0.0.0"},
	}
	g := NewGuard(nil, fakeResolver{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := &url.URL{Scheme: "https", Host: tc.host, Path: "/"}
			err := g.CheckURL(t.Context(), u)
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("CheckURL(%s): got %v, want ErrBlocked", u, err)
			}
			var be *BlockedError
			if !errors.As(err, &be) || !be.Addr.IsValid() {
				t.Fatalf("error %v does not carry the blocked address", err)
			}
		})
	}
}

func TestGuardAllowlistOptsIn(t *testing.T) {
	allow := []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fc00::/7"),
	}
	g := NewGuard(allow, fakeResolver{})
	for _, host := range []string{"127.0.0.1", "10.9.8.7", "169.254.169.254", "[::1]", "[fd00::1]", "[::ffff:10.1.1.1]"} {
		u := &url.URL{Scheme: "http", Host: host}
		if err := g.CheckURL(t.Context(), u); err != nil {
			t.Errorf("CheckURL(%s) with allowlist: %v", host, err)
		}
	}
	// The allowlist is exact: 127.0.0.2 is not in 127.0.0.1/32.
	if err := g.CheckURL(t.Context(), &url.URL{Scheme: "http", Host: "127.0.0.2"}); !errors.Is(err, ErrBlocked) {
		t.Errorf("127.0.0.2: got %v, want ErrBlocked", err)
	}
}

func TestGuardPublicAddressesAllowed(t *testing.T) {
	g := NewGuard(nil, fakeResolver{"registry.example": addrs("192.0.2.10", "2001:db8::10")})
	for _, raw := range []string{"https://192.0.2.1/x", "https://[2001:db8::1]:8443/", "https://registry.example/simple"} {
		u, _ := url.Parse(raw)
		if err := g.CheckURL(t.Context(), u); err != nil {
			t.Errorf("CheckURL(%s): %v", raw, err)
		}
	}
}

func TestGuardResolvedHostnames(t *testing.T) {
	g := NewGuard(nil, fakeResolver{
		"localhost":        addrs("127.0.0.1", "::1"),
		"mixed.example":    addrs("192.0.2.1", "10.0.0.5"),
		"internal.example": addrs("fd00::5"),
	})
	for _, host := range []string{"localhost", "mixed.example", "internal.example"} {
		err := g.CheckURL(t.Context(), &url.URL{Scheme: "https", Host: host})
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: got %v, want ErrBlocked", host, err)
		}
	}
	// Resolution failure fails closed.
	if err := g.CheckURL(t.Context(), &url.URL{Scheme: "https", Host: "unknown.example"}); err == nil {
		t.Error("unresolvable host: want error")
	}
}

func TestGuardRejectsBadURLs(t *testing.T) {
	g := NewGuard(nil, fakeResolver{})
	for _, raw := range []string{"ftp://192.0.2.1/", "file:///etc/passwd", "https:///nohost"} {
		u, _ := url.Parse(raw)
		if err := g.CheckURL(t.Context(), u); err == nil {
			t.Errorf("CheckURL(%s): want error", raw)
		}
	}
}
