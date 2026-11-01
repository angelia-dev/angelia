// Package schemes chooses the version scheme of a watch (by source kind,
// overridable per watch) and wraps it so that unparseable versions fall
// back to the lexical scheme with a warning instead of failing.
package schemes

import (
	"fmt"
	"log/slog"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

// Names lists the scheme names accepted by Lookup and in a watch's
// `scheme` override.
func Names() []string { return []string{semver.Name, pep440.Name, lexical.Name} }

// Lookup returns the scheme called name, without fallback.
func Lookup(name string) (version.Scheme, error) {
	switch name {
	case semver.Name:
		return semver.Scheme{}, nil
	case pep440.Name:
		return pep440.Scheme{}, nil
	case lexical.Name:
		return lexical.Scheme{}, nil
	}
	return nil, fmt.Errorf("unknown version scheme %q (want one of %v)", name, Names())
}

// ForSourceKind returns the default scheme for a source kind: PEP 440 for
// PyPI, SemVer for the registries and forges whose versions are usually
// SemVer (with the loose "v" prefix for tags), lexical for everything else.
// Maven and calver get their own schemes in Phase 1.
func ForSourceKind(kind string) version.Scheme {
	switch kind {
	case "pypi":
		return pep440.Scheme{}
	case "npm", "crates", "go-proxy", "github-release", "github-tag", "gitlab-release", "oci":
		return semver.Scheme{}
	}
	return lexical.Scheme{}
}

// Select returns the scheme for a watch: the override when it is not
// empty, otherwise the source kind's default, wrapped with WithFallback.
func Select(sourceKind, override string, logger *slog.Logger) (version.Scheme, error) {
	s := ForSourceKind(sourceKind)
	if override != "" {
		var err error
		if s, err = Lookup(override); err != nil {
			return nil, fmt.Errorf("watch scheme override: %w", err)
		}
	}
	return WithFallback(s, logger), nil
}

// WithFallback wraps primary so that Parse never fails for a non-blank
// string: when primary rejects raw, a warning is logged and raw is parsed
// as a lexical version instead. Compare, IsPrerelease and Bump are
// primary's, which by the version.Scheme contract rank such fallback
// versions below every version primary could parse. A nil logger discards
// the warnings. A lexical primary is returned unchanged.
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
	f.logger.Warn("version does not match the scheme, using lexical order",
		slog.String("scheme", f.Name()),
		slog.String("error", err.Error())) // the error quotes the version, truncated
	return lv, nil
}
