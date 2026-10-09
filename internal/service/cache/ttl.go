package cache

import (
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// directives are the Cache-Control directives the cache honors.
type directives struct {
	noStore, noCache, private bool
	maxAge, sMaxAge           time.Duration
	hasMaxAge, hasSMaxAge     bool
	swr, sie                  time.Duration
	hasSWR, hasSIE            bool
}

// parseCacheControl parses a Cache-Control value (RFC 9111 §5.2); unknown
// directives and invalid values are ignored.
func parseCacheControl(v string) directives {
	var d directives
	for _, part := range strings.Split(v, ",") {
		name, arg, _ := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.ToLower(strings.TrimSpace(name))
		arg = strings.Trim(strings.TrimSpace(arg), `"`)
		seconds := func() (time.Duration, bool) {
			n, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || n < 0 {
				return 0, false
			}
			if n > int64(100*365*day/time.Second) {
				n = int64(100 * 365 * day / time.Second)
			}
			return time.Duration(n) * time.Second, true
		}
		switch name {
		case "no-store":
			d.noStore = true
		case "no-cache":
			// A no-cache with field names only restricts those headers.
			if arg == "" {
				d.noCache = true
			}
		case "private":
			if arg == "" {
				d.private = true
			}
		case "max-age":
			d.maxAge, d.hasMaxAge = seconds()
		case "s-maxage":
			d.sMaxAge, d.hasSMaxAge = seconds()
		case "stale-while-revalidate":
			d.swr, d.hasSWR = seconds()
		case "stale-if-error":
			d.sie, d.hasSIE = seconds()
		}
	}
	return d
}

// freshness is how long an object stays fresh and usable stale.
type freshness struct {
	ttl, swr, sie time.Duration
	// cacheable is false for the objects that forbid a shared cache
	// (no-store, private) when the policy respects their Cache-Control.
	cacheable bool
}

// freshnessOf computes the lifetime of an object under a policy (TCC-04):
// its own Cache-Control (s-maxage over max-age) or Expires when the
// policy respects them, the default otherwise, always bounded by the
// minimum and the maximum of the policy.
func freshnessOf(p *params, info domain.ObjectInfo, now time.Time) freshness {
	f := freshness{ttl: p.TTLDefault, swr: p.SWR, sie: p.SIE, cacheable: true}
	if p.RespectCC {
		d := parseCacheControl(info.CacheControl)
		if d.noStore || d.private {
			return freshness{}
		}
		switch {
		case d.noCache:
			f.ttl = 0
		case d.hasSMaxAge:
			f.ttl = d.sMaxAge
		case d.hasMaxAge:
			f.ttl = d.maxAge
		case !info.Expires.IsZero():
			f.ttl = max(info.Expires.Sub(now), 0)
		}
		if d.hasSWR {
			f.swr = d.swr
		}
		if d.hasSIE {
			f.sie = d.sie
		}
	}
	f.ttl = min(max(f.ttl, p.TTLMin), p.TTLMax)
	return f
}
