package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The flags of the cache settings, shared by "cache policy create|edit",
// "cache settings" and "dist cache set" (docs/11 §11.10). Durations are
// "90s", "1h30m" or "365d", sizes "512MiB" or bytes; the server validates
// them. Only the flags given change the document.

// specFlags are the flags of the fields of a cache policy.
type specFlags struct {
	ttlDefault, ttlMin, ttlMax                string
	browserCC, query                          string
	keyHeaders, keyCookies                    []string
	respectOrigin, normalize, includeHost     bool
	swr, sie, negative                        string
	bypassAuth                                bool
	bypassCookies, bypassQuery, bypassHeaders []string
	compression                               bool
	algorithms, compressTypes                 []string
	compressMin, minSize, maxSize, blockSize  string
	rangeCaching                              bool
	allowTypes, denyTypes                     []string
}

func (s *specFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&s.ttlDefault, "ttl-default", "", "lifetime in cache of the objects stating none (90s, 1h, 365d)")
	f.StringVar(&s.ttlMin, "ttl-min", "", "shortest lifetime in cache, the objects' own included")
	f.StringVar(&s.ttlMax, "ttl-max", "", "longest lifetime in cache, the objects' own included")
	f.BoolVar(&s.respectOrigin, "respect-origin", false, "use the Cache-Control and Expires of the objects (bounded by --ttl-min/--ttl-max)")
	f.StringVar(&s.browserCC, "browser-cache-control", "", "Cache-Control sent to the clients (\"\" keeps the one of the object)")
	f.StringVar(&s.query, "query", "", "query parameters of the cache key: none, all, allowlist:v,w or denylist:utm_source")
	f.StringArrayVar(&s.keyHeaders, "key-header", nil, "request header entering the cache key (repeatable)")
	f.StringArrayVar(&s.keyCookies, "key-cookie", nil, "cookie entering the cache key (repeatable)")
	f.BoolVar(&s.normalize, "normalize", false, "normalize the key: sorted query, lowercase, no tracking parameters")
	f.BoolVar(&s.includeHost, "include-host", false, "one cache key per domain (false: shared by the aliases)")
	f.StringVar(&s.swr, "stale-while-revalidate", "", "serve an expired entry while refreshing it, up to this long")
	f.StringVar(&s.sie, "stale-if-error", "", "serve an expired entry when the origin fails, up to this long")
	f.StringVar(&s.negative, "negative-ttl", "", "lifetime of the 404 and 403 answers (0s: not cached)")
	f.BoolVar(&s.bypassAuth, "bypass-authorization", false, "skip the cache for the requests with credentials (false: cached per credential)")
	f.StringArrayVar(&s.bypassCookies, "bypass-cookie", nil, "skip the cache for the requests with this cookie (\"*\": any; repeatable)")
	f.StringArrayVar(&s.bypassQuery, "bypass-query", nil, "skip the cache for the requests with this query parameter (repeatable)")
	f.StringArrayVar(&s.bypassHeaders, "bypass-header", nil, "skip the cache for the requests with this header (repeatable)")
	f.BoolVar(&s.compression, "compression", false, "store compressed variants of the textual objects")
	f.StringArrayVar(&s.algorithms, "compress", nil, "compression algorithm, in order of preference: gzip, br, zstd (repeatable)")
	f.StringArrayVar(&s.compressTypes, "compress-type", nil, "compressible media type (text/*; repeatable)")
	f.StringVar(&s.compressMin, "compress-min-size", "", "smallest object compressed")
	f.StringVar(&s.minSize, "min-size", "", "smallest object cached")
	f.StringVar(&s.maxSize, "max-object-size", "", "largest object cached")
	f.BoolVar(&s.rangeCaching, "range-caching", false, "cache the large objects by blocks (range requests, video)")
	f.StringVar(&s.blockSize, "block-size", "", "size of the blocks of the range caching")
	f.StringArrayVar(&s.allowTypes, "allow-type", nil, "media type cached (image/*; repeatable; none: every type)")
	f.StringArrayVar(&s.denyTypes, "deny-type", nil, "media type never cached (repeatable)")
}

