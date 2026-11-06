package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/angelia-dev/angelia/internal/config"
)

// ResolveSecrets resolves the secret references of a sink configuration
// into the map of sink.Request.Secrets (ADR 0023 section 4, ADR 0024): every
// key ending in "_env", at any depth, names an environment variable read
// with lookupEnv. The map key is the RFC 6901 JSON Pointer of the reference
// key, such as "/secret_env". A configuration without references gives a
// nil map; one that is not JSON is left to the sink to reject.
//
// A reference that is not a valid variable name, or names a variable that
// is unset or empty, is an error naming the pointer (and the variable, when
// its name is valid), never a value. The dispatcher retries it: the
// operator fixes it by setting the variable.
func ResolveSecrets(cfg []byte, lookupEnv func(string) (string, bool)) (map[string][]byte, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, nil //nolint:nilerr // the sink reports an invalid configuration, permanently
	}
	var out map[string][]byte
	var walk func(v any, ptr string) error
	walk = func(v any, ptr string) error {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				p := ptr + "/" + escapePointer(k)
				if !strings.HasSuffix(k, "_env") {
					if err := walk(x[k], p); err != nil {
						return err
					}
					continue
				}
				name, ok := x[k].(string)
				if !ok || !config.IsEnvName(name) || config.LooksLikeToken(name) {
					return fmt.Errorf("dispatch: secret %s: not an environment variable name", p)
				}
				val, _ := lookupEnv(name)
				if val == "" {
					return fmt.Errorf("dispatch: secret %s: environment variable %s is not set or empty", p, name)
				}
				if out == nil {
					out = map[string][]byte{}
				}
				out[p] = []byte(val)
			}
		case []any:
			for i, e := range x {
				if err := walk(e, ptr+"/"+strconv.Itoa(i)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return out, nil
}

// escapePointer escapes a key for a JSON Pointer (RFC 6901 section 3).
func escapePointer(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
