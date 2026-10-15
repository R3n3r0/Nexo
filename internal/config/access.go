package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// Listeners that accept the PROXY protocol (Static.ProxyProtocol).
const (
	ListenerS3      = "s3"
	ListenerConsole = "console"
	ListenerWeb     = "web"
)

// Header families of the trusted proxies (Static.ForwardedHeaders).
const (
	ForwardedHeadersX       = "x-forwarded"
	ForwardedHeadersRFC7239 = "forwarded"
)

// Unicode normalization of the keys matched by the access rules
// (Static.KeyNormalization).
const (
	KeyNormalizationNFC  = "nfc"
	KeyNormalizationNone = "none"
)

// Modes of the block public access (Static.BlockPublicAccessMode).
const (
	BlockPublicReject = "reject"
	BlockPublicIgnore = "ignore"
)

// DefaultAccessRulesMax is the default maximum number of access rules of
// a bucket.
const DefaultAccessRulesMax = 200

// Environment variables of the proxies and of the access rules.
const (
	EnvProxyProtocol         = "NEXO_PROXY_PROTOCOL"
	EnvForwardedHeaders      = "NEXO_FORWARDED_HEADERS"
	EnvTrustForwardedHost    = "NEXO_TRUST_FORWARDED_HOST"
	EnvAccessRulesMax        = "NEXO_ACCESS_RULES_MAX"
	EnvKeyNormalization      = "NEXO_KEY_NORMALIZATION"
	EnvBlockPublicAccess     = "NEXO_BLOCK_PUBLIC_ACCESS"
	EnvBlockPublicAccessMode = "NEXO_BLOCK_PUBLIC_ACCESS_MODE"
	// EnvSecretPrefix starts the variables of the secrets referenced by
	// the access rules: NEXO_SECRET_PROXY_TOKEN (or NEXO_SECRET_PROXY_TOKEN_FILE)
	// is ${secret:proxy-token}.
	EnvSecretPrefix = "NEXO_SECRET_"
)

// Flags of the proxies and of the access rules.
const (
	FlagProxyProtocol         = "proxy-protocol"
	FlagForwardedHeaders      = "forwarded-headers"
	FlagTrustForwardedHost    = "trust-forwarded-host"
	FlagAccessRulesMax        = "access-rules-max"
	FlagKeyNormalization      = "key-normalization"
	FlagBlockPublicAccess     = "block-public-access"
	FlagBlockPublicAccessMode = "block-public-access-mode"
)

// bindAccessFlags defines the flags of the proxies and of the access
// rules.
func bindAccessFlags(fs *pflag.FlagSet) {
	d := Default()
	fs.StringSlice(FlagProxyProtocol, nil, "listeners accepting the PROXY protocol v1/v2 from the trusted proxies: s3, console, web")
	fs.String(FlagForwardedHeaders, d.ForwardedHeaders, "headers the trusted proxies set: x-forwarded (X-Forwarded-For/Proto/Host) or forwarded (RFC 7239)")
	fs.Bool(FlagTrustForwardedHost, false, "use the host the trusted proxies forward (X-Forwarded-Host, Forwarded host=) instead of the Host header; only for proxies that overwrite it")
	fs.Int(FlagAccessRulesMax, d.AccessRulesMax, "maximum number of access rules of a bucket")
	fs.String(FlagKeyNormalization, d.KeyNormalization, "Unicode normalization of the keys matched by the access rules: nfc or none")
	fs.Bool(FlagBlockPublicAccess, false, "block the access rules granting everyone (cluster level, until changed at runtime)")
	fs.String(FlagBlockPublicAccessMode, d.BlockPublicAccessMode, "what the block public access does with the rules for everyone: reject (refused when saved) or ignore")
}

// applyAccessEnv reads the environment of the proxies and of the access
// rules.
func (l *loader) applyAccessEnv(cfg *Static) error {
	if v, ok := l.env(EnvProxyProtocol); ok {
		cfg.ProxyProtocol = lowerList(splitList(v))
	}
	if v, ok := l.env(EnvForwardedHeaders); ok {
		cfg.ForwardedHeaders = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvKeyNormalization); ok {
		cfg.KeyNormalization = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvBlockPublicAccessMode); ok {
		cfg.BlockPublicAccessMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvAccessRulesMax); ok && strings.TrimSpace(v) != "" {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAccessRulesMax, err)
		}
		cfg.AccessRulesMax = n
	}
	if v, ok := l.env(EnvBlockPublicAccess); ok && strings.TrimSpace(v) != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %w", EnvBlockPublicAccess, err)
		}
		cfg.BlockPublicAccess = b
	}
	if v, ok := l.env(EnvTrustForwardedHost); ok && strings.TrimSpace(v) != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %w", EnvTrustForwardedHost, err)
		}
		cfg.TrustForwardedHost = b
	}
	return l.applySecrets(cfg)
}

