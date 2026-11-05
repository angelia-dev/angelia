package api

import (
	"encoding/base64"
	"encoding/json"
)

// pageCursor is the decoded form of an opaque list cursor (spec docs/api.md
// decision 17): the position of the last item of the previous page. K names
// the list it belongs to, so a cursor of one list is refused by another.
type pageCursor struct {
	K   string `json:"k"`
	N   string `json:"n,omitempty"` // name, kind or raw version
	I   string `json:"i,omitempty"` // id or interface
	Seq int64  `json:"s,omitempty"` // event seq (deliveries)
}

// Cursor list tags.
const (
	cursorWatches       = "w"
	cursorSinks         = "k"
	cursorSubscriptions = "s"
	cursorVersions      = "v"
	cursorDeliveries    = "d"
	cursorPlugins       = "p"
)

func encodeCursor(c pageCursor) *string {
	b, _ := json.Marshal(c) // plain strings and an integer
	s := base64.RawURLEncoding.EncodeToString(b)
	return &s
}

// decodeCursor reads the cursor parameter of list kind; nil or "" is the
// first page.
func decodeCursor(raw *string, kind string) (pageCursor, *ProblemDetail) {
	if raw == nil || *raw == "" {
		return pageCursor{K: kind}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(*raw)
	var c pageCursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.K != kind {
		return pageCursor{}, problem(SlugBadRequest, "query parameter cursor: not a cursor issued by this server")
	}
	return c, nil
}

// pageLimit is the limit parameter, or the default 100.
func pageLimit(l *int) int {
	if l == nil {
		return 100
	}
	return *l
}
