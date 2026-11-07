package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// releasePublishedType is the CloudEvents type of a new release, without its
// schema suffix (".v1").
const releasePublishedType = "dev.angelia.release.published"

// await polls GET {base}/received until a release.published delivery for
// version arrives, or ctx ends. It does not sleep longer than poll between
// requests, and never after success.
func await(ctx context.Context, c *http.Client, base, version string, poll time.Duration) (delivery, error) {
	var last error
	seen := 0
	for {
		got, err := fetch(ctx, c, base)
		switch {
		case err != nil:
			last = err
		default:
			seen = len(got)
			last = nil
			for _, d := range got {
				if d.Version == version && strings.HasPrefix(d.Type, releasePublishedType) {
					return d, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if last != nil {
				return delivery{}, fmt.Errorf("await: no release.published for version %s before the deadline; last error: %w", version, last)
			}
			return delivery{}, fmt.Errorf("await: no release.published for version %s before the deadline (%d other deliveries seen)", version, seen)
		case <-time.After(poll):
		}
	}
}

func fetch(ctx context.Context, c *http.Client, base string) ([]delivery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/received", nil) //nolint:gosec // G704: the URL is the operator's -receiver flag of a test tool
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("receiver answered " + resp.Status)
	}
	var out []delivery
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}