// parseQueryString parses the --query flag.
func parseQueryString(v string) (*nexoadmin.CacheQueryString, error) {
	mode, list, _ := strings.Cut(strings.TrimSpace(v), ":")
	q := &nexoadmin.CacheQueryString{Mode: strings.ToLower(strings.TrimSpace(mode)), Params: splitList([]string{list})}
	switch q.Mode {
	case "none", "all":
		if len(q.Params) > 0 {
			return nil, cli.UsageError("--query %s takes no parameter list", q.Mode)
		}
	case "allowlist", "denylist":
		if len(q.Params) == 0 {
			return nil, cli.UsageError("--query %s needs parameters (%s:v,w)", q.Mode, q.Mode)
		}
	default:
		return nil, cli.UsageError("--query %q: expected none, all, allowlist:v,w or denylist:v,w", v)
	}
	return q, nil
}

// apply sets the changed flags on spec; it reports whether a flag was
// given.
func (s *specFlags) apply(cmd *cobra.Command, spec *nexoadmin.CachePolicySpec) (bool, error) {
	f := cmd.Flags()
	changed := false
	set := func(flag string, apply func()) {
		if f.Changed(flag) {
			apply()
			changed = true
		}
	}
	ttl := func() *nexoadmin.CacheTTL {
		if spec.TTL == nil {
			spec.TTL = &nexoadmin.CacheTTL{}
		}
		return spec.TTL
	}
	key := func() *nexoadmin.CacheKeyPolicy {
		if spec.CacheKey == nil {
			spec.CacheKey = &nexoadmin.CacheKeyPolicy{}
		}
		return spec.CacheKey
	}
	bypass := func() *nexoadmin.CacheBypass {
		if spec.Bypass == nil {
			spec.Bypass = &nexoadmin.CacheBypass{}
		}
		return spec.Bypass
	}
	compression := func() *nexoadmin.CacheCompression {
		if spec.Compression == nil {
			spec.Compression = &nexoadmin.CacheCompression{}
		}
		return spec.Compression
	}
	ranges := func() *nexoadmin.CacheRangeCaching {
		if spec.RangeCaching == nil {
			spec.RangeCaching = &nexoadmin.CacheRangeCaching{}
		}
		return spec.RangeCaching
	}
	types := func() *nexoadmin.CacheContentTypes {
		if spec.ContentTypes == nil {
			spec.ContentTypes = &nexoadmin.CacheContentTypes{}
		}
		return spec.ContentTypes
	}
	boolPtr := func(b bool) *bool { return &b }
	set("ttl-default", func() { ttl().Default = s.ttlDefault })
	set("ttl-min", func() { ttl().Min = s.ttlMin })
	set("ttl-max", func() { ttl().Max = s.ttlMax })
	set("respect-origin", func() { spec.RespectObjectCacheControl = boolPtr(s.respectOrigin) })
	set("browser-cache-control", func() { v := s.browserCC; spec.BrowserCacheControl = &v })
	if f.Changed("query") {
		q, err := parseQueryString(s.query)
		if err != nil {
			return false, err
		}
		key().QueryString, changed = q, true
	}
	set("key-header", func() { key().Headers = splitList(s.keyHeaders) })
	set("key-cookie", func() { key().Cookies = splitList(s.keyCookies) })
	set("normalize", func() { key().Normalize = boolPtr(s.normalize) })
	set("include-host", func() { key().IncludeHost = boolPtr(s.includeHost) })
	set("stale-while-revalidate", func() { spec.StaleWhileRevalidate = s.swr })
	set("stale-if-error", func() { spec.StaleIfError = s.sie })
	set("negative-ttl", func() { spec.NegativeCaching = s.negative })
	set("bypass-authorization", func() { bypass().Authorization = boolPtr(s.bypassAuth) })
	set("bypass-cookie", func() { bypass().Cookies = splitList(s.bypassCookies) })
	set("bypass-query", func() { bypass().QueryParams = splitList(s.bypassQuery) })
	set("bypass-header", func() { bypass().Headers = splitList(s.bypassHeaders) })
	set("compression", func() { compression().Enabled = boolPtr(s.compression) })
	set("compress", func() { compression().Algorithms = splitList(s.algorithms) })
	set("compress-type", func() { compression().ContentTypes = splitList(s.compressTypes) })
	set("compress-min-size", func() { compression().MinSize = s.compressMin })
	set("min-size", func() { spec.MinSize = s.minSize })
	set("max-object-size", func() { spec.MaxObjectSize = s.maxSize })
	set("range-caching", func() { ranges().Enabled = boolPtr(s.rangeCaching) })
	set("block-size", func() { ranges().BlockSize = s.blockSize })
	set("allow-type", func() { types().Allow = splitList(s.allowTypes) })
	set("deny-type", func() { types().Deny = splitList(s.denyTypes) })
	return changed, nil
}

