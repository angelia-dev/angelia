//go:build live

package gitlabtrigger_test

import (
	"os"
	"testing"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/gitlabtrigger"
)

// TestLiveTriggerPipeline starts one pipeline on gitlab.com. Run with
// `go test -tags live ./internal/sink/gitlabtrigger`. It is skipped unless
// ANGELIA_LIVE_GITLAB_TRIGGER_TOKEN (a pipeline trigger token),
// ANGELIA_LIVE_GITLAB_TRIGGER_PROJECT (a path or numeric id) and
// ANGELIA_LIVE_GITLAB_TRIGGER_REF are set. It creates one real pipeline in
// that project; CI does not run it.
func TestLiveTriggerPipeline(t *testing.T) {
	tok := os.Getenv("ANGELIA_LIVE_GITLAB_TRIGGER_TOKEN")
	project := os.Getenv("ANGELIA_LIVE_GITLAB_TRIGGER_PROJECT")
	ref := os.Getenv("ANGELIA_LIVE_GITLAB_TRIGGER_REF")
	if tok == "" || project == "" || ref == "" {
		t.Skip("set ANGELIA_LIVE_GITLAB_TRIGGER_TOKEN, ..._PROJECT and ..._REF to start one pipeline on gitlab.com")
	}
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := gitlabtrigger.New(c.HTTPClient(), gitlabtrigger.Options{})
	cfg := `{"project":"` + project + `","ref":"` + ref + `","token_env":"LIVE_TOKEN"}`
	if err := s.Validate(sink.Config(cfg)); err != nil {
		t.Fatal(err)
	}
	name := "live-test"
	prev := "1.0.0"
	env := event.Envelope{
		SpecVersion: "1.0", Type: event.TypePublished, ID: "01JD0000000000000000000000",
		Data: event.Data{
			Source: "pypi", Package: "angelia-live-test", Version: "1.0.1", PreviousVersion: &prev,
			Watch: event.Watch{ID: "01JD00000000000000000WATCH", Name: &name},
		},
	}
	res, err := s.Deliver(t.Context(), sink.Request{
		Config:         sink.Config(cfg),
		Event:          sink.ReleaseEvent{Envelope: env},
		Secrets:        map[string]secrets.Value{gitlabtrigger.TokenPointer: secrets.NewValueString(tok)},
		IdempotencyKey: "live-test:1", Attempt: 1,
	})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	t.Logf("GitLab answered %d", res.StatusCode)
}
