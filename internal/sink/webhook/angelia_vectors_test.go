package webhook

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// angeliaVectors mirrors webhooks/v1/vectors.json of angelia-dev/spec, vendored
// unchanged under testdata/angelia-vectors (see its README.md).
type angeliaVectors struct {
	ToleranceSeconds int64 `json:"tolerance_seconds"`
	Cases            []struct {
		Name            string            `json:"name"`
		SignSecrets     []string          `json:"sign_secrets"`
		ReceiverSecrets []string          `json:"receiver_secrets"`
		Headers         map[string]string `json:"headers"`
		Body            string            `json:"body"`
		Now             int64             `json:"now"`
		Expected        struct {
			Valid bool   `json:"valid"`
			Error string `json:"error"`
		} `json:"expected"`
	} `json:"cases"`
}

// tamperedCase is the one vector whose body differs from the signed bytes:
// Sign over the vendored body must not reproduce its signature.
const tamperedCase = "tampered-body"

func loadAngeliaVectors(t *testing.T) angeliaVectors {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "angelia-vectors", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v angeliaVectors
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Cases) < 13 || v.ToleranceSeconds != 300 {
		t.Fatalf("vectors.json: %d cases, tolerance %d s", len(v.Cases), v.ToleranceSeconds)
	}
	return v
}

// Every case of the vendored Angelia vectors runs through the real Sign and
// Verify: Sign reproduces the header signature (except for the tampered body),
// and Verify returns the expected result and error sentinel.
func TestAngeliaVectors(t *testing.T) {
	v := loadAngeliaVectors(t)
	sentinels := map[string]error{
		"missing_headers": ErrMissingHeaders, "timestamp": ErrTimestamp,
		"no_matching_signature": ErrNoMatchingSignature, "invalid_secret": ErrInvalidSecret,
	}
	tolerance := time.Duration(v.ToleranceSeconds) * time.Second
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			id := c.Headers[HeaderID]
			ts := c.Headers[HeaderTimestamp]
			sig := c.Headers[HeaderSignature]
			if id == "" || ts == "" || sig == "" {
				t.Fatalf("case has no %s, %s or %s header", HeaderID, HeaderTimestamp, HeaderSignature)
			}

			got, err := Sign(c.SignSecrets, id, ts, []byte(c.Body))
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if c.Name == tamperedCase {
				if got == sig {
					t.Errorf("Sign reproduced the signature of the tampered body")
				}
			} else if got != sig {
				t.Errorf("Sign = %q, want %q", got, sig)
			}

			h := http.Header{}
			for k, val := range c.Headers {
				h.Set(k, val)
			}
			err = Verify(c.ReceiverSecrets, h, []byte(c.Body), time.Unix(c.Now, 0), tolerance)
			if c.Expected.Valid {
				if err != nil {
					t.Fatalf("Verify: %v, want valid", err)
				}
				return
			}
			want := sentinels[c.Expected.Error]
			if want == nil {
				t.Fatalf("unknown error %q in vectors.json", c.Expected.Error)
			}
			if !errors.Is(err, want) {
				t.Fatalf("Verify: %v, want %v", err, want)
			}
		})
	}
}
