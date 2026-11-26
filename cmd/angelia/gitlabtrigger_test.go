package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gitlabTriggerWatchFile = `version: 1
watches:
  - {name: pypi-requests, source: pypi, coordinates: {name: requests}, interval: 1h}
sinks:
  - name: gitlab-ci
    kind: gitlab-pipeline-trigger
    config:
      project: platform/deploys/app
      ref: main
      token_env: GITLAB_TRIGGER_TOKEN
      inputs:
        version: '{{.data.version}}'
subscriptions:
  - {name: requests-to-ci, sink: gitlab-ci}
`

// 90-C: angelia config check accepts a gitlab-pipeline-trigger sink.
func TestConfigCheckAcceptsGitlabTriggerSink(t *testing.T) {
	t.Setenv("GITLAB_TRIGGER_TOKEN", "glptt-test-value")
	t.Setenv("ANGELIA_CONFIG", "")
	wf := filepath.Join(t.TempDir(), "w.yaml")
	if err := osWriteFile(wf, gitlabTriggerWatchFile); err != nil {
		t.Fatal(err)
	}
	out, errb, err := runCheck(t, "--watches", wf)
	if err != nil || !strings.Contains(out, "watch file OK: 1 sinks, 1 watches, 1 subscriptions") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
}

func osWriteFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
