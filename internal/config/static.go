// Package config holds the static server configuration: the settings read
// once at startup from command-line flags, NEXO_* environment variables and
// an optional JSON file, with precedence flags > env > file > defaults.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
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
	// TLSMinVersion is the oldest TLS version the listeners accept:
	// TLSVersion12 (default) or TLSVersion13 (tls.go).
	TLSMinVersion string `json:"tlsMinVersion,omitempty"`
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
	// SendTimeout bounds how long a write of a response may stall
	// (environment only; zero keeps the default 20s): every response of
	// every listener (S3 and website downloads, console, Admin API), so
	// that a client that stops reading has its response aborted, releasing
	// the read lock of the object (docs/02 §2.4) and what the server
	// buffered for it.
	SendTimeout time.Duration `json:"-"`

	// KMS of the server-side encryption (docs/07 §7.2): the builtin KMS
	// keyed by KMSSecretKey, or the external KES servers of KMSEndpoints;
	// without either SSE-S3 and SSE-KMS are refused (SSE-C works).
	//
	// KMSSecretKey is the master key of the builtin KMS,
	// "<key-name>:<base64 of 32 bytes>" (NEXO_KMS_SECRET_KEY or _FILE,
	// environment only); the key name is the default key of SSE-S3.
	KMSSecretKey string `json:"-"`
	// KMSPreviousSecretKeys are the master keys KMSSecretKey replaced
	// (NEXO_KMS_SECRET_KEY_PREVIOUS, comma separated, environment only):
	// they still unseal the objects they sealed (master key rotation).
	KMSPreviousSecretKeys []string `json:"-"`
	// KMSEndpoints are the URLs of the KES servers (NEXO_KMS_ENDPOINT,
	// comma separated), KMSKeyName the default key there
	// (NEXO_KMS_KEY_NAME), reached with mutual TLS: the client
	// certificate KMSCertFile/KMSKeyFile (NEXO_KMS_CLIENT_CERT,
	// NEXO_KMS_CLIENT_KEY) and the CA certificates of KMSCAPath (a file or
	// a directory, NEXO_KMS_CAPATH; the system roots when empty).
	KMSEndpoints []string `json:"kmsEndpoints,omitempty"`
	KMSKeyName   string   `json:"kmsKeyName,omitempty"`
	KMSCertFile  string   `json:"kmsCertFile,omitempty"`
	KMSKeyFile   string   `json:"kmsKeyFile,omitempty"`
	KMSCAPath    string   `json:"kmsCAPath,omitempty"`
	// SSECAllowInsecure accepts the SSE-C keys over plain HTTP
	// (NEXO_SSEC_ALLOW_INSECURE, environment only): S3 requires TLS for
	// them; only for tests and development.
	SSECAllowInsecure bool `json:"-"`

	// Proxies and access rules (docs/10 §10.1, §10.2; access.go).
	//
	// ProxyProtocol lists the listeners (s3, console, web) accepting the
	// PROXY protocol v1/v2 from the TrustedProxies.
	ProxyProtocol []string `json:"proxyProtocol,omitempty"`
	// ForwardedHeaders is the header family the trusted proxies set:
	// "x-forwarded" (X-Forwarded-For/Proto/Host, the default) or
	// "forwarded" (RFC 7239).
	ForwardedHeaders string `json:"forwardedHeaders,omitempty"`
	// TrustForwardedHost uses the host the trusted proxies forward
	// (X-Forwarded-Host, Forwarded host=) instead of the Host header (off
	// by default: the proxies of deploy/proxy preserve the Host, and some
	// pass the X-Forwarded-Host of the client through unchanged).
	TrustForwardedHost bool `json:"trustForwardedHost,omitempty"`
	// AccessRulesMax bounds the access rules of a bucket (default 200).
	AccessRulesMax int `json:"accessRulesMax,omitempty"`
	// KeyNormalization is the Unicode normalization of the keys the
	// access rules match: "nfc" (default) or "none".
	KeyNormalization string `json:"keyNormalization,omitempty"`
	// BlockPublicAccess and BlockPublicAccessMode ("reject" or "ignore")
	// are the cluster-level block public access until it is changed at
	// runtime (the "access" subsystem of the dynamic configuration).
	BlockPublicAccess     bool   `json:"blockPublicAccess,omitempty"`
	BlockPublicAccessMode string `json:"blockPublicAccessMode,omitempty"`
	// Distributions (docs/10 §10.2; distribution.go).
	//
	// WebAddress and WebTLSAddress are the optional listeners serving the
	// distributions only (and the ACME challenges): plain HTTP (":80")
	// and TLS (":443") with the certificates of the domains by SNI.
	WebAddress    string `json:"webAddress,omitempty"`
	WebTLSAddress string `json:"webTLSAddress,omitempty"`
	// UnknownHost is what the S3 listener does with a host that is no
	// alias, virtual host or main domain: UnknownHostPathStyle (default)
	// or UnknownHostMisdirected (421).
	UnknownHost string `json:"unknownHost,omitempty"`
	// S3Distributions tells whether the S3 listener serves the
	// distributions: S3DistributionsAuto (default: when ServerURL or
	// Domain names the S3 endpoint), S3DistributionsOn (explicit opt-in)
	// or S3DistributionsOff (only the web listeners serve them). See
	// ServesDistributionsOnS3.
	S3Distributions string `json:"s3Distributions,omitempty"`
	// NoDistributions routes no host to a distribution on any listener
	// (recovery from a distribution that keeps the deployment from
	// working); they stay readable and editable through the Admin API.
	NoDistributions bool `json:"noDistributions,omitempty"`
	// ACMEDirectory, ACMEEmail and ACMEChallenge configure the automatic
	// certificates (tls.certificate "auto"): the directory of the CA
	// (default Let's Encrypt), the contact of the account and the
	// challenge (http-01 on the web listener, tls-alpn-01 on the TLS web
	// listener); ACMECACert is the PEM file of the CA of a private ACME
	// server.
	ACMEDirectory string `json:"acmeDirectory,omitempty"`
	ACMEEmail     string `json:"acmeEmail,omitempty"`
	ACMEChallenge string `json:"acmeChallenge,omitempty"`
	ACMECACert    string `json:"acmeCACert,omitempty"`
	// Cache of the distributions (docs/11; cache.go).
	//
	// CacheDir is the directory of the disk tier (empty: memory only),
	// CacheSize its size (the size of the memory tier without CacheDir),
	// CacheMemory the size of the memory tier (zero: the defaults) and
	// CacheMode the cluster mode (local, shared, shared+local).
	CacheDir    string `json:"cacheDir,omitempty"`
	CacheSize   int64  `json:"cacheSize,omitempty"`
	CacheMemory int64  `json:"cacheMemory,omitempty"`
	CacheMode   string `json:"cacheMode,omitempty"`
	// CacheWebhookAllow are the internal networks (CIDR or address) the
	// purge webhooks may reach: by default only public addresses.
	CacheWebhookAllow []string `json:"cacheWebhookAllow,omitempty"`

	// Distributed tracing (OpenTelemetry, docs/07 §7.4; observability.go).
	//
	// OTelEndpoint is the URL of the OTLP collector (empty: tracing
	// disabled), OTelProtocol its protocol ("http/protobuf", the default,
	// or "grpc") and OTelSampleRatio the share of the traces started by
	// the server that are recorded (0 to 1, default 1).
	OTelEndpoint    string  `json:"otelEndpoint,omitempty"`
	OTelProtocol    string  `json:"otelProtocol,omitempty"`
	OTelSampleRatio float64 `json:"otelSampleRatio,omitempty"`

	// Secrets are the secrets the access rules reference as
	// ${secret:<name>}: the NEXO_SECRET_<NAME> variables (or their _FILE
	// form), keyed by <name> in lowercase (environment only).
	Secrets map[string]string `json:"-"`
}

