package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// Environment variable names.
const (
	EnvPrefix             = "NEXO_"
	EnvAddress            = "NEXO_ADDRESS"
	EnvConsoleAddress     = "NEXO_CONSOLE_ADDRESS"
	EnvRootUser           = "NEXO_ROOT_USER"
	EnvRootPassword       = "NEXO_ROOT_PASSWORD"
	EnvRegion             = "NEXO_REGION"
	EnvDomain             = "NEXO_DOMAIN"
	EnvServerURL          = "NEXO_SERVER_URL"
	EnvBrowserRedirectURL = "NEXO_BROWSER_REDIRECT_URL"
	EnvCertsDir           = "NEXO_CERTS_DIR"
	EnvTrustedProxies     = "NEXO_TRUSTED_PROXIES"
	EnvLogLevel           = "NEXO_LOG_LEVEL"
	EnvLogFormat          = "NEXO_LOG_FORMAT"
	EnvEndpoints          = "NEXO_ENDPOINTS"
	EnvConfigFile         = "NEXO_CONFIG_FILE"
	EnvPrometheusAuthType = "NEXO_PROMETHEUS_AUTH_TYPE"
	EnvStorageClassStd    = "NEXO_STORAGE_CLASS_STANDARD"
	EnvStorageClassRRS    = "NEXO_STORAGE_CLASS_RRS"
	EnvLockStaleAfter     = "NEXO_LOCK_STALE_AFTER"
	EnvLockRefresh        = "NEXO_LOCK_REFRESH_INTERVAL"
	EnvRPCCallTimeout     = "NEXO_RPC_CALL_TIMEOUT"
	EnvHealRetryMaxDelay  = "NEXO_HEAL_RETRY_MAX_DELAY"
	EnvCacheResync        = "NEXO_CACHE_RESYNC_INTERVAL"
	EnvSendTimeout        = "NEXO_SEND_TIMEOUT"
	EnvTLSInternodeVerify = "NEXO_TLS_INTERNODE_VERIFY"
	EnvKMSSecretKey       = "NEXO_KMS_SECRET_KEY"
	EnvKMSSecretKeyPrev   = "NEXO_KMS_SECRET_KEY_PREVIOUS"
	EnvKMSEndpoint        = "NEXO_KMS_ENDPOINT"
	EnvKMSKeyName         = "NEXO_KMS_KEY_NAME"
	EnvKMSClientCert      = "NEXO_KMS_CLIENT_CERT"
	EnvKMSClientKey       = "NEXO_KMS_CLIENT_KEY"
	EnvKMSCAPath          = "NEXO_KMS_CAPATH"
	EnvSSECAllowInsecure  = "NEXO_SSEC_ALLOW_INSECURE"
	// FileSuffix is appended to a variable name to read its value from a file.
	FileSuffix = "_FILE"
)

// Flag names.
const (
	FlagAddress            = "address"
	FlagConsoleAddress     = "console-address"
	FlagRegion             = "region"
	FlagDomain             = "domain"
	FlagServerURL          = "server-url"
	FlagBrowserRedirectURL = "browser-redirect-url"
	FlagCertsDir           = "certs-dir"
	FlagTrustedProxies     = "trusted-proxies"
	FlagLogLevel           = "log-level"
	FlagLogFormat          = "log-format"
	FlagConfigFile         = "config"
	FlagStorageClassStd    = "storage-class-standard"
	FlagStorageClassRRS    = "storage-class-rrs"
	FlagTLSInternodeVerify = "tls-internode-verify"
)

