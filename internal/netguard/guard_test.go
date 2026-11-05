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
// refused unless allowlisted; tests cover each range, plus the extra ranges
// the maintainer asked for on PR #127 (0/8, CGNAT, multicast, broadcast,
// NAT64, 6to4, IPv4-compatible and site-local).
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
		{"unspecified ::", "[::]"},
		{"this network 0/8", "0.1.2.3"},
		{"CGNAT 100.64/10 low", "100.64.0.1"},
		{"CGNAT 100.64/10 high", "100.127.255.254"},
		{"CGNAT metadata 100.100.100.200", "100.100.100.200"},
		{"multicast 224/4 low", "224.0.0.1"},
		{"multicast 224/4 high", "239.255.255.250"},
		{"multicast ff00::/8", "[ff02::1]"},
		{"multicast ff00::/8 global", "[ff0e::1]"},
		{"broadcast 255.255.255.255", "255.255.255.255"},
		{"NAT64 64:ff9b::/96 public", "[64:ff9b::808:808]"},
		{"NAT64 64:ff9b::/96 metadata", "[64:ff9b::a9fe:a9fe]"},
		{"6to4 2002::/16 public", "[2002:808:808::1]"},
		{"6to4 2002::/16 loopback", "[2002:7f00:1::1]"},
		{"ipv4-compatible ::/96", "[::808:808]"},
		{"ipv4-compatible ::/96 metadata", "[::a9fe:a9fe]"},
		{"site-local fec0::/10", "[fec0::1]"},
		{"site-local fec0::/10 high", "[feff:ffff::1]"},
		{"ipv4-mapped CGNAT metadata", "[::ffff:100.100.100.200]"},
		// Added by #108 (ADR 0023 section 5).
		{"IETF protocol assignments 192.0.0.0/24", "192.0.0.170"},
		{"IETF protocol assignments 192.0.0.0/24 high", "192.0.0.255"},
		{"benchmarking 198.18.0.0/15 low", "198.18.0.1"},
		{"benchmarking 198.18.0.0/15 high", "198.19.255.254"},
		{"reserved 240.0.0.0/4", "240.0.0.1"},
		{"reserved 240.0.0.0/4 high", "255.255.255.254"},
		{"Teredo 2001::/32", "[2001:0:4136:e378:8000:63bf:3fff:fdd2]"},
		{"Teredo 2001::/32 high", "[2001:0:ffff:ffff::1]"},
		{"NAT64 local-use 64:ff9b:1::/48", "[64:ff9b:1::a00:1]"},
		{"NAT64 local-use 64:ff9b:1::/48 high", "[64:ff9b:1:ffff::1]"},
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
		netip.MustParsePrefix("100.64.0.0/10"),
	}
	g := NewGuard(allow, fakeResolver{})
	for _, host := range []string{"127.0.0.1", "10.9.8.7", "169.254.169.254", "[::1]", "[fd00::1]", "[::ffff:10.1.1.1]", "100.100.100.200"} {
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

// S7: non-canonical IPv4 forms fail closed; other spellings of blocked
// addresses are still blocked.
func TestGuardAddressSpellings(t *testing.T) {
	numeric := []string{"2130706433", "0x7f000001", "017700000001", "127.1", "0177.0.0.1", "0x7f.0.0.1"}
	table := fakeResolver{"localhost.": addrs("127.0.0.1")}
	for _, h := range numeric {
		table[h] = addrs("192.0.2.1") // even a resolver that says "public" is not asked
	}
	g := NewGuard(nil, table)
	for _, host := range numeric {
		if err := g.CheckURL(t.Context(), &url.URL{Scheme: "http", Host: host}); err == nil {
			t.Errorf("%s: want error", host)
		}
	}
	for _, raw := range []string{"http://[fe80::1%25eth0]/", "http://[::ffff:7f00:1]/", "http://localhost./"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.CheckURL(t.Context(), u); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: got %v, want ErrBlocked", raw, err)
		}
	}
	// Ordinary hostnames with hex-looking labels are not caught.
	ok := NewGuard(nil, fakeResolver{"cafe.example": addrs("192.0.2.1"), "v2.dead.beef": addrs("192.0.2.1")})
	for _, host := range []string{"cafe.example", "v2.dead.beef"} {
		if err := ok.CheckURL(t.Context(), &url.URL{Scheme: "http", Host: host}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
}
