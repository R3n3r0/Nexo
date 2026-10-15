package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Files of the certificates directory read by the probe, the same names
// as the keystore of the server (--certs-dir).
const (
	probeCertFile = "public.crt"
	probeCAsDir   = "CAs"
	// probeMaxPEM bounds every certificate file read by the probe.
	probeMaxPEM = 1 << 20
)

// newHealthcheckCmd probes the health endpoint of a local server. It is
// the HEALTHCHECK of the Docker images (distroless, no curl) and the
// readiness check of the systemd and cluster procedures. Without --url it
// probes the console address (NEXO_CONSOLE_ADDRESS) over HTTPS when the
// server runs with TLS (NEXO_CERTS_DIR or --certs-dir), trusting the
// certificates of that directory.
func newHealthcheckCmd(deps cmdDeps) *cobra.Command {
	var target, certsDir string
	var ready bool
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the local server's health endpoint (exit 0 when healthy)",
		Long: `Probe the health endpoint of the local server: exit 0 when it answers 200.

Without --url the console address (NEXO_CONSOLE_ADDRESS, default :9001) is
probed on the loopback interface, over HTTPS when the server runs with TLS
(NEXO_CERTS_DIR or --certs-dir). An HTTPS probe trusts the system CAs plus
public.crt and the CAs/ of the certificates directory; the certificate
chain of the local server is verified, not its host names, which need not
include the loopback address.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if certsDir == "" {
				certsDir, _ = deps.lookupEnv(config.EnvCertsDir)
			}
			u, loopback := target, false
			if u == "" {
				addr := config.DefaultConsoleAddress
				if v, ok := deps.lookupEnv(config.EnvConsoleAddress); ok && v != "" {
					addr = v
				}
				path := domain.HealthLivePath
				if ready {
					path = domain.HealthReadyPath
				}
				scheme := "http"
				if certsDir != "" {
					scheme = "https"
				}
				var err error
				if u, err = healthURL(scheme, addr, path); err != nil {
					return err
				}
				loopback = true
			}
			client := deps.client
			if certsDir != "" && strings.HasPrefix(strings.ToLower(u), "https://") {
				tlsCfg, err := probeTLSConfig(certsDir, loopback)
				if err != nil {
					return err
				}
				client = withTLSConfig(deps.client, tlsCfg)
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, u, nil)
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			resp, err := client.Do(req)
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
	cmd.Flags().StringVar(&certsDir, config.FlagCertsDir, "",
		"certificates directory of the server (default NEXO_CERTS_DIR): probe over HTTPS trusting its certificates")
	return cmd
}

// healthURL turns a listen address such as ":9001" into
// <scheme>://127.0.0.1:9001/<path>.
func healthURL(scheme, addr, path string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("console address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: path}).String(), nil
}

// probeTLSConfig trusts the system CAs, the certificate of the server
// (dir/public.crt, possibly self-signed) and the CAs of dir/CAs. For the
// loopback probe of the local server only the chain is verified: the
// certificate names the public and node host names, rarely 127.0.0.1.
func probeTLSConfig(dir string, loopback bool) (*tls.Config, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	files := []string{filepath.Join(dir, probeCertFile)}
	entries, err := os.ReadDir(filepath.Join(dir, probeCAsDir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("healthcheck: read the CAs of %s: %w", dir, err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			files = append(files, filepath.Join(dir, probeCAsDir, e.Name()))
		}
	}
	for _, f := range files {
		data, err := readPEM(f)
		if err != nil {
			return nil, err
		}
		pool.AppendCertsFromPEM(data)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	if loopback {
		// The default verification is replaced by VerifyConnection, which
		// checks the chain against the pool but not the host name.
		cfg.InsecureSkipVerify = true // #nosec G402 -- chain verified in VerifyConnection; loopback probe of the local server, no credentials sent
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyChain(cs.PeerCertificates, pool)
		}
	}
	return cfg, nil
}

// verifyChain verifies the certificate chain presented by the server
// against roots, for server authentication, without a host name.
func verifyChain(peer []*x509.Certificate, roots *x509.CertPool) error {
	if len(peer) == 0 {
		return errors.New("healthcheck: the server presented no certificate")
	}
	opts := x509.VerifyOptions{Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range peer[1:] {
		opts.Intermediates.AddCert(c)
	}
	if _, err := peer[0].Verify(opts); err != nil {
		return fmt.Errorf("healthcheck: server certificate: %w", err)
	}
	return nil
}

// readPEM reads a certificate file of the certificates directory (at most
// probeMaxPEM bytes); a missing file reads as empty.
func readPEM(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- file of the certificates directory chosen by the operator
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("healthcheck: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("healthcheck: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, probeMaxPEM))
	if err != nil {
		return nil, fmt.Errorf("healthcheck: read %s: %w", path, err)
	}
	return data, nil
}

// withTLSConfig returns a copy of base whose transport uses cfg; base is
// left untouched.
func withTLSConfig(base *http.Client, cfg *tls.Config) *http.Client {
	c := *base
	var tr *http.Transport
	if t, ok := base.Transport.(*http.Transport); ok && t != nil {
		tr = t.Clone()
	} else if t, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = t.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.TLSClientConfig = cfg
	c.Transport = tr
	return &c
}
