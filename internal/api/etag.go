package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/angelia-dev/angelia/internal/store"
)

// Optimistic concurrency on watches, sinks and subscriptions (spec
// docs/api.md decision 52, story #274): an ETag on GET and PUT, and the
// If-Match and If-None-Match: * preconditions on PUT and DELETE.
//
// The tag is strong and derived from the stored row: SHA-256 over a
// canonical JSON encoding of the resource's identity and configuration,
// never its status, cut to 128 bits and base64url-encoded. So every replica
// computes the same tag without any stored version, a PUT that changes
// nothing keeps it, and the tag is the same for every caller (it is computed
// before any redaction).

// etagVersion separates this encoding from any later one.
const etagVersion = "angelia-etag-v1"

// etagOf returns the quoted tag of the resource kind with the given parts.
func etagOf(kind string, parts ...any) string {
	b, err := json.Marshal(parts)
	if err != nil {
		// Every part is a string, a number, a map of strings or a
		// canonical JSON value: Marshal cannot fail.
		panic("api: etag: " + err.Error())
	}
	h := sha256.New()
	h.Write([]byte(etagVersion + "\x00" + kind + "\x00"))
	h.Write(b)
	return `"` + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:16]) + `"`
}

// canonicalJSON decodes stored JSON and returns it in a form that marshals
// the same whatever the engine did to it (key order, spacing): Postgres
// may store JSON as jsonb. Bytes that are not JSON are kept as they are
// (marshalled as base64).
func canonicalJSON(b []byte) any {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	v, err := decodeJSON(b)
	if err != nil {
		return b
	}
	return v
}

// watchETag is the tag of a watch on its target: name, id, owner, the
// target's source and coordinates, and the watch's interval, labels and
// policy. The target's check times and the watch's baseline are status.
func watchETag(w store.Watch, t store.Target) string {
	labels := w.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return etagOf("watch", w.ID, w.Name, ownerOf(w), t.Kind, t.Coordinates, int64(w.Interval), labels, canonicalJSON(w.Policy))
}

// sinkETag is the tag of a sink: name, id, owner, kind and the stored
// configuration (before any redaction).
func sinkETag(k store.Sink) string {
	return etagOf("sink", k.ID, k.Name, ownerOf(k), k.Kind, canonicalJSON(k.ConfigEncrypted))
}

// subscriptionETag is the tag of a subscription: name, id, owner, sink id,
// filter, policy and template.
func subscriptionETag(sub store.Subscription) string {
	return etagOf("subscription", sub.ID, sub.Name, ownerOf(sub), sub.SinkID, canonicalJSON(sub.Filter), canonicalJSON(sub.Policy), sub.Template)
}

// entityTags splits an If-Match value (already checked against the
// specification's pattern) into its tags, each with its W/ prefix if it has
// one; "*" is returned alone.
func entityTags(v string) []string {
	var out []string
	for v = strings.TrimSpace(v); v != ""; {
		if v == "*" {
			return []string{"*"}
		}
		weak := strings.HasPrefix(v, "W/")
		rest := strings.TrimPrefix(v, "W/")
		if !strings.HasPrefix(rest, `"`) {
			return out
		}
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return out
		}
		tag := rest[:end+2]
		if weak {
			tag = "W/" + tag
		}
		out = append(out, tag)
		v = strings.TrimLeft(rest[end+2:], " \t")
		v = strings.TrimLeft(strings.TrimPrefix(v, ","), " \t")
	}
	return out
}

// preconditions are a write's If-Match and If-None-Match headers.
type preconditions struct {
	ifMatch     *string
	ifNoneMatch bool // If-None-Match: * (the only value the spec allows)
}

// check evaluates the preconditions against the resource's current tag,
// "" when it does not exist, and returns the 412 problem if they fail.
// Strong comparison (RFC 9110 §8.8.3.2): a weak tag never matches.
func (pc preconditions) check(current, what, name string) error {
	if pc.ifNoneMatch && current != "" {
		return problem(SlugPreconditionFailed, "%s %s exists and If-None-Match is *", what, name)
	}
	if pc.ifMatch == nil {
		return nil
	}
	for _, t := range entityTags(*pc.ifMatch) {
		if current != "" && (t == "*" || t == current) {
			return nil
		}
	}
	if current == "" {
		return problem(SlugPreconditionFailed, "%s %s does not exist and If-Match is set", what, name)
	}
	return problem(SlugPreconditionFailed, "%s %s is not at the version If-Match names; read it again", what, name)
}

// withETag returns r with an ETag header.
func (r response) withETag(tag string) response {
	if r.header == nil {
		r.header = make(map[string][]string)
	}
	r.header.Set("ETag", tag)
	return r
}
