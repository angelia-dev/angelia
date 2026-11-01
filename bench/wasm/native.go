// Package wasm is benchmark 4 of architecture.md section 6: the overhead of
// a WASM source plugin per List call, against a native Go implementation of
// the same crates.io source. See overhead_test.go.
package wasm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// NativeCrates is a Go port of bench/plugins/crates/src/lib.rs, request for
// request, so that the benchmark compares the plugin runtime and not two
// algorithms. It is not the built-in crates source.
type NativeCrates struct {
	base   string
	client *http.Client
}

// NewNativeCrates returns the native crates source.
func NewNativeCrates(baseURL string, c *http.Client) *NativeCrates {
	return &NativeCrates{base: strings.TrimRight(baseURL, "/"), client: c}
}

// Kind implements source.Source.
func (n *NativeCrates) Kind() string { return "crates" }

// Validate implements source.Source.
func (n *NativeCrates) Validate(t source.Target) error {
	if t.Kind != "crates" || t.Coordinates["name"] == "" {
		return fmt.Errorf("%w: crates needs a name", source.ErrInvalidTarget)
	}
	return nil
}

type page struct {
	Versions []struct {
		Num         string  `json:"num"`
		CreatedAt   string  `json:"created_at"`
		Yanked      bool    `json:"yanked"`
		Checksum    *string `json:"checksum"`
		License     *string `json:"license"`
		RustVersion *string `json:"rust_version"`
		DLPath      *string `json:"dl_path"`
	} `json:"versions"`
	Meta struct {
		NextPage *string `json:"next_page"`
	} `json:"meta"`
}

// List implements source.Source.
func (n *NativeCrates) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	name := t.Coordinates["name"]
	first := n.base + "/api/v1/crates/" + name + "/versions"
	var res source.Result
	url := first
	for p := 0; p < 50; p++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return source.Result{}, err
		}
		req.Header.Set("Accept", "application/json")
		if p == 0 && st.ETag != "" {
			req.Header.Set("If-None-Match", st.ETag)
		}
		resp, err := n.client.Do(req) //nolint:gosec // G704: benchmark client, injected.
		if err != nil {
			return source.Result{}, fmt.Errorf("crates %s: %w", name, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return source.Result{}, err
		}
		if p == 0 {
			if resp.StatusCode == http.StatusNotModified {
				st.ETag = coalesce(resp.Header.Get("ETag"), st.ETag)
				return source.Result{NotModified: true, State: st}, nil
			}
			res.State = source.FetchState{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}
		}
		if resp.StatusCode != http.StatusOK {
			return source.Result{}, fmt.Errorf("crates %s: status %d", name, resp.StatusCode)
		}
		var pg page
		if err := json.Unmarshal(body, &pg); err != nil {
			return source.Result{}, err
		}
		for _, v := range pg.Versions {
			pub, err := time.Parse(time.RFC3339Nano, v.CreatedAt)
			if err != nil {
				return source.Result{}, err
			}
			md := map[string]any{}
			put := func(k string, v *string) {
				if v != nil {
					md[k] = *v
				}
			}
			put("checksum", v.Checksum)
			put("license", v.License)
			put("rust_version", v.RustVersion)
			if v.DLPath != nil {
				md["download_url"] = n.base + *v.DLPath
			}
			res.Versions = append(res.Versions, source.Version{
				Raw:         v.Num,
				PublishedAt: pub.UTC(),
				Yanked:      v.Yanked,
				URL:         "https://crates.io/crates/" + name + "/" + v.Num,
				Metadata:    md,
			})
		}
		if pg.Meta.NextPage == nil || *pg.Meta.NextPage == "" {
			return res, nil
		}
		url = first + *pg.Meta.NextPage
	}
	return source.Result{}, fmt.Errorf("crates %s: too many pages", name)
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
