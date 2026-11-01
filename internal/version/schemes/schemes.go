// Package schemes chooses the version scheme of a watch (by source kind,
// overridable per watch) and wraps it so that unparseable versions fall
// back to the lexical scheme instead of failing.
package schemes

import (
	"fmt"
	"log/slog"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/calver"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

// maxLoggedRaw bounds how much of an upstream string is logged.
const maxLoggedRaw = 128

// Names lists the scheme names accepted by Lookup and in a watch's
// `scheme` override.
func Names() []string { return []string{semver.Name, pep440.Name, calver.Name, lexical.Name} }

// Lookup returns the scheme called name, without fallback. "semver" is
// strict SemVer.
func Lookup(name string) (version.Scheme, error) {
	switch name {
	case semver.Name:
		return semver.Scheme{}, nil
	case pep440.Name:
		return pep440.Scheme{}, nil
	case calver.Name:
		return calver.Scheme{}, nil
	case lexical.Name:
		return lexical.Scheme{}, nil
	}
	return nil, fmt.Errorf("unknown version scheme %q (want one of %v)", name, Names())
}

// ForSourceKind returns the default scheme for a source kind:
//
//   - pypi: PEP 440;
//   - npm, crates, github-release, gitlab-release: strict SemVer (with the
//     loose "v" prefix);
//   - oci, github-tag, go-proxy: SemVer that coerces short cores, so tags
//     like "3.12" or "v2" order as 3.12.0 and 2.0.0;
//   - anything else: lexical. Maven gets its own scheme in Phase 1.
//
// No source kind defaults to calver and the scheme is never auto-detected
// from the tags: a watch that wants calendar versions says `scheme: calver`
// (see Select).
func ForSourceKind(kind string) version.Scheme {
	switch kind {
	case "pypi":
		return pep440.Scheme{}
	case "npm", "crates", "github-release", "gitlab-release":
		return semver.Scheme{}
	case "oci", "github-tag", "go-proxy":
		return semver.Scheme{Coerce: true}
	}
	return lexical.Scheme{}
}

// Select returns the scheme for a watch: the override (the watch's
// `scheme:` value, an exact name from Names) when it is not empty, otherwise the source kind's default, wrapped with WithFallback.
// An override that names the default scheme keeps the default's settings
// (an oci watch with `scheme: semver` still coerces short tags).
func Select(sourceKind, override string, logger *slog.Logger) (version.Scheme, error) {
	s := ForSourceKind(sourceKind)
	if override != "" && override != s.Name() {
		var err error
		if s, err = Lookup(override); err != nil {
			return nil, fmt.Errorf("watch scheme override: %w", err)
		}
	}
	return WithFallback(s, logger), nil
}

// WithFallback wraps primary so that Parse never fails for a non-blank
// string: when primary rejects raw, raw is parsed as a lexical version
// instead and a Debug record is logged. Compare, IsPrerelease and Bump are
// primary's, which by the version.Scheme contract rank such fallback
// versions below every version primary could parse. A nil logger discards
// the records. A lexical primary is returned unchanged.
//
// Callers detect a fallback with v.Scheme() == lexical.Name. The fetch
// loop (#56) should pass logger.With("watch", id) and log one aggregated
// WARN per fetch (count and a few examples) rather than one per version,
// since a source with many odd tags would otherwise flood the log on
// every poll.
func WithFallback(primary version.Scheme, logger *slog.Logger) version.Scheme {
	if primary.Name() == lexical.Name {
		return primary
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return fallback{Scheme: primary, logger: logger}
}

type fallback struct {
	version.Scheme
	logger *slog.Logger
}

// Parse implements version.Scheme.
func (f fallback) Parse(raw string) (version.Version, error) {
	v, err := f.Scheme.Parse(raw)
	if err == nil {
		return v, nil
	}
	lv, lerr := lexical.Parse(raw)
	if lerr != nil {
		return nil, fmt.Errorf("%w (lexical fallback: %w)", err, lerr)
	}
	f.logger.Debug("version does not match the scheme, using lexical order",
		slog.String("scheme", f.Name()),
		slog.String("raw", version.Truncate(raw, maxLoggedRaw)),
		slog.String("error", err.Error()))
	return lv, nil
}