// KMSConfigured reports whether a KMS is configured (builtin or external).
func (s Static) KMSConfigured() bool { return s.KMSSecretKey != "" || len(s.KMSEndpoints) > 0 }

// Default returns the configuration defaults.
func Default() Static {
	return Static{
		Address:        DefaultAddress,
		ConsoleAddress: DefaultConsoleAddress,
		Region:         DefaultRegion,
		LogLevel:       DefaultLogLevel,
		LogFormat:      DefaultLogFormat,

		PrometheusAuthType: PrometheusAuthJWT,
		OTelSampleRatio:    1,

		ForwardedHeaders:      ForwardedHeadersX,
		AccessRulesMax:        DefaultAccessRulesMax,
		KeyNormalization:      KeyNormalizationNFC,
		BlockPublicAccessMode: BlockPublicReject,
		S3Distributions:       S3DistributionsAuto,
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
	errs = append(errs, credentialLength("root user", "NEXO_ROOT_USER", s.RootUser, MinRootUserLength)...)
	errs = append(errs, credentialLength("root password", "NEXO_ROOT_PASSWORD", s.RootPassword, MinRootPasswordLength)...)
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
		"heal retry max delay": s.HealRetryMaxDelay, "cache resync interval": s.CacheResyncInterval, "send timeout": s.SendTimeout,
	} {
		if d < 0 {
			errs = append(errs, fmt.Errorf("%s %s must not be negative", name, d))
		}
	}
	errs = append(errs, s.validateKMS()...)
	errs = append(errs, s.validateAccess()...)
	errs = append(errs, s.validateDistribution()...)
	errs = append(errs, s.validateCache()...)
	errs = append(errs, s.validateTLS()...)
	errs = append(errs, s.validateObservability()...)
	if s.LockStaleAfter > 0 && s.LockRefreshInterval > 0 && s.LockRefreshInterval >= s.LockStaleAfter {
		errs = append(errs, fmt.Errorf("lock refresh interval %s must be shorter than the lock stale delay %s", s.LockRefreshInterval, s.LockStaleAfter))
	}
	return errors.Join(errs...)
}

