package main

import (
	"strings"
	"testing"
)

func TestInvalidServerURLNeverEchoesInput(t *testing.T) {
	for _, in := range []string{
		"htp://u:p@ss@host", "http://u:s3cr@t@host:99999x", "ftp://u:hunter2@host", "://u:hunter2@",
	} {
		_, err := resolveConnection(in, "", envMap(map[string]string{"XDG_CONFIG_HOME": t.TempDir()}))
		if err == nil {
			t.Errorf("%q accepted", in)
			continue
		}
		for _, leak := range []string{"ss@", "s3cr", "t@host", "hunter2", "u:p", "99999"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%q: error %q leaks %q", in, err, leak)
			}
		}
	}
}

func TestCheckTokenTransport(t *testing.T) {
	cases := []struct {
		server, token string
		insecure      bool
		refuse        bool
	}{
		{"http://localhost:8080", "t", false, false},
		{"http://LocalHost:8080", "t", false, false},
		{"http://127.0.0.1:8080", "t", false, false},
		{"http://127.5.6.7", "t", false, false},
		{"http://[::1]:8080", "t", false, false},
		{"https://angelia.example.com", "t", false, false},
		{"http://angelia.example.com", "", false, false},
		{"http://angelia.example.com", "t", false, true},
		{"http://10.0.0.5:8080", "t", false, true},
		{"http://localhost.example.com", "t", false, true},
		{"http://localhost.:8080", "t", false, true},
		{"http://api.localhost:8080", "t", false, true},
		{"http://angelia.example.com", "t", true, false},
	}
	for _, c := range cases {
		err := checkTokenTransport(Connection{Server: c.server, Token: c.token}, c.insecure)
		if (err != nil) != c.refuse {
			t.Errorf("%s token=%q insecure=%v: err = %v", c.server, c.token, c.insecure, err)
		}
		if err != nil && !strings.Contains(err.Error(), "--insecure-http") {
			t.Errorf("error %q", err)
		}
	}
}

func TestCommandRefusesTokenOverRemoteHTTP(t *testing.T) {
	isolate(t)
	_, _, err := runCLI(t, "watch", "list", "--server", "http://angelia.invalid", "--token", testToken)
	if err == nil || !strings.Contains(err.Error(), "--insecure-http") {
		t.Fatalf("err = %v", err)
	}
	// With the flag the check passes; the request then fails to resolve.
	_, _, err = runCLI(t, "watch", "list", "--server", "http://angelia.invalid", "--token", testToken, "--insecure-http")
	if err == nil || strings.Contains(err.Error(), "refusing") || strings.Contains(err.Error(), testToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeDocuments(t *testing.T) {
	docs, err := decodeDocuments([]byte("a: 1\n---\n---\nb: 2\n---\n- x\n"))
	if err != nil || len(docs) != 3 {
		t.Fatalf("%v, %v", docs, err)
	}
	if _, err := toJSONObject([]byte("a: 1\n---\nb: 2\n")); err == nil {
		t.Error("two documents accepted as one resource")
	}
	if _, err := decodeDocuments([]byte("a: [")); err == nil {
		t.Error("bad YAML accepted")
	}
}
