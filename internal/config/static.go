// Package config holds the static server configuration: the settings read
// once at startup from command-line flags, NEXO_* environment variables and
// an optional JSON file, with precedence flags > env > file > defaults.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultAddress        = ":9000"
	DefaultConsoleAddress = ":9001"
	DefaultRegion         = "us-east-1"
	DefaultLogLevel       = "info"
	DefaultLogFormat      = "json"
	MinRootUserLength     = 3
	MinRootPasswordLength = 8
)

// Authentication of the Prometheus endpoints (/nexo/metrics/v1/*),
// NEXO_PROMETHEUS_AUTH_TYPE.
const (
	// PrometheusAuthJWT requires a bearer JWT signed with the secret key
	// of an identity allowed admin:Prometheus (the default).
	PrometheusAuthJWT = "jwt"
	// PrometheusAuthPublic serves the metrics to anyone.
	PrometheusAuthPublic = "public"
)

// Static is the startup configuration.
type Static struct {
	// Address is the S3 API listen address (host:port).
	Address string `json:"address"`
	// ConsoleAddress is the console/Admin API listen address.
	ConsoleAddress string `json:"consoleAddress"`
	// RootUser and RootPassword are the root credentials. They are never
	// read from flags or files, only from the environment (or *_FILE).
	RootUser     string `json:"-"`
	RootPassword string `json:"-"`
	// Region is the default bucket region.
	Region string `json:"region"`
	// Domain enables virtual-host style requests for *.Domain.
	Domain string `json:"domain"`
	// ServerURL is the public S3 URL used for presigned links.
	ServerURL string `json:"serverUrl"`
	// BrowserRedirectURL is the public console URL.
	BrowserRedirectURL string `json:"browserRedirectUrl"`
	// CertsDir holds public.crt/private.key for TLS; empty disables TLS.
	CertsDir string `json:"certsDir"`
	// TLSInternodeVerify enables mutual TLS between the nodes of a
	// cluster: the internode calls need a client certificate verified
	// with the CAs of CertsDir/CAs (and the certificate of the node), and
	// every node presents its own certificate as a client (it must allow
	// client authentication). It needs TLS (CertsDir).
	TLSInternodeVerify bool `json:"tlsInternodeVerify"`
	// TrustedProxies are the CIDRs or IPs whose X-Forwarded-* headers are trusted.
	TrustedProxies []string `json:"trustedProxies"`
	LogLevel       string   `json:"logLevel"`
	LogFormat      string   `json:"logFormat"`
	// Endpoints are the raw drive arguments (paths, URLs, ellipsis patterns).
	Endpoints []string `json:"endpoints"`
	// PrometheusAuthType protects the metrics endpoints: PrometheusAuthJWT
	// (default) or PrometheusAuthPublic.
	PrometheusAuthType string `json:"prometheusAuthType"`
	// StorageClassStandard and StorageClassRRS set the parity of the
	// STANDARD and REDUCED_REDUNDANCY storage classes of the erasure-coded
	// modes as "EC:n" (empty: the defaults of docs/03 §3.3).
	StorageClassStandard string `json:"storageClassStandard"`
	StorageClassRRS      string `json:"storageClassRRS"`
	// LockStaleAfter, LockRefreshInterval and RPCCallTimeout tune the
	// internode protocol of a cluster (environment only; zero keeps the
	// defaults: 60s, 10s and 30s): how long a lock of a node that stopped
	// refreshing it survives on the lock servers, how often a held lock is
	// refreshed, and how long an internode call may take.
	LockStaleAfter      time.Duration `json:"-"`
	LockRefreshInterval time.Duration `json:"-"`
	RPCCallTimeout      time.Duration `json:"-"`
	// HealRetryMaxDelay caps the delay between two attempts of the
	// background healing (MRF) at an object it could not heal yet, e.g.
	// while a node is down (environment only; zero keeps the default 5m).
	HealRetryMaxDelay time.Duration `json:"-"`
	// CacheResyncInterval is how often a cluster node reloads the shared
	// state it caches (IAM, policies, configuration, bucket metadata) even
	// without invalidation: the safety net of the invalidations a node
	// missed (environment only; zero keeps the default 2m).
	CacheResyncInterval time.Duration `json:"-"`
}

// Default returns the configuration defaults.
func Default() Static {
	return Static{
		Address:        DefaultAddress,
		ConsoleAddress: DefaultConsoleAddress,
		Region:         DefaultRegion,
		LogLevel:       DefaultLogLevel,
		LogFormat:      DefaultLogFormat,

		PrometheusAuthType: PrometheusAuthJWT,
	}
}