// validateKMS checks the KMS settings: one KMS at most, master keys of the
// form "<name>:<base64 of 32 bytes>", a default key and http(s) URLs for
// KES, the client certificate with its key.
func (s Static) validateKMS() []error {
	var errs []error
	if s.KMSSecretKey != "" && len(s.KMSEndpoints) > 0 {
		errs = append(errs, errors.New("kms: set either the builtin master key (NEXO_KMS_SECRET_KEY) or the KES endpoints (NEXO_KMS_ENDPOINT), not both"))
	}
	for i, k := range append([]string{s.KMSSecretKey}, s.KMSPreviousSecretKeys...) {
		if k == "" {
			continue
		}
		if err := validateMasterKey(k); err != nil {
			name := "NEXO_KMS_SECRET_KEY"
			if i > 0 {
				name = "NEXO_KMS_SECRET_KEY_PREVIOUS"
			}
			errs = append(errs, fmt.Errorf("kms: %s: %w", name, err))
		}
	}
	if len(s.KMSPreviousSecretKeys) > 0 && s.KMSSecretKey == "" {
		errs = append(errs, errors.New("kms: previous master keys need the current one (NEXO_KMS_SECRET_KEY)"))
	}
	if len(s.KMSEndpoints) > 0 {
		if s.KMSKeyName == "" {
			errs = append(errs, errors.New("kms: the KES default key is required (NEXO_KMS_KEY_NAME)"))
		}
		for _, e := range s.KMSEndpoints {
			// https only: the KES API returns the data keys in clear. The
			// errors reach stderr and the startup logs: the endpoint is
			// printed without its user info and query.
			shown := domain.RedactURL(e)
			switch u, err := url.Parse(e); {
			case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
				errs = append(errs, fmt.Errorf("kms: endpoint %q must be an https URL (NEXO_KMS_ENDPOINT)", shown))
			case u.Scheme != "https":
				errs = append(errs, fmt.Errorf("kms: endpoint %q must use https (NEXO_KMS_ENDPOINT): the KES API returns the data keys in clear", shown))
			case u.User != nil:
				errs = append(errs, fmt.Errorf("kms: endpoint %q must not carry credentials (NEXO_KMS_ENDPOINT)", shown))
			}
		}
	}
	if (s.KMSCertFile == "") != (s.KMSKeyFile == "") {
		errs = append(errs, errors.New("kms: the client certificate (NEXO_KMS_CLIENT_CERT) and its key (NEXO_KMS_CLIENT_KEY) go together"))
	}
	return errs
}

// validateMasterKey checks the form of a master key: "<name>:<base64 of
// 32 bytes>", the name made of letters, digits, '-', '_' and '.'.
func validateMasterKey(v string) error {
	name, encoded, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok || name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return errors.New("must be <key-name>:<base64 of 32 bytes>")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return errors.New("the key must be the base64 encoding of 32 bytes")
	}
	return nil
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

// credentialLength checks a root credential: missing, or set but shorter
// than minLen (the length only, never the value, is reported).
func credentialLength(what, env, value string, minLen int) []error {
	switch {
	case value == "":
		return []error{fmt.Errorf("%s is required (%s, at least %d characters)", what, env, minLen)}
	case len(value) < minLen:
		return []error{fmt.Errorf("%s must be at least %d characters (%s has %d)", what, minLen, env, len(value))}
	}
	return nil
}
