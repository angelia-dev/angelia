// Package webui serves the admin UI, a single-page app built from web/, from
// files embedded in the binary (ADR 0033 §7).
//
// # Embedding without Node
//
// The embedded tree is dist/. It always holds a committed placeholder,
// dist/index.html, which says that the UI was not built, so that a plain
// `CGO_ENABLED=0 go build ./...` and `go test ./...` never need Node. The
// real build (`make web`, which the release runs through GoReleaser's
// before hook) writes the Vite output to dist/app/, which git ignores: the
// placeholder is never overwritten, so `git status` stays clean. When
// dist/app/index.html is embedded the handler serves dist/app, else the
// placeholder.
//
// # Routes
//
// [Handler] serves everything under /ui/. A path without a file extension
// that matches no file gets index.html (the SPA's client-side routes).
// Paths under /ui/auth/ are not served here: sign-in belongs to another
// handler (#262), and until it exists they are 404.
package webui

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var embedded embed.FS

// Prefix is the URL path the UI is served under.
const Prefix = "/ui/"

const (
	// CSP is the Content-Security-Policy of every UI response.
	CSP = "default-src 'self'; img-src 'self' https://avatars.githubusercontent.com data:; " +
		"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	immutable = "public, max-age=31536000, immutable"
	noCache   = "no-cache"
)

// Built reports whether the embedded tree holds a real build, not the placeholder.
func Built() bool {
	_, err := fs.Stat(embedded, "dist/app/index.html")
	return err == nil
}

// Handler serves the embedded UI under [Prefix].
func Handler() http.Handler {
	root := "dist"
	if Built() {
		root = "dist/app"
	}
	sub, err := fs.Sub(embedded, root)
	if err != nil { // root is a constant that exists
		panic("webui: " + err.Error())
	}
	return handler(sub)
}

func handler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel, ok := strings.CutPrefix(path.Clean("/"+r.URL.Path), "/ui")
		if !ok || rel == "/auth" || strings.HasPrefix(rel, "/auth/") {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(rel, "/")
		if name == "" {
			name = "index.html"
		}
		if st, err := fs.Stat(files, name); err != nil || st.IsDir() {
			// A missing build asset is a 404; any other path is a client-side
			// route (names may hold dots, such as /ui/watches/django-4.2) and
			// gets the SPA's index.html.
			if strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", immutable)
		} else if name == "index.html" {
			h.Set("Cache-Control", noCache)
		}
		serveFile(w, r, files, name)
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, files fs.FS, name string) {
	f, err := files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// ServeContent sets Content-Type from the extension and handles HEAD and
	// ranges. Embedded files have no modification time, so no validators.
	http.ServeContent(w, r, name, time.Time{}, rs)
}

// embeddedSize is the sum of the embedded files' sizes.
func embeddedSize() (int64, error) {
	var total int64
	err := fs.WalkDir(embedded, "dist", func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err //nolint:wrapcheck // test helper
}
