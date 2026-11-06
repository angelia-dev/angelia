package sink

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// W2 (#75 review): Request.Secrets holds secrets.Value, so a Request passed
// to fmt or slog prints [REDACTED], never the value (ADR 0024 amendment).
func TestRequestSecretsRedacted(t *testing.T) {
	const canary = "whsec_cmVxdWVzdC1jYW5hcnktdmFsdWUtMDEyMzQ1Njc="
	req := Request{
		Config:  Config(`{"url":"https://h.example/","secret_ref":"hook"}`),
		Secrets: map[string]secrets.Value{"/secret_ref": secrets.NewValueString(canary)},
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("deliver", "req", req, "secrets", req.Secrets)
	for name, out := range map[string]string{
		"%v": fmt.Sprintf("%v", req), "%+v": fmt.Sprintf("%+v", req), "%#v": fmt.Sprintf("%#v", req),
		"%s": fmt.Sprintf("%s", req.Secrets), "slog JSON": logs.String(),
	} {
		if strings.Contains(out, canary) || strings.Contains(out, strings.TrimPrefix(canary, "whsec_")) {
			t.Errorf("%s prints the secret", name)
		}
		if !strings.Contains(out, secrets.Redacted) {
			t.Errorf("%s does not print %s: %s", name, secrets.Redacted, out)
		}
	}
}