// settingsFlags are the flags of the cache settings of a level: the
// policy it names, on/off and the fields it overrides; quota, webhooks
// and autoInvalidate where the level has them.
type settingsFlags struct {
	spec                    specFlags
	policy                  string
	enable, disable         bool
	quota                   string
	webhooks                []string
	autoInvalidate          bool
	withQuota, withWebhooks bool
	withAutoInvalidate      bool
}

func (s *settingsFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&s.policy, "policy", "", "cache policy the settings start from (built-in or defined; \"\" inherits)")
	f.BoolVar(&s.enable, "enable", false, "turn the cache on")
	f.BoolVar(&s.disable, "disable", false, "turn the cache off (always BYPASS)")
	if s.withQuota {
		f.StringVar(&s.quota, "quota", "", "bytes cached at most for the distribution or the domain (20GiB; \"\" for none)")
	}
	if s.withWebhooks {
		f.StringArrayVar(&s.webhooks, "purge-webhook", nil, "purge webhook of the distribution called on the invalidations of the domain (repeatable)")
	}
	if s.withAutoInvalidate {
		f.BoolVar(&s.autoInvalidate, "auto-invalidate", true, "drop the entries of the objects written (default true)")
	}
	s.spec.register(cmd)
}

// apply sets the changed flags on settings and reports whether one was
// given.
func (s *settingsFlags) apply(cmd *cobra.Command, settings *nexoadmin.CacheSettings) (bool, error) {
	f := cmd.Flags()
	if s.enable && s.disable {
		return false, cli.UsageError("--enable and --disable are mutually exclusive")
	}
	changed, err := s.spec.apply(cmd, &settings.CachePolicySpec)
	if err != nil {
		return false, err
	}
	if f.Changed("policy") {
		settings.Policy, changed = s.policy, true
	}
	if f.Changed("enable") || f.Changed("disable") {
		on := s.enable
		settings.Enabled, changed = &on, true
	}
	if s.withQuota && f.Changed("quota") {
		settings.Quota, changed = s.quota, true
	}
	if s.withWebhooks && f.Changed("purge-webhook") {
		settings.PurgeWebhooks, changed = splitList(s.webhooks), true
	}
	if s.withAutoInvalidate && f.Changed("auto-invalidate") {
		v := s.autoInvalidate
		settings.AutoInvalidate, changed = &v, true
	}
	return changed, nil
}

