package config

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// What the S3 listener does with a host that is neither an alias of a
// distribution, a virtual host nor a main domain (Static.UnknownHost).
const (
	// UnknownHostPathStyle serves the path-style S3 API (the default).
	UnknownHostPathStyle = "path-style"
	// UnknownHostMisdirected answers 421 Misdirected Request.
	UnknownHostMisdirected = "misdirected"
)

// Whether the S3 listener serves the distributions (Static.S3Distributions).
const (
	// S3DistributionsAuto serves them when the deployment declares the
	// names of its S3 endpoint (NEXO_SERVER_URL or NEXO_DOMAIN), which
	// are then reserved: the default.
	S3DistributionsAuto = "auto"
	// S3DistributionsOn always serves them (explicit opt-in).
	S3DistributionsOn = "on"
	// S3DistributionsOff never serves them: only the web listeners do.
	S3DistributionsOff = "off"
)

// ACME challenges (Static.ACMEChallenge).
const (
	ACMEChallengeHTTP01    = "http-01"
	ACMEChallengeTLSALPN01 = "tls-alpn-01"
)

// Environment variables of the distributions and of their certificates.
const (
	EnvWebAddress    = "NEXO_WEB_ADDRESS"
	EnvWebTLSAddress = "NEXO_WEB_TLS_ADDRESS"
	EnvUnknownHost   = "NEXO_UNKNOWN_HOST"
	// EnvS3Distributions and EnvNoDistributions: see Static.S3Distributions
	// and Static.NoDistributions.
	EnvS3Distributions = "NEXO_S3_DISTRIBUTIONS"
	EnvNoDistributions = "NEXO_NO_DISTRIBUTIONS"
	EnvACMEDirectory   = "NEXO_ACME_DIRECTORY"
	EnvACMEEmail       = "NEXO_ACME_EMAIL"
	EnvACMEChallenge   = "NEXO_ACME_CHALLENGE"
	EnvACMECACert      = "NEXO_ACME_CA_CERT"
)

// Flags of the distributions and of their certificates.
const (
	FlagWebAddress    = "web-address"
	FlagWebTLSAddress = "web-tls-address"
	FlagUnknownHost   = "unknown-host"
	// FlagS3Distributions and FlagNoDistributions: see
	// Static.S3Distributions and Static.NoDistributions.
	FlagS3Distributions = "s3-distributions"
	FlagNoDistributions = "no-distributions"
	FlagACMEDirectory   = "acme-directory"
	FlagACMEEmail       = "acme-email"
	FlagACMEChallenge   = "acme-challenge"
	FlagACMECACert      = "acme-ca-cert"
)

// bindDistributionFlags defines the flags of the distributions.
func bindDistributionFlags(fs *pflag.FlagSet) {
	fs.String(FlagWebAddress, "", "listen address (host:port, e.g. :80) serving only the distributions and the ACME HTTP-01 challenges")
	fs.String(FlagWebTLSAddress, "", "TLS listen address (host:port, e.g. :443) serving only the distributions, with their certificates by SNI")
	fs.String(FlagUnknownHost, UnknownHostPathStyle, "S3 listener answer to an unknown host: path-style (S3 API) or misdirected (421)")
	fs.String(FlagS3Distributions, S3DistributionsAuto,
		"S3 listener serving the distributions: auto (when --server-url or --domain names the S3 endpoint), on or off (web listeners only)")
	fs.Bool(FlagNoDistributions, false, "recovery: route no host to a distribution (they stay editable through the Admin API)")
	fs.String(FlagACMEDirectory, "", "ACME directory URL of the automatic certificates (default Let's Encrypt)")
	fs.String(FlagACMEEmail, "", "contact of the ACME account")
	fs.String(FlagACMEChallenge, ACMEChallengeHTTP01, "ACME challenge: http-01 (web listener) or tls-alpn-01 (TLS web listener)")
	fs.String(FlagACMECACert, "", "PEM file of the CA of the ACME server (a private ACME CA)")
}

