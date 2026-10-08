// Package config holds the static server configuration: the settings read
// once at startup from command-line flags, NEXO_* environment variables and
// an optional JSON file, with precedence flags > env > file > defaults.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
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
	// TrustedProxies are the CIDRs or IPs whose X-Forwarded-* headers are trusted.
	TrustedProxies []string `json:"trustedProxies"`
	LogLevel       string   `json:"logLevel"`
	LogFormat      string   `json:"logFormat"`
	// Endpoints are the raw drive arguments (paths, URLs, ellipsis patterns).
	Endpoints []string `json:"endpoints"`
}

// Default returns the configuration defaults.
func Default() Static {
	return Static{
		Address:        DefaultAddress,
		ConsoleAddress: DefaultConsoleAddress,
		Region:         DefaultRegion,
		LogLevel:       DefaultLogLevel,
		LogFormat:      DefaultLogFormat,
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
	if len(s.Endpoints) == 0 {
		errs = append(errs, errors.New("at least one endpoint (drive path or URL) is required"))
	}
	return errors.Join(errs...)
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