// BindFlags defines the server flags on fs. Root credentials have no flag
// on purpose: they would leak through the process list.
func BindFlags(fs *pflag.FlagSet) {
	d := Default()
	fs.String(FlagAddress, d.Address, "S3 API listen address (host:port)")
	fs.String(FlagConsoleAddress, d.ConsoleAddress, "console and Admin API listen address (host:port)")
	fs.String(FlagRegion, d.Region, "default region")
	fs.String(FlagDomain, "", "domain for virtual-host style requests")
	fs.String(FlagServerURL, "", "public S3 URL used in presigned links")
	fs.String(FlagBrowserRedirectURL, "", "public console URL")
	fs.String(FlagCertsDir, "", "directory with public.crt and private.key (enables TLS)")
	fs.StringSlice(FlagTrustedProxies, nil, "IPs or CIDRs of trusted reverse proxies")
	fs.String(FlagLogLevel, d.LogLevel, "log level: debug, info, warn, error")
	fs.String(FlagLogFormat, d.LogFormat, "log format: json, text")
	fs.String(FlagConfigFile, "", "JSON configuration file (lowest precedence)")
	fs.String(FlagStorageClassStd, "", "parity of the STANDARD storage class as EC:n (erasure-coded modes)")
	fs.String(FlagStorageClassRRS, "", "parity of the REDUCED_REDUNDANCY storage class as EC:n (erasure-coded modes)")
	fs.Bool(FlagTLSInternodeVerify, false, "mutual TLS between the nodes: the internode calls need a client certificate verified with the CAs of --certs-dir (https endpoints)")
}

// Input is what the Loader reads from.
type Input struct {
	// Flags is the parsed flag set (optional).
	Flags *pflag.FlagSet
	// Args are the positional arguments: the endpoints.
	Args []string
}

// Loader builds a Static configuration.
type Loader interface {
	Load(ctx context.Context, in Input) (Static, error)
}

// Option configures the loader.
type Option func(*loader)

// LookupEnv is the environment accessor (os.LookupEnv by default).
type LookupEnv func(key string) (string, bool)

// ReadFile reads a file (os.ReadFile by default).
type ReadFile func(name string) ([]byte, error)

// WithEnv replaces the environment accessor (tests).
func WithEnv(fn LookupEnv) Option {
	return func(l *loader) {
		if fn != nil {
			l.env = fn
		}
	}
}

// WithReadFile replaces the file reader (tests).
func WithReadFile(fn ReadFile) Option {
	return func(l *loader) {
		if fn != nil {
			l.readFile = fn
		}
	}
}

type loader struct {
	env      LookupEnv
	readFile ReadFile
}

var _ Loader = (*loader)(nil)

// NewLoader returns the default loader.
func NewLoader(opts ...Option) Loader {
	l := &loader{env: os.LookupEnv, readFile: os.ReadFile}
	for _, o := range opts {
		o(l)
	}
	return l
}

