package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/spf13/cobra"
)

// defaultHealthcheckURL is the /healthz route of a process that runs with the
// default listen address (:8080, docs/configuration.md). Set --url when the
// listen port differs.
const defaultHealthcheckURL = "http://127.0.0.1:8080/healthz"

// healthcheckTimeout bounds the whole probe, so a hung process fails the
// container healthcheck instead of stalling it.
const healthcheckTimeout = 3 * time.Second

// newHealthcheckCmd is the probe of the container image: the image is
// distroless (no shell, no curl), so the healthcheck runs the binary itself.
func newHealthcheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the /healthz endpoint of a running instance (exit 0 when it answers 2xx)",
		Long: "Probe the /healthz endpoint of a running instance. Exits 0 when the answer is 2xx, " +
			"and 1 otherwise (connection refused, timeout or any other status). Used by the " +
			"container HEALTHCHECK, where the image has no shell or curl.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := cmd.Flags().GetString("url")
			if err != nil {
				return err
			}
			return checkHealthz(cmd.Context(), url, healthcheckTimeout)
		},
	}
	cmd.Flags().String("url", defaultHealthcheckURL, "healthz URL to probe (http or https)")
	return cmd
}

// checkHealthz GETs url within timeout and returns an error unless the answer
// is 2xx. The body is read only up to a few KiB and then discarded.
func checkHealthz(ctx context.Context, url string, timeout time.Duration) error {
	u, err := neturl.Parse(url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("healthcheck: --url must be an absolute http or https URL, got %q", url)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthcheck: %s answered %s", url, resp.Status)
	}
	return nil
}
