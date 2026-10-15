package config

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"
)

// Oldest TLS versions the listeners accept (Static.TLSMinVersion): TLS 1.3
// is always negotiated with the clients that support it.
const (
	TLSVersion12 = "1.2"
	TLSVersion13 = "1.3"
)

// Environment variable and flag of the TLS settings of the listeners.
const (
	EnvTLSMinVersion  = "NEXO_TLS_MIN_VERSION"
	FlagTLSMinVersion = "tls-min-version"
)

// bindTLSFlags defines the flags of the TLS settings.
func bindTLSFlags(fs *pflag.FlagSet) {
	fs.String(FlagTLSMinVersion, TLSVersion12, "oldest TLS version accepted by the listeners: 1.2 or 1.3 (TLS 1.3 is negotiated whenever the client supports it)")
}

// applyTLSEnv reads the environment of the TLS settings.
func (l *loader) applyTLSEnv(cfg *Static) {
	if v, ok := l.env(EnvTLSMinVersion); ok {
		cfg.TLSMinVersion = strings.TrimSpace(v)
	}
}

// applyTLSFlags reads the flags of the TLS settings.
func applyTLSFlags(cfg *Static, fs *pflag.FlagSet) error {
	if fs.Lookup(FlagTLSMinVersion) == nil || !fs.Changed(FlagTLSMinVersion) {
		return nil
	}
	v, err := fs.GetString(FlagTLSMinVersion)
	if err != nil {
		return fmt.Errorf("read flags: %w", err)
	}
	cfg.TLSMinVersion = strings.TrimSpace(v)
	return nil
}

// validateTLS checks the TLS settings.
func (s Static) validateTLS() []error {
	switch s.TLSMinVersion {
	case "", TLSVersion12, TLSVersion13:
		return nil
	default:
		return []error{fmt.Errorf("tls min version %q must be %s or %s", s.TLSMinVersion, TLSVersion12, TLSVersion13)}
	}
}
