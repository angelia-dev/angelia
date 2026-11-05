package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// fallbackHealthcheckURL is the /healthz route of a process that runs with the
// default listen address (:8080, docs/configuration.md) on loopback.
const fallbackHealthcheckURL = "http://127.0.0.1:8080/healthz"

// healthcheckTimeout bounds the whole probe (dial, headers and body), so a hung
// process fails the container healthcheck instead of stalling it.
const healthcheckTimeout = 3 * time.Second

// healthcheckURL derives the default probe URL from a listen address such as
// the ANGELIA_LISTEN value (":8080", "10.0.0.5:9000", "[::]:9000"). An empty
// or unspecified host (0.0.0.0, ::) becomes 127.0.0.1, because the probe runs
// in the same container. An empty or unparseable address gives the fallback.
func healthcheckURL(listen string) string {
	if listen == "" {
		return fallbackHealthcheckURL
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return fallbackHealthcheckURL
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

// newHealthcheckCmd is the probe of the container image: the image is
// distroless (no shell, no curl), so the healthcheck runs the binary itself.
func newHealthcheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the /healthz endpoint of a running instance (exit 0 when it answers 2xx)",
		Long: "Probe the /healthz endpoint of a running instance. Exits 0 when the answer is 2xx, " +
			"and 1 otherwise (connection refused, timeout or any other status). Used by the " +
			"container HEALTHCHECK, where the image has no shell or curl. The default URL is " +
			"built from ANGELIA_LISTEN, or 127.0.0.1:8080 when it is not set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := cmd.Flags().GetString("url")
			if err != nil {
				return err
			}
			return checkHealthz(cmd.Context(), url, healthcheckTimeout)
		},
	}
	cmd.Flags().String("url", healthcheckURL(os.Getenv("ANGELIA_LISTEN")),
		"healthz URL to probe (http or https, no credentials)")
	return cmd
}

// checkHealthz GETs url within timeout and returns an error unless the answer
// is 2xx. Redirects are not followed (a 3xx is not healthy), and no HTTP proxy
// from the environment is used. The body is read only up to 4 KiB and then
// discarded. Error messages never contain the credentials of the URL.
func checkHealthz(ctx context.Context, url string, timeout time.Duration) error {
	u, err := neturl.Parse(url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("healthcheck: --url must be an absolute http or https URL")
	}
	if u.User != nil {
		return errors.New("healthcheck: --url must not contain credentials")
	}
	shown := u.Redacted()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("healthcheck: cannot build the request")
	}
	client := &http.Client{
		// Proxy nil: the probe is for this container, never for a proxy.
		Transport: &http.Transport{Proxy: nil},
		// A redirect answer is returned as is, so it fails the 2xx test.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		// Unwrap *url.Error, whose text includes the URL.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("healthcheck: GET %s: %w", shown, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("healthcheck: GET %s answered %s", shown, resp.Status)
	}
	return nil
}
