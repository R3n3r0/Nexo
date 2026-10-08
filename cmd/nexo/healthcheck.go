package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

// newHealthcheckCmd probes the liveness endpoint of a local server. It is
// used as the Docker HEALTHCHECK because distroless images have no curl.
func newHealthcheckCmd(deps cmdDeps) *cobra.Command {
	var target string
	var ready bool
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the local server's health endpoint (exit 0 when healthy)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := target
			if u == "" {
				addr := config.DefaultConsoleAddress
				if v, ok := deps.lookupEnv(config.EnvConsoleAddress); ok && v != "" {
					addr = v
				}
				path := s3.PathHealthLive
				if ready {
					path = s3.PathHealthReady
				}
				var err error
				if u, err = healthURL(addr, path); err != nil {
					return err
				}
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, u, nil)
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			resp, err := deps.client.Do(req)
			if err != nil {
				return fmt.Errorf("healthcheck %s: %w", u, err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("healthcheck %s: status %s", u, resp.Status)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "ok: %s\n", u)
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "health URL to probe (default derived from NEXO_CONSOLE_ADDRESS)")
	cmd.Flags().BoolVar(&ready, "ready", false, "probe the readiness endpoint instead of liveness")
	return cmd
}

// healthURL turns a listen address such as ":9001" into http://127.0.0.1:9001/<path>.
func healthURL(addr, path string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("console address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: path}).String(), nil
}
