package webhook

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/ulid"
)

type vectorFile struct {
	Sign []struct {
		Name, Secret, ID, Timestamp, Body, Signature string
	}
	Verify []struct {
		Name    string
		Secret  string
		Headers map[string]string
		Body    string
		Now     int64
		Valid   bool
		Error   string
	}
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "standard-webhooks", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectorFile
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Sign) == 0 || len(v.Verify) < 15 {
		t.Fatalf("vectors.json: %d sign and %d verify cases", len(v.Sign), len(v.Verify))
	}
	return v
}

// AC 1 (#108): signatures match the official Standard Webhooks reference
// library's test vectors, through Sign and through Verify.
func TestStandardWebhooksVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.Sign {
		got, err := Sign([]string{c.Secret}, c.ID, c.Timestamp, []byte(c.Body))
		if err != nil || got != c.Signature {
			t.Errorf("%s: Sign = %q, %v; want %q", c.Name, got, err, c.Signature)
		}
		h := http.Header{}
		h.Set(HeaderID, c.ID)
		h.Set(HeaderTimestamp, c.Timestamp)
		h.Set(HeaderSignature, got)
		ts, _ := time.Parse(time.RFC3339, "2021-02-25T15:02:10Z") // 1614265330
		if err := Verify([]string{c.Secret}, h, []byte(c.Body), ts, DefaultTolerance); err != nil {
			t.Errorf("%s: Verify of the signed message: %v", c.Name, err)
		}
	}
	sentinels := map[string]error{
		"missing_headers": ErrMissingHeaders, "timestamp": ErrTimestamp,
		"no_matching_signature": ErrNoMatchingSignature, "invalid_secret": ErrInvalidSecret,
	}
	for _, c := range v.Verify {
		t.Run(c.Name, func(t *testing.T) {
			h := http.Header{}
			for k, val := range c.Headers {
				h.Set(k, val)
			}
			err := Verify([]string{c.Secret}, h, []byte(c.Body), time.Unix(c.Now, 0), DefaultTolerance)
			if c.Valid {
				if err != nil {
					t.Fatalf("Verify: %v, want valid", err)
				}
				return
			}
			want := sentinels[c.Error]
			if want == nil {
				t.Fatalf("unknown error %q in vectors.json", c.Error)
			}
			if !errors.Is(err, want) {
				t.Fatalf("Verify: %v, want %v", err, want)
			}
		})
	}
}

func genSecret(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(b)
}

// Angelia-shaped deliveries (ULID id pair, CloudEvents body, three
// secrets) verify with each secret alone, and not with another one.
func TestSignRoundTripAngeliaShaped(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	body, err := os.ReadFile(filepath.Join("..", "..", "event", "testdata", "spec", "examples", "valid", "release", "envelope", "v1", "published-pypi.json"))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{genSecret(t, 24), genSecret(t, 32), genSecret(t, 64)}
	id := ulid.New(now) + ":" + ulid.New(now)
	ts := "1793880000"
	header, err := Sign(secrets, id, ts, body)
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Split(header, " ")
	if len(entries) != 3 {
		t.Fatalf("header %q: want 3 entries", header)
	}
	h := http.Header{}
	h.Set(HeaderID, id)
	h.Set(HeaderTimestamp, ts)
	h.Set(HeaderSignature, header)
	at := time.Unix(1793880000, 0)
	for i, s := range secrets {
		if err := Verify([]string{s}, h, body, at, DefaultTolerance); err != nil {
			t.Errorf("secret %d alone: %v", i, err)
		}
		// Each entry belongs to its secret, in order.
		one := h.Clone()
		one.Set(HeaderSignature, entries[i])
		if err := Verify([]string{s}, one, body, at, DefaultTolerance); err != nil {
			t.Errorf("entry %d with secret %d: %v", i, i, err)
		}
	}
	if err := Verify([]string{genSecret(t, 32)}, h, body, at, DefaultTolerance); !errors.Is(err, ErrNoMatchingSignature) {
		t.Errorf("other secret: %v", err)
	}
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] ^= 1
	if err := Verify(secrets, h, tampered, at, DefaultTolerance); !errors.Is(err, ErrNoMatchingSignature) {
		t.Errorf("tampered body: %v", err)
	}
}

