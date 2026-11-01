package netguard

import "net/http"

// Validators are the cache validators of a previous response, kept in a
// source's fetch state so that the next request can be conditional.
type Validators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

// apply sets If-None-Match and If-Modified-Since on h unless the caller has
// already set them.
func (v Validators) apply(h http.Header) {
	if v.ETag != "" && h.Get("If-None-Match") == "" {
		h.Set("If-None-Match", v.ETag)
	}
	if v.LastModified != "" && h.Get("If-Modified-Since") == "" {
		h.Set("If-Modified-Since", v.LastModified)
	}
}

// ValidatorsFrom reads ETag and Last-Modified from response headers.
func ValidatorsFrom(h http.Header) Validators {
	return Validators{ETag: h.Get("ETag"), LastModified: h.Get("Last-Modified")}
}

// merge returns v with the non-empty fields of n.
func (v Validators) merge(n Validators) Validators {
	if n.ETag != "" {
		v.ETag = n.ETag
	}
	if n.LastModified != "" {
		v.LastModified = n.LastModified
	}
	return v
}
