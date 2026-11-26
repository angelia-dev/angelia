package repolink

import (
	"encoding/json"
	"strings"
)

// NPM reads the top-level "repository" of a full npm packument: a string
// ("github:owner/repo", "owner/repo", a URL) or an object {type, url,
// directory}. directory is the package's folder in a monorepo ("" when the
// field is absent); it is only reported, never fetched. ok is false when the
// field is absent or not a recognised forge link.
func NPM(raw json.RawMessage) (repo Repo, directory string, ok bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		repo, ok = ParseNPM(s)
		return repo, "", ok
	}
	var o struct {
		URL       string `json:"url"`
		Directory string `json:"directory"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return Repo{}, "", false
	}
	repo, ok = ParseNPM(o.URL)
	if !ok {
		return Repo{}, "", false
	}
	directory = strings.Trim(strings.TrimSpace(o.Directory), "/")
	if len(directory) > 512 || strings.ContainsFunc(directory, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		directory = ""
	}
	return repo, directory, true
}
