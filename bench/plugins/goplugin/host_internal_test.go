package goplugin

import (
	"testing"
)

func TestNewRequestRefusals(t *testing.T) {
	for name, r := range map[string]*HTTPRequest{
		"method":      {Method: "DELETE", URL: "https://crates.io/"},
		"reserved":    {URL: "https://crates.io/", Headers: map[string]string{"host": "evil.test"}},
		"duplicate":   {URL: "https://crates.io/", Headers: map[string]string{"x-a": "1", "X-A": "2"}},
		"credentials": {URL: "https://user@crates.io/"}, // any userinfo is refused
	} {
		if _, err := newRequest(t.Context(), r); err == nil {
			t.Errorf("%s: request accepted", name)
		}
	}
	req, err := newRequest(t.Context(), &HTTPRequest{URL: "https://crates.io/", Validators: &Validators{ETag: `"e"`, LastModified: "lm"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "GET" || req.Header.Get("If-None-Match") != `"e"` || req.Header.Get("If-Modified-Since") != "lm" {
		t.Errorf("request = %s %v", req.Method, req.Header)
	}
}
