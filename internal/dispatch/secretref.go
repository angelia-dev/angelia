package dispatch

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// errSecretRefNotWired is the last_error of a delivery whose sink names a
// stored secret. Until the dispatcher resolves *_ref keys into
// sink.Request.Secrets (ADR 0024 §6, the #108 wiring), such a delivery
// fails closed, before Deliver, and is retried: a sink that only knows
// *_env would otherwise send it unsigned without a word.
const errSecretRefNotWired = "names a stored secret, and stored secrets are not resolved at delivery yet (#108); the delivery is retried"

// secretRef returns the JSON Pointer of the first *_ref key in a sink
// config, or "" when it has none or is not a JSON object.
func secretRef(cfg []byte) string {
	var v any
	if json.Unmarshal(cfg, &v) != nil {
		return ""
	}
	return findRef(v, "")
}

func findRef(v any, ptr string) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			p := ptr + "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(k)
			if strings.HasSuffix(k, "_ref") {
				return p
			}
			if r := findRef(t[k], p); r != "" {
				return r
			}
		}
	case []any:
		for i, e := range t {
			if r := findRef(e, ptr+"/"+strconv.Itoa(i)); r != "" {
				return r
			}
		}
	}
	return ""
}