// TLSEnabled reports whether a certificates directory is configured.
func (s Static) TLSEnabled() bool { return s.CertsDir != "" }

// Validate checks the configuration and returns every problem found.
func (s Static) Validate() error {
	var errs []error
	if err := validateAddr(s.Address); err != nil {
		errs = append(errs, fmt.Errorf("address: %w", err))
	}
	if err := validateAddr(s.ConsoleAddress); err != nil {
		errs = append(errs, fmt.Errorf("console address: %w", err))
	}
	if s.Address == s.ConsoleAddress && !isEphemeralPort(s.Address) {
		errs = append(errs, errors.New("address and console address must differ"))
	}
	if len(s.RootUser) < MinRootUserLength {
		errs = append(errs, fmt.Errorf("root user is required (NEXO_ROOT_USER, at least %d characters)", MinRootUserLength))
	}
	if len(s.RootPassword) < MinRootPasswordLength {
		errs = append(errs, fmt.Errorf("root password is required (NEXO_ROOT_PASSWORD, at least %d characters)", MinRootPasswordLength))
	}
	if s.Region == "" {
		errs = append(errs, errors.New("region must not be empty"))
	}
	if s.TLSInternodeVerify && !s.TLSEnabled() {
		errs = append(errs, errors.New("tls internode verify needs TLS: set the certificates directory"))
	}
	for name, u := range map[string]string{"server url": s.ServerURL, "browser redirect url": s.BrowserRedirectURL} {
		if u == "" {
			continue
		}
		if parsed, err := url.Parse(u); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("%s %q must be an http(s) URL", name, u))
		}
	}
	for _, p := range s.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil && net.ParseIP(p) == nil {
			errs = append(errs, fmt.Errorf("trusted proxy %q is neither an IP nor a CIDR", p))
		}
	}
	switch strings.ToLower(s.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log level %q must be debug, info, warn or error", s.LogLevel))
	}
	switch strings.ToLower(s.LogFormat) {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("log format %q must be json or text", s.LogFormat))
	}
	switch s.PrometheusAuthType {
	case PrometheusAuthJWT, PrometheusAuthPublic:
	default:
		errs = append(errs, fmt.Errorf("prometheus auth type %q must be %s or %s", s.PrometheusAuthType, PrometheusAuthJWT, PrometheusAuthPublic))
	}
	if len(s.Endpoints) == 0 {
		errs = append(errs, errors.New("at least one endpoint (drive path or URL) is required"))
	}
	for name, v := range map[string]string{"standard storage class": s.StorageClassStandard, "reduced redundancy storage class": s.StorageClassRRS} {
		if err := validateStorageClass(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	for name, d := range map[string]time.Duration{
		"lock stale after": s.LockStaleAfter, "lock refresh interval": s.LockRefreshInterval, "rpc call timeout": s.RPCCallTimeout,
		"heal retry max delay": s.HealRetryMaxDelay, "cache resync interval": s.CacheResyncInterval,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s %s must not be negative", name, d))
		}
	}
	if s.LockStaleAfter > 0 && s.LockRefreshInterval > 0 && s.LockRefreshInterval >= s.LockStaleAfter {
		errs = append(errs, fmt.Errorf("lock refresh interval %s must be shorter than the lock stale delay %s", s.LockRefreshInterval, s.LockStaleAfter))
	}
	return errors.Join(errs...)
}

// validateStorageClass checks a storage class parity setting: empty, or
// "EC:n" with n >= 0.
func validateStorageClass(v string) error {
	if v == "" {
		return nil
	}
	scheme, num, ok := strings.Cut(strings.TrimSpace(v), ":")
	if n, err := strconv.Atoi(num); !ok || !strings.EqualFold(scheme, "EC") || err != nil || n < 0 {
		return fmt.Errorf("%q must be EC:<parity>", v)
	}
	return nil
}

// isEphemeralPort reports whether addr asks the kernel for a free port.
func isEphemeralPort(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	return err == nil && port == "0"
}

func validateAddr(addr string) error {
	if addr == "" {
		return errors.New("must not be empty")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	if port == "" {
		return fmt.Errorf("%q has no port", addr)
	}
	if host != "" && net.ParseIP(host) == nil && strings.ContainsAny(host, " /") {
		return fmt.Errorf("%q has an invalid host", addr)
	}
	return nil
}
