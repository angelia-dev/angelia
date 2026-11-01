// Package wasmtest holds test helpers for the WASM plugin host: a fake
// crates.io API with synthetic data, used by the host's tests and by
// benchmark 4 (bench/wasm).
package wasmtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CratesUpstream is a fake crates.io API serving
// GET /api/v1/crates/{name}/versions with seek pagination
// (meta.next_page = "?per_page=N&seek=K"), an ETag per page and 304s on
// If-None-Match. All data is synthetic.
type CratesUpstream struct {
	pageSize int

	mu       sync.Mutex
	versions map[string]int // crate name -> number of versions
	rev      int
	auth     []string // Authorization header of each request
	requests int
}

// NewCratesUpstream serves the given crates, each with n versions
// "1.0.<n-1>" … "1.0.0" (newest first), pageSize versions per page.
func NewCratesUpstream(pageSize int, crates map[string]int) *CratesUpstream {
	v := make(map[string]int, len(crates))
	for k, n := range crates {
		v[k] = n
	}
	return &CratesUpstream{pageSize: pageSize, versions: v}
}

// Change publishes one more version of every crate, which changes every ETag.
func (u *CratesUpstream) Change() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rev++
	for k := range u.versions {
		u.versions[k]++
	}
}

// Authorizations returns the Authorization header of every request so far.
func (u *CratesUpstream) Authorizations() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auth...)
}

// Requests returns the number of requests served.
func (u *CratesUpstream) Requests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests
}

// WantRaw returns the Raw values List must return for a crate with n
// versions.
func WantRaw(n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = versionName(n - 1 - i)
	}
	return out
}

func versionName(i int) string { return "1.0." + strconv.Itoa(i) }

type crateVersion struct {
	Num         string  `json:"num"`
	CreatedAt   string  `json:"created_at"`
	Yanked      bool    `json:"yanked"`
	Checksum    string  `json:"checksum"`
	License     string  `json:"license"`
	RustVersion *string `json:"rust_version"`
	DLPath      string  `json:"dl_path"`
	Crate       string  `json:"crate"`
}

type versionsPage struct {
	Versions []crateVersion `json:"versions"`
	Meta     struct {
		Total    int     `json:"total"`
		NextPage *string `json:"next_page"`
	} `json:"meta"`
}

var epoch = time.Date(2024, 1, 2, 3, 4, 5, 123456000, time.UTC)

// ServeHTTP implements http.Handler.
func (u *CratesUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.requests++
	u.auth = append(u.auth, r.Header.Get("Authorization"))
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/crates/")
	name, tail, _ := strings.Cut(rest, "/")
	n, known := u.versions[name]
	rev := u.rev
	u.mu.Unlock()

	if !ok || tail != "versions" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if !known {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[{"detail":"crate not found"}]}`))
		return
	}
	seek, _ := strconv.Atoi(r.URL.Query().Get("seek"))
	if seek < 0 || seek > n {
		http.Error(w, "bad seek", http.StatusBadRequest)
		return
	}
	etag := fmt.Sprintf(`"%s-r%d-s%d"`, name, rev, seek)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	var page versionsPage
	page.Meta.Total = n
	end := min(seek+u.pageSize, n)
	for i := seek; i < end; i++ {
		idx := n - 1 - i // newest first
		num := versionName(idx)
		sum := sha256.Sum256([]byte(name + "@" + num))
		v := crateVersion{
			Num:       num,
			CreatedAt: epoch.Add(time.Duration(idx) * 24 * time.Hour).Format(time.RFC3339Nano),
			Yanked:    idx%7 == 3,
			Checksum:  hex.EncodeToString(sum[:]),
			License:   "MIT OR Apache-2.0",
			DLPath:    "/api/v1/crates/" + name + "/" + num + "/download",
			Crate:     name,
		}
		if idx%2 == 0 {
			rv := "1.70"
			v.RustVersion = &rv
		}
		page.Versions = append(page.Versions, v)
	}
	if end < n {
		next := fmt.Sprintf("?per_page=%d&seek=%d", u.pageSize, end)
		page.Meta.NextPage = &next
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}