// Load implements Loader: defaults, then file, then env, then flags, then
// validation.
func (l *loader) Load(_ context.Context, in Input) (Static, error) {
	cfg := Default()

	file, _ := l.env(EnvConfigFile)
	if in.Flags != nil && in.Flags.Changed(FlagConfigFile) {
		file, _ = in.Flags.GetString(FlagConfigFile)
	}
	if file != "" {
		if err := l.applyFile(&cfg, file); err != nil {
			return Static{}, err
		}
	}
	if err := l.applyEnv(&cfg); err != nil {
		return Static{}, err
	}
	if in.Flags != nil {
		if err := applyFlags(&cfg, in.Flags); err != nil {
			return Static{}, err
		}
	}
	if len(in.Args) > 0 {
		cfg.Endpoints = append([]string(nil), in.Args...)
	}
	if err := cfg.Validate(); err != nil {
		return Static{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

func (l *loader) applyFile(cfg *Static, name string) error {
	data, err := l.readFile(name)
	if err != nil {
		return fmt.Errorf("read config file %s: %w", name, err)
	}
	// Decode into the current config so that absent keys keep their values.
	if err := json.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse config file %s: %w", name, err)
	}
	return nil
}

func (l *loader) applyEnv(cfg *Static) error {
	set := func(key string, dst *string) {
		if v, ok := l.env(key); ok {
			*dst = v
		}
	}
	set(EnvAddress, &cfg.Address)
	set(EnvConsoleAddress, &cfg.ConsoleAddress)
	set(EnvRegion, &cfg.Region)
	set(EnvDomain, &cfg.Domain)
	set(EnvServerURL, &cfg.ServerURL)
	set(EnvBrowserRedirectURL, &cfg.BrowserRedirectURL)
	set(EnvCertsDir, &cfg.CertsDir)
	set(EnvLogLevel, &cfg.LogLevel)
	set(EnvLogFormat, &cfg.LogFormat)
	if v, ok := l.env(EnvPrometheusAuthType); ok {
		cfg.PrometheusAuthType = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvTrustedProxies); ok {
		cfg.TrustedProxies = splitList(v)
	}
	if v, ok := l.env(EnvEndpoints); ok {
		cfg.Endpoints = strings.Fields(v)
	}
	set(EnvStorageClassStd, &cfg.StorageClassStandard)
	set(EnvStorageClassRRS, &cfg.StorageClassRRS)
	for key, dst := range map[string]*bool{EnvTLSInternodeVerify: &cfg.TLSInternodeVerify, EnvSSECAllowInsecure: &cfg.SSECAllowInsecure} {
		if v, ok := l.env(key); ok && strings.TrimSpace(v) != "" {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = b
		}
	}
	if v, ok := l.env(EnvKMSEndpoint); ok {
		cfg.KMSEndpoints = splitList(v)
	}
	set(EnvKMSKeyName, &cfg.KMSKeyName)
	set(EnvKMSClientCert, &cfg.KMSCertFile)
	set(EnvKMSClientKey, &cfg.KMSKeyFile)
	set(EnvKMSCAPath, &cfg.KMSCAPath)
	for key, dst := range map[string]*time.Duration{
		EnvLockStaleAfter: &cfg.LockStaleAfter, EnvLockRefresh: &cfg.LockRefreshInterval, EnvRPCCallTimeout: &cfg.RPCCallTimeout,
		EnvHealRetryMaxDelay: &cfg.HealRetryMaxDelay, EnvCacheResync: &cfg.CacheResyncInterval, EnvSendTimeout: &cfg.SendTimeout,
	} {
		if v, ok := l.env(key); ok && strings.TrimSpace(v) != "" {
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = d
		}
	}
	var err error
	if cfg.RootUser, err = l.secret(EnvRootUser, cfg.RootUser); err != nil {
		return err
	}
	if cfg.RootPassword, err = l.secret(EnvRootPassword, cfg.RootPassword); err != nil {
		return err
	}
	if cfg.KMSSecretKey, err = l.secret(EnvKMSSecretKey, cfg.KMSSecretKey); err != nil {
		return err
	}
	previous, err := l.secret(EnvKMSSecretKeyPrev, "")
	if err != nil {
		return err
	}
	if previous != "" {
		cfg.KMSPreviousSecretKeys = splitList(previous)
	}
	return nil
}

// secret reads KEY or, when set, the file named by KEY_FILE (trimmed).
// KEY_FILE wins over KEY so that Docker secrets override a stale value.
func (l *loader) secret(key, current string) (string, error) {
	value := current
	if v, ok := l.env(key); ok {
		value = v
	}
	if path, ok := l.env(key + FileSuffix); ok && path != "" {
		data, err := l.readFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s from %s: %w", key, path, err)
		}
		value = strings.TrimSpace(string(data))
	}
	return value, nil
}

func applyFlags(cfg *Static, fs *pflag.FlagSet) error {
	var err error
	setString := func(flag string, dst *string) {
		if err != nil || !fs.Changed(flag) {
			return
		}
		var v string
		if v, err = fs.GetString(flag); err == nil {
			*dst = v
		}
	}
	setString(FlagAddress, &cfg.Address)
	setString(FlagConsoleAddress, &cfg.ConsoleAddress)
	setString(FlagRegion, &cfg.Region)
	setString(FlagDomain, &cfg.Domain)
	setString(FlagServerURL, &cfg.ServerURL)
	setString(FlagBrowserRedirectURL, &cfg.BrowserRedirectURL)
	setString(FlagCertsDir, &cfg.CertsDir)
	setString(FlagLogLevel, &cfg.LogLevel)
	setString(FlagLogFormat, &cfg.LogFormat)
	setString(FlagStorageClassStd, &cfg.StorageClassStandard)
	setString(FlagStorageClassRRS, &cfg.StorageClassRRS)
	if err != nil {
		return fmt.Errorf("read flags: %w", err)
	}
	if fs.Changed(FlagTLSInternodeVerify) {
		v, err := fs.GetBool(FlagTLSInternodeVerify)
		if err != nil {
			return fmt.Errorf("read flags: %w", err)
		}
		cfg.TLSInternodeVerify = v
	}
	if fs.Changed(FlagTrustedProxies) {
		v, err := fs.GetStringSlice(FlagTrustedProxies)
		if err != nil {
			return fmt.Errorf("read flags: %w", err)
		}
		cfg.TrustedProxies = v
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
