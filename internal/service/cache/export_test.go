package cache

import (
	"context"
	"net/netip"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// Exports of the unexported helpers for the external tests.

// Params are the flattened settings of an effective policy.
type Params = params

// Level settings of a resolution.
type LevelSettings = levelSettings

// NewLevel returns the settings of a level.
func NewLevel(l Level, s *Settings) LevelSettings { return levelSettings{level: l, s: s} }

// ParamsOf flattens a policy completed with the standard one.
func ParamsOf(spec PolicySpec) Params { return paramsOf(complete(spec), true, true) }

// Resolve resolves levels over the built-in policies plus extra.
func Resolve(extra map[string]Policy, levels ...LevelSettings) EffectivePolicy {
	ps := builtinPolicies()
	for k, v := range extra {
		v.PolicySpec = complete(v.PolicySpec)
		ps[k] = v
	}
	return resolve(levels, ps)
}

// BuiltinPolicies returns the predefined policies.
func BuiltinPolicies() map[string]Policy { return builtinPolicies() }

// KeyOf computes a cache key.
func KeyOf(p *Params, scope, bucket, object string, req *Request, credential string) string {
	return keyOf(p, scope, bucket, object, req, credential)
}

// ScopeOf computes the scope of a key.
func ScopeOf(p *Params, dist, host string) string { return scopeOf(p, dist, host) }

// FreshnessOf computes the lifetime of an object.
func FreshnessOf(p *Params, info domain.ObjectInfo, now time.Time) (ttl, swr, sie time.Duration, cacheable bool) {
	f := freshnessOf(p, info, now)
	return f.ttl, f.swr, f.sie, f.cacheable
}

// Negotiate picks a content encoding.
func Negotiate(accept string, algorithms []string) string { return negotiate(accept, algorithms) }

// Compress encodes a body.
func Compress(enc string, body []byte) ([]byte, error) { return compress(enc, body) }

// BypassReason tells why a request skips the cache.
func BypassReason(p *Params, req *Request) string { return bypassReason(p, req) }

// LiteralPrefix converts the patterns for the webhooks.
var LiteralPrefix = literalPrefix

// PatternRegex is the regular expression of a pattern for the Varnish
// bans.
func PatternRegex(s Service, p distribution.PathPattern) string {
	return s.(*service).patternRegex(context.Background(), p)
}

// Owner returns the node owning a key (shared mode).
func Owner(s Service, key string) string {
	if p := s.(*service).ownerOf(key); p != nil {
		return p.NodeID()
	}
	return s.(*service).deps.NodeID
}

// SyncPeers runs a comparison of the sequences with the peers.
func SyncPeers(s Service) { s.(*service).syncPeers(s.(*service).lifeContext()) }

// RetryLinks sends the invalidations the peers missed.
func RetryLinks(s Service) { s.(*service).retryLinks(s.(*service).lifeContext()) }

// Trusted reports whether the node trusts its entries.
func Trusted(s Service) bool { return s.(*service).trusted() }

// Entries is the number of entries of the store.
func Entries(s Service) int { return s.(*service).store.usage().entries }

// DiskDir is the directory of the files of the disk tier.
func DiskDir(s Service) string {
	if d := s.(*service).store.deps.disk; d != nil {
		return d.dir
	}
	return ""
}

// WaitDisk waits for the queued disk writes.
func WaitDisk(s Service) {
	st := s.(*service).store
	for st.pending.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
}

// SendWebhooks sends the due purges at once.
func SendWebhooks(s Service) { s.(*service).sendDue(s.(*service).lifeContext()) }

// ScopeBytes returns the bytes of a quota scope.
func ScopeBytes(s Service, id string) int64 { return s.(*service).store.scopeBytes(id) }

// Reload reloads the configuration from the system store.
func Reload(s Service) error { return s.(*service).Reload(context.Background()) }

// EgressCheck applies the egress policy of the webhooks to an address
// ("ip:port") as the dialer does.
func EgressCheck(s Service, addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return err
	}
	return s.(*service).egress.checkAddr(ap)
}