// readSecretFlag returns the value of a secret flag, or the trimmed
// content of its file flag.
func readSecretFlag(cmd *cobra.Command, value, file, flag string) (string, error) {
	if !cmd.Flags().Changed(flag + "-file") {
		return value, nil
	}
	if cmd.Flags().Changed(flag) {
		return "", cli.UsageError("--%s and --%s-file are mutually exclusive", flag, flag)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// --- formatting --------------------------------------------------------------------------

// yesNo renders a boolean setting ("-" when unset).
func yesNo(b *bool) string {
	switch {
	case b == nil:
		return "-"
	case *b:
		return "yes"
	}
	return "no"
}

// specField is a setting of a policy spec and its value.
type specField struct{ name, value string }

// specFields returns the fields set in a policy spec, in the order of the
// policy.
func specFields(s nexoadmin.CachePolicySpec) []specField {
	var out []specField
	add := func(name, v string) {
		if v != "" {
			out = append(out, specField{name, v})
		}
	}
	list := func(name string, v []string) {
		if len(v) > 0 {
			add(name, strings.Join(v, ","))
		}
	}
	flag := func(name string, b *bool) {
		if b != nil {
			add(name, yesNo(b))
		}
	}
	if t := s.TTL; t != nil {
		add("TTL default", t.Default)
		add("TTL min", t.Min)
		add("TTL max", t.Max)
	}
	flag("respect origin", s.RespectObjectCacheControl)
	if s.BrowserCacheControl != nil {
		add("browser cache-control", fmt.Sprintf("%q", *s.BrowserCacheControl))
	}
	if k := s.CacheKey; k != nil {
		if k.QueryString != nil {
			add("key query", formatQuery(k.QueryString))
		}
		list("key headers", k.Headers)
		list("key cookies", k.Cookies)
		flag("key normalize", k.Normalize)
		flag("key per host", k.IncludeHost)
	}
	add("stale-while-revalidate", s.StaleWhileRevalidate)
	add("stale-if-error", s.StaleIfError)
	add("negative TTL", s.NegativeCaching)
	if b := s.Bypass; b != nil {
		flag("bypass always", b.Always)
		flag("bypass authorization", b.Authorization)
		list("bypass cookies", b.Cookies)
		list("bypass query", b.QueryParams)
		list("bypass headers", b.Headers)
	}
	if c := s.Compression; c != nil {
		flag("compression", c.Enabled)
		list("compress", c.Algorithms)
		list("compress types", c.ContentTypes)
		add("compress min size", c.MinSize)
	}
	add("min size", s.MinSize)
	add("max object size", s.MaxObjectSize)
	if r := s.RangeCaching; r != nil {
		flag("range caching", r.Enabled)
		add("block size", r.BlockSize)
	}
	if t := s.ContentTypes; t != nil {
		list("allow types", t.Allow)
		list("deny types", t.Deny)
	}
	return out
}

// formatSpec renders the fields set in a policy spec, "name value" each.
func formatSpec(s nexoadmin.CachePolicySpec) []string {
	fields := specFields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.name+" "+f.value)
	}
	return out
}

// formatQuery renders a query string mode ("allowlist:v,w").
func formatQuery(q *nexoadmin.CacheQueryString) string {
	if len(q.Params) == 0 {
		return q.Mode
	}
	return q.Mode + ":" + strings.Join(q.Params, ",")
}

// formatSettings renders the settings of a level on one line ("inherited"
// when nothing is set).
func formatSettings(s nexoadmin.CacheSettings) string {
	var parts []string
	if s.Enabled != nil {
		parts = append(parts, map[bool]string{true: "enabled", false: "disabled"}[*s.Enabled])
	}
	if s.Policy != "" {
		parts = append(parts, "policy "+s.Policy)
	}
	if s.Quota != "" {
		parts = append(parts, "quota "+s.Quota)
	}
	parts = append(parts, formatSpec(s.CachePolicySpec)...)
	if len(s.PurgeWebhooks) > 0 {
		parts = append(parts, "purge webhooks "+strings.Join(s.PurgeWebhooks, ","))
	}
	if s.AutoInvalidate != nil {
		parts = append(parts, "auto-invalidate "+yesNo(s.AutoInvalidate))
	}
	if len(parts) == 0 {
		return "inherited"
	}
	return strings.Join(parts, ", ")
}