// applyDistributionEnv reads the environment of the distributions.
func (l *loader) applyDistributionEnv(cfg *Static) error {
	for key, dst := range map[string]*string{
		EnvWebAddress: &cfg.WebAddress, EnvWebTLSAddress: &cfg.WebTLSAddress, EnvUnknownHost: &cfg.UnknownHost,
		EnvACMEDirectory: &cfg.ACMEDirectory, EnvACMEEmail: &cfg.ACMEEmail, EnvACMEChallenge: &cfg.ACMEChallenge,
		EnvACMECACert: &cfg.ACMECACert, EnvS3Distributions: &cfg.S3Distributions,
	} {
		if v, ok := l.env(key); ok {
			*dst = strings.TrimSpace(v)
		}
	}
	cfg.S3Distributions = strings.ToLower(cfg.S3Distributions)
	if v, ok := l.env(EnvNoDistributions); ok && strings.TrimSpace(v) != "" {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%s: %w", EnvNoDistributions, err)
		}
		cfg.NoDistributions = b
	}
	return nil
}

// applyDistributionFlags reads the flags of the distributions.
func applyDistributionFlags(cfg *Static, fs *pflag.FlagSet) error {
	for flag, dst := range map[string]*string{
		FlagWebAddress: &cfg.WebAddress, FlagWebTLSAddress: &cfg.WebTLSAddress, FlagUnknownHost: &cfg.UnknownHost,
		FlagACMEDirectory: &cfg.ACMEDirectory, FlagACMEEmail: &cfg.ACMEEmail, FlagACMEChallenge: &cfg.ACMEChallenge,
		FlagACMECACert: &cfg.ACMECACert, FlagS3Distributions: &cfg.S3Distributions,
	} {
		if fs.Lookup(flag) == nil || !fs.Changed(flag) {
			continue
		}
		v, err := fs.GetString(flag)
		if err != nil {
			return fmt.Errorf("read flags: %w", err)
		}
		*dst = strings.TrimSpace(v)
	}
	cfg.S3Distributions = strings.ToLower(cfg.S3Distributions)
	if fs.Lookup(FlagNoDistributions) != nil && fs.Changed(FlagNoDistributions) {
		v, err := fs.GetBool(FlagNoDistributions)
		if err != nil {
			return fmt.Errorf("read flags: %w", err)
		}
		cfg.NoDistributions = v
	}
	return nil
}

// ServesDistributionsOnS3 reports whether the S3 listener routes the
// aliases of the distributions (Static.S3Distributions): always with "on",
// never with "off", and with "auto" only when the deployment names its S3
// endpoint (ServerURL or Domain). The S3 port answers on every name that
// reaches it (load balancers, service names, DNS aliases): without the
// declared names nothing tells an alias from a host the S3 clients use.
func (s Static) ServesDistributionsOnS3() bool {
	switch s.S3Distributions {
	case S3DistributionsOn:
		return true
	case S3DistributionsOff:
		return false
	default:
		return s.ServerURL != "" || s.Domain != ""
	}
}

// validateDistribution checks the settings of the distributions.
func (s Static) validateDistribution() []error {
	var errs []error
	used := map[string]string{s.Address: "address", s.ConsoleAddress: "console address"}
	for name, addr := range map[string]string{"web address": s.WebAddress, "web TLS address": s.WebTLSAddress} {
		if addr == "" {
			continue
		}
		if err := validateAddr(addr); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if other, taken := used[addr]; taken && !isEphemeralPort(addr) {
			errs = append(errs, fmt.Errorf("%s %s is also the %s", name, addr, other))
		}
		used[addr] = name
	}
	switch s.UnknownHost {
	case "", UnknownHostPathStyle, UnknownHostMisdirected:
	default:
		errs = append(errs, fmt.Errorf("unknown host %q must be %s or %s", s.UnknownHost, UnknownHostPathStyle, UnknownHostMisdirected))
	}
	switch s.S3Distributions {
	case "", S3DistributionsAuto, S3DistributionsOn, S3DistributionsOff:
	default:
		errs = append(errs, fmt.Errorf("s3 distributions %q must be %s, %s or %s", s.S3Distributions,
			S3DistributionsAuto, S3DistributionsOn, S3DistributionsOff))
	}
	switch s.ACMEChallenge {
	case "", ACMEChallengeHTTP01, ACMEChallengeTLSALPN01:
	default:
		errs = append(errs, fmt.Errorf("acme challenge %q must be %s or %s", s.ACMEChallenge, ACMEChallengeHTTP01, ACMEChallengeTLSALPN01))
	}
	if s.ACMEDirectory != "" {
		if u, err := url.Parse(s.ACMEDirectory); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, errors.New("acme directory must be an http(s) URL"))
		}
	}
	return errs
}
