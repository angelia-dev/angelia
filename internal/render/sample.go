package render

import (
	"encoding/json"
	"fmt"

	"github.com/angelia-dev/angelia/internal/event"
)

// Sample is an event a template is checked against.
type Sample struct {
	// Name says what the sample covers: "full" or "sparse".
	Name     string
	Envelope event.Envelope
}

func ptr(s string) *string { return &s }

// Samples are the events Check renders: "full" has every nullable field set,
// notes and labels included; "sparse" is a first version with every nullable
// field null, no watch name and no labels. A template that renders both
// handles any event the spec allows, as long as it guards nullable fields
// with "with" or "if" (docs/templates.md).
func Samples() []Sample {
	const watchID = "01JBX1Z3M4N5P6Q7R8S9T0V1W2"
	full := event.Envelope{
		SpecVersion:     event.SpecVersion,
		Type:            event.TypePublished,
		Source:          "angelia://angelia/watches/" + watchID,
		ID:              "01JBX2A7C8D9E0F1G2H3J4K5M6",
		Subject:         "pypi:requests@2.33.0",
		Time:            "2026-10-30T17:00:02.512Z",
		DataContentType: event.DataContentType,
		DataSchema:      event.DataSchema,
		Data: event.Data{
			Source:              "pypi",
			Package:             "requests",
			Version:             "2.33.0",
			NormalizedVersion:   "2.33.0",
			PreviousVersion:     ptr("2.32.5"),
			Scheme:              "pep440",
			Bump:                ptr("minor"),
			PublishedAt:         ptr("2026-10-27T14:02:11Z"),
			DetectedAt:          "2026-10-27T14:15:00Z",
			CooldownSatisfiedAt: ptr("2026-10-30T14:02:11Z"),
			URLs: event.URLs{
				Release:   ptr("https://pypi.org/project/requests/2.33.0/"),
				Changelog: ptr("https://pypi.org/project/requests/2.33.0/#history"),
				Artifact:  ptr("https://files.pythonhosted.org/packages/source/r/requests/requests-2.33.0.tar.gz"),
			},
			Notes: event.NewNotes("markdown", "## 2.33.0\n\n- Sample release notes."),
			Watch: event.Watch{ID: watchID, Name: ptr("requests"), Labels: map[string]string{"team": "platform", "repo": "api"}},
			PURL:  ptr("pkg:pypi/requests@2.33.0"),
		},
	}
	sparse := event.Envelope{
		SpecVersion:     event.SpecVersion,
		Type:            event.TypePublished,
		Source:          "angelia://angelia/watches/" + watchID,
		ID:              "01JBX2A7C8D9E0F1G2H3J4K5M7",
		Subject:         "github-release:octo-org/octo-repo@v0.1.0",
		Time:            "2026-10-30T17:00:02Z",
		DataContentType: event.DataContentType,
		DataSchema:      event.DataSchema,
		Data: event.Data{
			Source:            "github-release",
			Package:           "octo-org/octo-repo",
			Version:           "v0.1.0",
			NormalizedVersion: "0.1.0",
			Scheme:            "semver",
			DetectedAt:        "2026-10-30T17:00:00Z",
			Watch:             event.Watch{ID: watchID, Labels: map[string]string{}},
		},
	}
	return []Sample{{Name: "full", Envelope: full}, {Name: "sparse", Envelope: sparse}}
}

// Check renders t against every sample event and returns the first failure,
// naming the sample. Configuration loading calls it so that a template that
// cannot render, or renders past the limits, is rejected before any
// delivery uses it. It does no I/O and each sample is bounded by
// CheckTimeout (or Limits.Timeout if shorter), so it takes no context.
func (t *Template) Check() error { return t.CheckOutput(nil) }

// CheckOutput is Check that also passes the output for each sample event to
// check (when not nil); its error is returned naming the sample.
func (t *Template) CheckOutput(check func(out []byte) error) error {
	timeout := min(t.limits.Timeout, CheckTimeout)
	for _, s := range Samples() {
		b, err := json.Marshal(s.Envelope)
		if err != nil {
			return fmt.Errorf("render: sample %s: %w", s.Name, err)
		}
		r, err := t.render(newGuard(timeout), b)
		if err != nil {
			return fmt.Errorf("on the %s sample event: %w", s.Name, err)
		}
		if check != nil {
			if err := check(r.Body); err != nil {
				return fmt.Errorf("on the %s sample event, the result %w", s.Name, err)
			}
		}
	}
	return nil
}
