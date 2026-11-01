package gitlabrelease

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretRedactsEverywhere(t *testing.T) {
	const raw = "glpat-SyntheticSecretValue" //nolint:gosec // G101: synthetic test value.
	tok := secret(raw)
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		slog.New(h).Info("x", "token", tok, slog.Any("t2", tok))
	}
	j, _ := json.Marshal(map[string]any{"t": tok})
	out := buf.String() + string(j) + fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)
	if strings.Contains(out, raw) {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("no redaction marker in %s", out)
	}
}
