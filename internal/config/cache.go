package config

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// Cluster modes of the cache of the distributions (Static.CacheMode,
// docs/11 §11.4).
const (
	CacheModeLocal       = "local"
	CacheModeShared      = "shared"
	CacheModeSharedLocal = "shared+local"
)

// Defaults of the cache.
const (
	// DefaultCacheSize is the size of the disk tier (with --cache-dir).
	DefaultCacheSize = 10 << 30
	// DefaultCacheMemory is the size of the memory tier.
	DefaultCacheMemory = 256 << 20
)

// Environment variables of the cache.
const (
	EnvCacheDir    = "NEXO_CACHE_DIR"
	EnvCacheSize   = "NEXO_CACHE_SIZE"
	EnvCacheMemory = "NEXO_CACHE_MEMORY"
	EnvCacheMode   = "NEXO_CACHE_MODE"
	// EnvCacheWebhookAllow lists, comma separated, the internal networks
	// the purge webhooks may reach.
	EnvCacheWebhookAllow = "NEXO_CACHE_WEBHOOK_ALLOW"
)

// Flags of the cache.
const (
	FlagCacheDir    = "cache-dir"
	FlagCacheSize   = "cache-size"
	FlagCacheMemory = "cache-memory"
	FlagCacheMode   = "cache-mode"
	// FlagCacheWebhookAllow is repeatable (or comma separated).
	FlagCacheWebhookAllow = "cache-webhook-allow"
)

// bindCacheFlags defines the flags of the cache.
func bindCacheFlags(fs *pflag.FlagSet) {
	fs.String(FlagCacheDir, "", "directory of the disk tier of the distribution cache (a separate SSD/NVMe; /cache in the containers); without it only the memory tier is used")
	fs.String(FlagCacheSize, "", "size of the distribution cache (the disk tier with --cache-dir, the memory tier without), e.g. 10GiB")
	fs.String(FlagCacheMemory, "", "size of the memory tier of the distribution cache, e.g. 256MiB")
	fs.String(FlagCacheMode, CacheModeLocal, "cache of a cluster: local (one per node), shared (one owner per key) or shared+local")
	fs.StringSlice(FlagCacheWebhookAllow, nil, "internal networks (CIDR or address) the purge webhooks of the cache may reach, e.g. 10.0.0.0/8 for an nginx or Varnish of the LAN; by default only public addresses")
}

// applyCacheEnv reads the environment of the cache.
func (l *loader) applyCacheEnv(cfg *Static) error {
	if v, ok := l.env(EnvCacheDir); ok {
		cfg.CacheDir = strings.TrimSpace(v)
	}
	if v, ok := l.env(EnvCacheMode); ok {
		cfg.CacheMode = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := l.env(EnvCacheWebhookAllow); ok {
		cfg.CacheWebhookAllow = splitList(v)
	}
	for key, dst := range map[string]*int64{EnvCacheSize: &cfg.CacheSize, EnvCacheMemory: &cfg.CacheMemory} {
		if v, ok := l.env(key); ok && strings.TrimSpace(v) != "" {
			n, err := ParseSize(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = n
		}
	}
	return nil
}

// applyCacheFlags reads the flags of the cache.
func applyCacheFlags(cfg *Static, fs *pflag.FlagSet) error {
	get := func(flag string) (string, bool, error) {
		if fs.Lookup(flag) == nil || !fs.Changed(flag) {
			return "", false, nil
		}
		v, err := fs.GetString(flag)
		if err != nil {
			return "", false, fmt.Errorf("read flags: %w", err)
		}
		return strings.TrimSpace(v), true, nil
	}
	if v, ok, err := get(FlagCacheDir); err != nil {
		return err
	} else if ok {
		cfg.CacheDir = v
	}
	if v, ok, err := get(FlagCacheMode); err != nil {
		return err
	} else if ok {
		cfg.CacheMode = strings.ToLower(v)
	}
	if f := fs.Lookup(FlagCacheWebhookAllow); f != nil && fs.Changed(FlagCacheWebhookAllow) {
		v, err := fs.GetStringSlice(FlagCacheWebhookAllow)
		if err != nil {
			return fmt.Errorf("read flags: %w", err)
		}
		cfg.CacheWebhookAllow = splitList(strings.Join(v, ","))
	}
	for flag, dst := range map[string]*int64{FlagCacheSize: &cfg.CacheSize, FlagCacheMemory: &cfg.CacheMemory} {
		v, ok, err := get(flag)
		if err != nil {
			return err
		}
		if !ok || v == "" {
			continue
		}
		n, err := ParseSize(v)
		if err != nil {
			return fmt.Errorf("--%s: %w", flag, err)
		}
		*dst = n
	}
	return nil
}

// validateCache checks the settings of the cache.
func (s Static) validateCache() []error {
	var errs []error
	switch s.CacheMode {
	case "", CacheModeLocal, CacheModeShared, CacheModeSharedLocal:
	default:
		errs = append(errs, fmt.Errorf("cache mode %q must be %s, %s or %s", s.CacheMode, CacheModeLocal, CacheModeShared, CacheModeSharedLocal))
	}
	if s.CacheSize < 0 || s.CacheMemory < 0 {
		errs = append(errs, fmt.Errorf("cache sizes must not be negative"))
	}
	if _, err := s.CacheWebhookNetworks(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// CacheWebhookNetworks parses CacheWebhookAllow: a CIDR, or an address
// (a network of one address).
func (s Static) CacheWebhookNetworks() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(s.CacheWebhookAllow))
	for _, v := range s.CacheWebhookAllow {
		if p, err := netip.ParsePrefix(v); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("cache webhook network %q must be a CIDR or an address", v)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// ParseSize parses a size in bytes: a number, or a number with a unit
// (KiB, MiB, GiB, TiB, KB, MB, GB, TB; K, M, G, T are binary).
func ParseSize(v string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(v))
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"tib", 1 << 40}, {"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10},
		{"tb", 1e12}, {"gb", 1e9}, {"mb", 1e6}, {"kb", 1e3},
		{"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1},
	} {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 || n*float64(mult) > math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	return int64(n * float64(mult)), nil
}