// applySecrets reads the NEXO_SECRET_<NAME> variables (and their _FILE
// form) into Static.Secrets, keyed by <name> in lowercase.
func (l *loader) applySecrets(cfg *Static) error {
	for _, kv := range l.environ() {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(key, EnvSecretPrefix) || len(key) == len(EnvSecretPrefix) {
			continue
		}
		base := strings.TrimSuffix(key, FileSuffix)
		if base == EnvSecretPrefix {
			continue
		}
		value, err := l.secret(base, "")
		if err != nil {
			return err
		}
		if cfg.Secrets == nil {
			cfg.Secrets = map[string]string{}
		}
		cfg.Secrets[strings.ToLower(strings.TrimPrefix(base, EnvSecretPrefix))] = value
	}
	return nil
}

// applyAccessFlags reads the flags of the proxies and of the access rules.
func applyAccessFlags(cfg *Static, fs *pflag.FlagSet) error {
	var errs []error
	if fs.Changed(FlagProxyProtocol) {
		v, err := fs.GetStringSlice(FlagProxyProtocol)
		errs = append(errs, err)
		cfg.ProxyProtocol = lowerList(v)
	}
	for flag, dst := range map[string]*string{
		FlagForwardedHeaders: &cfg.ForwardedHeaders, FlagKeyNormalization: &cfg.KeyNormalization, FlagBlockPublicAccessMode: &cfg.BlockPublicAccessMode,
	} {
		if fs.Changed(flag) {
			v, err := fs.GetString(flag)
			errs = append(errs, err)
			*dst = strings.ToLower(strings.TrimSpace(v))
		}
	}
	if fs.Changed(FlagAccessRulesMax) {
		v, err := fs.GetInt(FlagAccessRulesMax)
		errs = append(errs, err)
		cfg.AccessRulesMax = v
	}
	if fs.Changed(FlagBlockPublicAccess) {
		v, err := fs.GetBool(FlagBlockPublicAccess)
		errs = append(errs, err)
		cfg.BlockPublicAccess = v
	}
	if fs.Changed(FlagTrustForwardedHost) {
		v, err := fs.GetBool(FlagTrustForwardedHost)
		errs = append(errs, err)
		cfg.TrustForwardedHost = v
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("read flags: %w", err)
	}
	return nil
}

// validateAccess checks the settings of the proxies and of the access
// rules.
func (s Static) validateAccess() []error {
	var errs []error
	for _, l := range s.ProxyProtocol {
		switch l {
		case ListenerS3, ListenerConsole, ListenerWeb:
		default:
			errs = append(errs, fmt.Errorf("proxy protocol listener %q must be s3, console or web", l))
		}
	}
	if len(s.ProxyProtocol) > 0 && len(s.TrustedProxies) == 0 {
		errs = append(errs, errors.New("the proxy protocol needs the trusted proxies (--trusted-proxies, NEXO_TRUSTED_PROXIES)"))
	}
	switch s.ForwardedHeaders {
	case ForwardedHeadersX, ForwardedHeadersRFC7239:
	default:
		errs = append(errs, fmt.Errorf("forwarded headers %q must be %s or %s", s.ForwardedHeaders, ForwardedHeadersX, ForwardedHeadersRFC7239))
	}
	switch s.KeyNormalization {
	case KeyNormalizationNFC, KeyNormalizationNone:
	default:
		errs = append(errs, fmt.Errorf("key normalization %q must be %s or %s", s.KeyNormalization, KeyNormalizationNFC, KeyNormalizationNone))
	}
	switch s.BlockPublicAccessMode {
	case BlockPublicReject, BlockPublicIgnore:
	default:
		errs = append(errs, fmt.Errorf("block public access mode %q must be %s or %s", s.BlockPublicAccessMode, BlockPublicReject, BlockPublicIgnore))
	}
	if s.AccessRulesMax < 1 || s.AccessRulesMax > 10000 {
		errs = append(errs, fmt.Errorf("access rules max %d must be between 1 and 10000", s.AccessRulesMax))
	}
	return errs
}

// ProxyProtocolOn reports whether a listener accepts the PROXY protocol.
func (s Static) ProxyProtocolOn(listener string) bool {
	for _, l := range s.ProxyProtocol {
		if l == listener {
			return true
		}
	}
	return false
}

// lowerList trims and lowercases the values of a list.
func lowerList(in []string) []string {
	var out []string
	for _, v := range in {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}
