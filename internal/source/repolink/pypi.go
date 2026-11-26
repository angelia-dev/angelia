package repolink

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
)

// Links is what a package's own metadata says about its repository and its
// changelog.
type Links struct {
	// Repo is the declared repository; HasRepo is false when none parses.
	Repo    Repo
	HasRepo bool
	// Changelog is a declared changelog or release-notes page (an absolute
	// https URL on any host), "" when there is none.
	Changelog string
}

var (
	repoLabels      = []string{"source", "sourcecode", "repository", "code", "github", "gitlab", "homepage", "home"}
	changelogLabels = []string{"changelog", "releasenotes", "changes", "history", "whatsnew", "news"}
)

// PyPI picks the links out of the "info" object of the PyPI JSON response:
// projectURLs is info.project_urls (a JSON object, or anything else when the
// project has none) and homePage is info.home_page (a JSON string or null).
//
// Labels are compared after lower-casing and removing everything but letters
// and digits (PEP 753). The repository is the first label of "source",
// "sourcecode", "repository", "code", "github", "gitlab", "homepage", "home"
// whose URL parses, then any other label whose URL parses (in label order),
// then home_page. The changelog is the first of "changelog", "releasenotes",
// "changes", "history", "whatsnew", "news" whose value is an absolute https
// URL.
func PyPI(projectURLs, homePage json.RawMessage) Links {
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(projectURLs, &raw) // not an object: no labels
	type entry struct{ label, norm, value string }
	entries := make([]entry, 0, len(raw))
	for label, v := range raw {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue // null or not a string
		}
		entries = append(entries, entry{label, normLabel(label), strings.TrimSpace(s)})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if c := strings.Compare(a.norm, b.norm); c != 0 {
			return c
		}
		return strings.Compare(a.label, b.label)
	})
	var l Links
	for _, want := range repoLabels {
		for _, e := range entries {
			if e.norm != want {
				continue
			}
			if r, ok := Parse(e.value); ok {
				l.Repo, l.HasRepo = r, true
				break
			}
		}
		if l.HasRepo {
			break
		}
	}
	if !l.HasRepo {
		for _, e := range entries {
			if slices.Contains(repoLabels, e.norm) {
				continue
			}
			if r, ok := Parse(e.value); ok {
				l.Repo, l.HasRepo = r, true
				break
			}
		}
	}
	if !l.HasRepo {
		var hp string
		if json.Unmarshal(homePage, &hp) == nil {
			l.Repo, l.HasRepo = Parse(hp)
		}
	}
	for _, want := range changelogLabels {
		for _, e := range entries {
			if e.norm == want && httpsURL(e.value) {
				l.Changelog = e.value
				return l
			}
		}
	}
	return l
}

// normLabel lower-cases s and drops everything but ASCII letters and digits.
func normLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// httpsURL reports whether s is an absolute https URL with a host and no
// whitespace or control characters.
func httpsURL(s string) bool {
	if s == "" || len(s) > 2048 || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}
