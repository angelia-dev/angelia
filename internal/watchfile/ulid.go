package watchfile

import (
	"time"

	"github.com/angelia-dev/angelia/internal/ulid"
)

// newULID returns a new ULID string for the instant now (see package ulid).
func newULID(now time.Time) string { return ulid.New(now) }