func TestParseSecrets(t *testing.T) {
	s24, s32, s64 := genSecret(t, 24), genSecret(t, 32), genSecret(t, 64)
	raw32 := strings.TrimPrefix(s32, SecretPrefix)
	key32, _ := base64.StdEncoding.DecodeString(raw32)
	valid := map[string]int{
		s32:                          1,
		s32 + "\n":                   1, // a mounted file's newline
		"  " + s24 + "\t" + s64:      2,
		s32 + " " + s24 + "  " + s64: 3,
	}
	for v, n := range valid {
		keys, err := ParseSecrets(v)
		if err != nil || len(keys) != n {
			t.Errorf("ParseSecrets(%d fields): %d keys, %v", n, len(keys), err)
		}
	}
	if keys, _ := ParseSecrets(s32); string(keys[0]) != string(key32) {
		t.Error("the HMAC key is the decoded bytes")
	}
	urlSafe := SecretPrefix + base64.URLEncoding.EncodeToString([]byte(strings.Repeat("\xfb\xff", 16)))
	unpadded := SecretPrefix + base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdefghijklmnop")) // 26 bytes
	// 26 bytes end in "X=": bumping X sets padding bits that Strict refuses.
	enc := base64.StdEncoding.EncodeToString([]byte("0123456789abcdefghijklmnop"))
	trailingBits := SecretPrefix + enc[:len(enc)-2] + string(enc[len(enc)-2]+1) + "="
	invalid := map[string]string{
		"empty":                  "",
		"blank":                  " \n ",
		"no prefix":              raw32,
		"other prefix":           "whsek_" + raw32,
		"too short":              genSecret(t, 23),
		"too long":               genSecret(t, 65),
		"four secrets":           strings.Join([]string{s24, s32, s64, genSecret(t, 32)}, " "),
		"duplicate":              s32 + " " + s32,
		"url-safe":               urlSafe,
		"unpadded":               unpadded,
		"not base64":             SecretPrefix + strings.Repeat("!", 32),
		"repeated byte":          SecretPrefix + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"prefix only":            SecretPrefix,
		"non-zero trailing bits": trailingBits,
	}
	for name, v := range invalid {
		_, err := ParseSecrets(v)
		if !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: got %v, want ErrInvalidSecret", name, err)
			continue
		}
		if raw := strings.TrimPrefix(strings.TrimSpace(v), SecretPrefix); len(raw) > 4 && strings.Contains(err.Error(), raw) {
			t.Errorf("%s: error quotes the value: %v", name, err)
		}
	}
}

func TestSignRejects(t *testing.T) {
	s := genSecret(t, 32)
	for name, secrets := range map[string][]string{
		"none":       nil,
		"four":       {s, genSecret(t, 32), genSecret(t, 32), genSecret(t, 32)},
		"two in one": {s + " " + genSecret(t, 32)},
		"invalid":    {"whsec_x"},
	} {
		if _, err := Sign(secrets, "a:b", "1", nil); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("%s: got %v, want ErrInvalidSecret", name, err)
		}
	}
}

func TestVerifyTimestampRules(t *testing.T) {
	s := genSecret(t, 32)
	at := time.Unix(1_000_000, 0)
	for _, ts := range []string{"+1000000", "-1", "1e6", " 1000000", "99999999999999999999", "1000000.0"} {
		h := http.Header{}
		h.Set(HeaderID, "a:b")
		h.Set(HeaderTimestamp, ts)
		h.Set(HeaderSignature, "v1,AAAA")
		if err := Verify([]string{s}, h, nil, at, DefaultTolerance); !errors.Is(err, ErrTimestamp) {
			t.Errorf("timestamp %q: got %v, want ErrTimestamp", ts, err)
		}
	}
	h := http.Header{}
	h.Set(HeaderID, "x")
	h.Set(HeaderTimestamp, "1")
	h.Set(HeaderSignature, "v1,x")
	if err := Verify(nil, h, nil, at, DefaultTolerance); !errors.Is(err, ErrInvalidSecret) {
		t.Errorf("no secrets: %v", err)
	}
}
