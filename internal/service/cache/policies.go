package cache

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// Limits of the policies.
const (
	minBlockSize           = 64 << 10
	maxBlockSize           = 32 << 20
	defaultBlockSize       = 8 << 20
	maxBrowserCacheControl = 1024
	maxNameLength          = 64
	maxListLength          = 64
	// passTTL is how long an object found uncacheable is remembered
	// (hit-for-pass).
	passTTL = 10 * time.Second
)

// textTypes are the media types compressed by default.
func textTypes() []string {
	return []string{
		"text/*", "application/javascript", "application/x-javascript", "application/json", "application/ld+json",
		"application/manifest+json", "application/xml", "application/xhtml+xml", "application/rss+xml", "application/atom+xml",
		"application/wasm", "image/svg+xml", "font/ttf", "font/otf", "application/vnd.ms-fontobject",
	}
}

func dur(d time.Duration) *Duration { return ptr(Duration(d)) }
func size(n int64) *Size            { return ptr(Size(n)) }

// standardSpec is the complete standard policy (docs/11 §11.2 defaults):
// every other policy and level starts from it.
func standardSpec() PolicySpec {
	return PolicySpec{
		TTL:                       &TTL{Default: dur(time.Hour), Min: dur(0), Max: dur(7 * day)},
		RespectObjectCacheControl: ptr(true),
		BrowserCacheControl:       ptr(""),
		CacheKey: &KeyPolicy{QueryString: &QueryString{Mode: QueryNone}, Headers: []string{}, Cookies: []string{},
			Normalize: ptr(true), IncludeHost: ptr(true)},
		StaleWhileRevalidate: dur(time.Minute),
		StaleIfError:         dur(day),
		NegativeCaching:      dur(10 * time.Second),
		Bypass: &Bypass{Always: ptr(false), Authorization: ptr(true), Cookies: []string{}, QueryParams: []string{},
			Headers: []string{}},
		Compression: &Compression{Enabled: ptr(true), Algorithms: []string{EncodingGzip, EncodingBrotli},
			ContentTypes: textTypes(), MinSize: size(1 << 10)},
		MinSize:       size(0),
		MaxObjectSize: size(1 << 30),
		RangeCaching:  &RangeCaching{Enabled: ptr(true), BlockSize: size(defaultBlockSize)},
		ContentTypes:  &ContentTypes{Allow: []string{}, Deny: []string{}},
	}
}

// builtinPolicies returns the predefined policies, complete.
func builtinPolicies() map[string]Policy {
	out := map[string]Policy{}
	add := func(name, comment string, over PolicySpec) {
		spec := standardSpec()
		overlay(&spec, over, "", nil)
		out[name] = Policy{Name: name, Comment: comment, Builtin: true, PolicySpec: spec}
	}
	add(PolicyStandard, "Default: 1h fresh (bounded to 7d), the Cache-Control of the objects respected.", PolicySpec{})
	add(PolicyDisabled, "No caching: every request is served by the origin.", PolicySpec{Bypass: &Bypass{Always: ptr(true)}})
	add(PolicyAggressive, "Long lifetimes (1d, at least 1h, at most 30d), long stale windows, zstd too.", PolicySpec{
		TTL:                  &TTL{Default: dur(day), Min: dur(time.Hour), Max: dur(30 * day)},
		StaleWhileRevalidate: dur(time.Hour),
		StaleIfError:         dur(7 * day),
		NegativeCaching:      dur(time.Minute),
		Compression:          &Compression{Algorithms: []string{EncodingBrotli, EncodingZstd, EncodingGzip}},
	})
	add(PolicyImmutableAssets, "Versioned assets: one year in cache and in the browsers.", PolicySpec{
		TTL:                       &TTL{Default: dur(365 * day), Min: dur(30 * day), Max: dur(365 * day)},
		RespectObjectCacheControl: ptr(false),
		BrowserCacheControl:       ptr("public, max-age=31536000, immutable"),
		StaleWhileRevalidate:      dur(day),
		StaleIfError:              dur(30 * day),
		NegativeCaching:           dur(time.Minute),
	})
	add(PolicyRespectOrigin, "Only the lifetime the objects state (Cache-Control, Expires); none means revalidate.", PolicySpec{
		TTL:                       &TTL{Default: dur(0), Min: dur(0), Max: dur(365 * day)},
		RespectObjectCacheControl: ptr(true),
	})
	return out
}

// complete fills the unset fields of spec from the standard policy.
func complete(spec PolicySpec) PolicySpec {
	out := standardSpec()
	overlay(&out, spec, "", nil)
	return out
}

// overlay copies the set fields of src over dst; fields records the level
// of every field copied (nil: not recorded).
func overlay(dst *PolicySpec, src PolicySpec, level Level, fields map[string]Level) {
	set := func(name string) {
		if fields != nil {
			fields[name] = level
		}
	}
	if t := src.TTL; t != nil {
		if dst.TTL == nil {
			dst.TTL = &TTL{}
		}
		if t.Default != nil {
			dst.TTL.Default = ptr(*t.Default)
			set("ttl.default")
		}
		if t.Min != nil {
			dst.TTL.Min = ptr(*t.Min)
			set("ttl.min")
		}
		if t.Max != nil {
			dst.TTL.Max = ptr(*t.Max)
			set("ttl.max")
		}
	}
	if src.RespectObjectCacheControl != nil {
		dst.RespectObjectCacheControl = ptr(*src.RespectObjectCacheControl)
		set("respectObjectCacheControl")
	}
	if src.BrowserCacheControl != nil {
		dst.BrowserCacheControl = ptr(*src.BrowserCacheControl)
		set("browserCacheControl")
	}
	if k := src.CacheKey; k != nil {
		if dst.CacheKey == nil {
			dst.CacheKey = &KeyPolicy{}
		}
		if k.QueryString != nil {
			dst.CacheKey.QueryString = &QueryString{Mode: k.QueryString.Mode, Params: slices.Clone(k.QueryString.Params)}
			set("cacheKey.queryString")
		}
		if k.Headers != nil {
			dst.CacheKey.Headers = slices.Clone(k.Headers)
			set("cacheKey.headers")
		}
		if k.Cookies != nil {
			dst.CacheKey.Cookies = slices.Clone(k.Cookies)
			set("cacheKey.cookies")
		}
		if k.Normalize != nil {
			dst.CacheKey.Normalize = ptr(*k.Normalize)
			set("cacheKey.normalize")
		}
		if k.IncludeHost != nil {
			dst.CacheKey.IncludeHost = ptr(*k.IncludeHost)
			set("cacheKey.includeHost")
		}
	}
	if src.StaleWhileRevalidate != nil {
		dst.StaleWhileRevalidate = ptr(*src.StaleWhileRevalidate)
		set("staleWhileRevalidate")
	}
	if src.StaleIfError != nil {
		dst.StaleIfError = ptr(*src.StaleIfError)
		set("staleIfError")
	}
	if src.NegativeCaching != nil {
		dst.NegativeCaching = ptr(*src.NegativeCaching)
		set("negativeCaching")
	}
	if b := src.Bypass; b != nil {
		if dst.Bypass == nil {
			dst.Bypass = &Bypass{}
		}
		if b.Always != nil {
			dst.Bypass.Always = ptr(*b.Always)
			set("bypass.always")
		}
		if b.Authorization != nil {
			dst.Bypass.Authorization = ptr(*b.Authorization)
			set("bypass.authorization")
		}
		if b.Cookies != nil {
			dst.Bypass.Cookies = slices.Clone(b.Cookies)
			set("bypass.cookies")
		}
		if b.QueryParams != nil {
			dst.Bypass.QueryParams = slices.Clone(b.QueryParams)
			set("bypass.queryParams")
		}
		if b.Headers != nil {
			dst.Bypass.Headers = slices.Clone(b.Headers)
			set("bypass.headers")
		}
	}
	if c := src.Compression; c != nil {
		if dst.Compression == nil {
			dst.Compression = &Compression{}
		}
		if c.Enabled != nil {
			dst.Compression.Enabled = ptr(*c.Enabled)
			set("compression.enabled")
		}
		if c.Algorithms != nil {
			dst.Compression.Algorithms = slices.Clone(c.Algorithms)
			set("compression.algorithms")
		}
		if c.ContentTypes != nil {
			dst.Compression.ContentTypes = slices.Clone(c.ContentTypes)
			set("compression.contentTypes")
		}
		if c.MinSize != nil {
			dst.Compression.MinSize = ptr(*c.MinSize)
			set("compression.minSize")
		}
	}
	if src.MinSize != nil {
		dst.MinSize = ptr(*src.MinSize)
		set("minSize")
	}
	if src.MaxObjectSize != nil {
		dst.MaxObjectSize = ptr(*src.MaxObjectSize)
		set("maxObjectSize")
	}
	if r := src.RangeCaching; r != nil {
		if dst.RangeCaching == nil {
			dst.RangeCaching = &RangeCaching{}
		}
		if r.Enabled != nil {
			dst.RangeCaching.Enabled = ptr(*r.Enabled)
			set("rangeCaching.enabled")
		}
		if r.BlockSize != nil {
			dst.RangeCaching.BlockSize = ptr(*r.BlockSize)
			set("rangeCaching.blockSize")
		}
	}
	if c := src.ContentTypes; c != nil {
		if dst.ContentTypes == nil {
			dst.ContentTypes = &ContentTypes{}
		}
		if c.Allow != nil {
			dst.ContentTypes.Allow = slices.Clone(c.Allow)
			set("contentTypes.allow")
		}
		if c.Deny != nil {
			dst.ContentTypes.Deny = slices.Clone(c.Deny)
			set("contentTypes.deny")
		}
	}
}

// policyFields are the names of every field of a policy, for the
// provenance of the effective policy.
func policyFields() []string {
	return []string{
		"ttl.default", "ttl.min", "ttl.max", "respectObjectCacheControl", "browserCacheControl",
		"cacheKey.queryString", "cacheKey.headers", "cacheKey.cookies", "cacheKey.normalize", "cacheKey.includeHost",
		"staleWhileRevalidate", "staleIfError", "negativeCaching",
		"bypass.always", "bypass.authorization", "bypass.cookies", "bypass.queryParams", "bypass.headers",
		"compression.enabled", "compression.algorithms", "compression.contentTypes", "compression.minSize",
		"minSize", "maxObjectSize", "rangeCaching.enabled", "rangeCaching.blockSize", "contentTypes.allow", "contentTypes.deny",
	}
}

// params are the settings of an effective policy, flattened: what the
// request path reads (and what a node sends to the owner of a key in the
// shared mode).
type params struct {
	Enabled        bool          `json:"enabled"`
	TTLDefault     time.Duration `json:"ttlDefault"`
	TTLMin         time.Duration `json:"ttlMin"`
	TTLMax         time.Duration `json:"ttlMax"`
	RespectCC      bool          `json:"respectCC"`
	BrowserCC      string        `json:"browserCC,omitempty"`
	Query          string        `json:"query"`
	QueryParams    []string      `json:"queryParams,omitempty"`
	KeyHeaders     []string      `json:"keyHeaders,omitempty"`
	KeyCookies     []string      `json:"keyCookies,omitempty"`
	Normalize      bool          `json:"normalize"`
	IncludeHost    bool          `json:"includeHost"`
	SWR            time.Duration `json:"swr"`
	SIE            time.Duration `json:"sie"`
	Negative       time.Duration `json:"negative"`
	BypassAuth     bool          `json:"bypassAuth"`
	BypassCookies  []string      `json:"bypassCookies,omitempty"`
	BypassQuery    []string      `json:"bypassQuery,omitempty"`
	BypassHeaders  []string      `json:"bypassHeaders,omitempty"`
	Compression    bool          `json:"compression"`
	Algorithms     []string      `json:"algorithms,omitempty"`
	CompressTypes  []string      `json:"compressTypes,omitempty"`
	CompressMin    int64         `json:"compressMin"`
	MinSize        int64         `json:"minSize"`
	MaxSize        int64         `json:"maxSize"`
	RangeCaching   bool          `json:"rangeCaching"`
	BlockSize      int64         `json:"blockSize"`
	AllowTypes     []string      `json:"allowTypes,omitempty"`
	DenyTypes      []string      `json:"denyTypes,omitempty"`
	AutoInvalidate bool          `json:"autoInvalidate"`
}

// paramsOf flattens a complete policy.
func paramsOf(spec PolicySpec, enabled, autoInvalidate bool) params {
	p := params{
		Enabled:        enabled && !boolOf(spec.Bypass.Always, false),
		TTLDefault:     spec.TTL.Default.D(),
		TTLMin:         spec.TTL.Min.D(),
		TTLMax:         spec.TTL.Max.D(),
		RespectCC:      *spec.RespectObjectCacheControl,
		BrowserCC:      *spec.BrowserCacheControl,
		Query:          spec.CacheKey.QueryString.Mode,
		Normalize:      *spec.CacheKey.Normalize,
		IncludeHost:    *spec.CacheKey.IncludeHost,
		SWR:            spec.StaleWhileRevalidate.D(),
		SIE:            spec.StaleIfError.D(),
		Negative:       spec.NegativeCaching.D(),
		BypassAuth:     *spec.Bypass.Authorization,
		Compression:    *spec.Compression.Enabled,
		Algorithms:     spec.Compression.Algorithms,
		CompressMin:    int64(*spec.Compression.MinSize),
		MinSize:        int64(*spec.MinSize),
		MaxSize:        int64(*spec.MaxObjectSize),
		RangeCaching:   *spec.RangeCaching.Enabled,
		BlockSize:      int64(*spec.RangeCaching.BlockSize),
		AutoInvalidate: autoInvalidate,
	}
	for _, q := range spec.CacheKey.QueryString.Params {
		p.QueryParams = append(p.QueryParams, strings.ToLower(q))
	}
	for _, h := range spec.CacheKey.Headers {
		p.KeyHeaders = append(p.KeyHeaders, http.CanonicalHeaderKey(h))
	}
	p.KeyCookies = slices.Clone(spec.CacheKey.Cookies)
	for _, q := range spec.Bypass.QueryParams {
		p.BypassQuery = append(p.BypassQuery, strings.ToLower(q))
	}
	p.BypassCookies = slices.Clone(spec.Bypass.Cookies)
	for _, h := range spec.Bypass.Headers {
		p.BypassHeaders = append(p.BypassHeaders, http.CanonicalHeaderKey(h))
	}
	for _, t := range spec.Compression.ContentTypes {
		p.CompressTypes = append(p.CompressTypes, strings.ToLower(t))
	}
	for _, t := range spec.ContentTypes.Allow {
		p.AllowTypes = append(p.AllowTypes, strings.ToLower(t))
	}
	for _, t := range spec.ContentTypes.Deny {
		p.DenyTypes = append(p.DenyTypes, strings.ToLower(t))
	}
	if p.BlockSize < minBlockSize {
		p.BlockSize = defaultBlockSize
	}
	return p
}

// levelSettings is the settings of one level of a resolution.
type levelSettings struct {
	level Level
	s     *Settings
}

// resolution is an effective policy: the public view and the flattened
// parameters.
type resolution struct {
	eff EffectivePolicy
	p   params
	// distQuota and domainQuota are the quotas, domainKey the domain
	// settings matched ("" when none).
	distQuota   int64
	domainQuota int64
	domainKey   string
}

// resolve applies the levels, from the most general, over the standard
// policy. A level naming an unknown policy keeps the previous settings
// (the configuration refuses such names; a policy removed meanwhile
// should not disable the cache).
func resolve(levels []levelSettings, policies map[string]Policy) EffectivePolicy {
	spec := standardSpec()
	fields := map[string]Level{}
	for _, f := range policyFields() {
		fields[f] = LevelDefault
	}
	eff := EffectivePolicy{Enabled: true, EnabledLevel: LevelDefault, PolicyName: PolicyStandard, PolicyLevel: LevelDefault,
		Behavior: -1, AutoInvalidate: true}
	for _, l := range levels {
		if l.s == nil {
			continue
		}
		if l.s.Enabled != nil {
			eff.Enabled, eff.EnabledLevel = *l.s.Enabled, l.level
		}
		if l.s.Policy != "" {
			if p, ok := policies[l.s.Policy]; ok {
				spec = complete(p.PolicySpec)
				for _, f := range policyFields() {
					fields[f] = l.level
				}
				eff.PolicyName, eff.PolicyLevel = l.s.Policy, l.level
			}
		}
		overlay(&spec, l.s.PolicySpec, l.level, fields)
		if l.s.AutoInvalidate != nil && (l.level == LevelGlobal || l.level == LevelDistribution) {
			eff.AutoInvalidate = *l.s.AutoInvalidate
		}
	}
	eff.Policy = spec
	eff.Fields = fields
	eff.CacheControl = *spec.BrowserCacheControl
	if boolOf(spec.Bypass.Always, false) {
		eff.Enabled = false
	}
	return eff
}

// --- validation -------------------------------------------------------------------

// validName reports a policy name or an id: lowercase letters, digits,
// '.', '_' and '-', at most 64, starting with a letter or a digit.
func validName(s string) bool {
	if s == "" || len(s) > maxNameLength {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '.' || c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// validToken reports an HTTP token (header and cookie names).
func validToken(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c >= 0x7f || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, c) {
			return false
		}
	}
	return true
}

// validMediaPattern reports "type/subtype" with "*" wildcards.
func validMediaPattern(s string) bool {
	t, sub, ok := strings.Cut(s, "/")
	return ok && t != "" && sub != "" && validToken(strings.ReplaceAll(t, "*", "x")) && validToken(strings.ReplaceAll(sub, "*", "x"))
}

func validateQueryString(q QueryString) error {
	switch q.Mode {
	case QueryNone, QueryAll, QueryAllowlist, QueryDenylist:
	default:
		return fmt.Errorf("%w: cacheKey.queryString mode %q must be none, all, allowlist or denylist", ErrInvalid, q.Mode)
	}
	if len(q.Params) > maxListLength {
		return fmt.Errorf("%w: cacheKey.queryString: at most %d parameters", ErrInvalid, maxListLength)
	}
	for _, p := range q.Params {
		if p == "" || strings.ContainsAny(p, "&= \t\r\n") {
			return fmt.Errorf("%w: cacheKey.queryString: invalid parameter %q", ErrInvalid, p)
		}
	}
	return nil
}

// validateSpec checks the set fields of a policy or of the overrides of a
// level (where names the field in the errors).
func validateSpec(where string, s PolicySpec) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s"+format, append([]any{ErrInvalid, where}, args...)...)
	}
	if t := s.TTL; t != nil {
		for name, d := range map[string]*Duration{"ttl.default": t.Default, "ttl.min": t.Min, "ttl.max": t.Max} {
			if d != nil && *d < 0 {
				return bad("%s must not be negative", name)
			}
		}
		if t.Min != nil && t.Default != nil && *t.Min > *t.Default {
			return bad("ttl.min %s is above ttl.default %s", t.Min, t.Default)
		}
		if t.Default != nil && t.Max != nil && *t.Default > *t.Max {
			return bad("ttl.default %s is above ttl.max %s", t.Default, t.Max)
		}
		if t.Min != nil && t.Max != nil && *t.Min > *t.Max {
			return bad("ttl.min %s is above ttl.max %s", t.Min, t.Max)
		}
	}
	if c := s.BrowserCacheControl; c != nil && (len(*c) > maxBrowserCacheControl || strings.ContainsAny(*c, "\r\n\x00")) {
		return bad("browserCacheControl is not a valid header value")
	}
	if k := s.CacheKey; k != nil {
		if k.QueryString != nil {
			if err := validateQueryString(*k.QueryString); err != nil {
				return fmt.Errorf("%s%w", where, err)
			}
		}
		if len(k.Headers) > maxListLength || len(k.Cookies) > maxListLength {
			return bad("cacheKey: at most %d headers and cookies", maxListLength)
		}
		for _, h := range k.Headers {
			if !validToken(h) {
				return bad("cacheKey.headers: invalid header %q", h)
			}
			switch http.CanonicalHeaderKey(h) {
			case "Authorization", "Cookie", "Proxy-Authorization":
				// The credentials never key shared entries: the
				// bypass.authorization setting decides about them.
				return bad("cacheKey.headers: %s cannot be part of the key", h)
			}
		}
		for _, c := range k.Cookies {
			if !validToken(c) {
				return bad("cacheKey.cookies: invalid cookie %q", c)
			}
		}
	}
	for name, d := range map[string]*Duration{"staleWhileRevalidate": s.StaleWhileRevalidate, "staleIfError": s.StaleIfError,
		"negativeCaching": s.NegativeCaching} {
		if d != nil && *d < 0 {
			return bad("%s must not be negative", name)
		}
	}
	if b := s.Bypass; b != nil {
		if len(b.Cookies)+len(b.QueryParams)+len(b.Headers) > 3*maxListLength {
			return bad("bypass: too many conditions")
		}
		for _, c := range b.Cookies {
			if c != "*" && !validToken(c) {
				return bad("bypass.cookies: invalid cookie %q", c)
			}
		}
		for _, h := range b.Headers {
			if !validToken(h) {
				return bad("bypass.headers: invalid header %q", h)
			}
		}
		for _, q := range b.QueryParams {
			if q == "" || strings.ContainsAny(q, "&= ") {
				return bad("bypass.queryParams: invalid parameter %q", q)
			}
		}
	}
	if c := s.Compression; c != nil {
		seen := map[string]bool{}
		for _, a := range c.Algorithms {
			switch a {
			case EncodingGzip, EncodingBrotli, EncodingZstd:
			default:
				return bad("compression.algorithms: %q is not gzip, br or zstd", a)
			}
			if seen[a] {
				return bad("compression.algorithms: %s listed twice", a)
			}
			seen[a] = true
		}
		for _, t := range c.ContentTypes {
			if !validMediaPattern(t) {
				return bad("compression.contentTypes: invalid media type %q", t)
			}
		}
		if c.MinSize != nil && *c.MinSize < 0 {
			return bad("compression.minSize must not be negative")
		}
	}
	if s.MinSize != nil && *s.MinSize < 0 {
		return bad("minSize must not be negative")
	}
	if s.MaxObjectSize != nil && *s.MaxObjectSize < 0 {
		return bad("maxObjectSize must not be negative")
	}
	if s.MinSize != nil && s.MaxObjectSize != nil && *s.MinSize > *s.MaxObjectSize {
		return bad("minSize %s is above maxObjectSize %s", s.MinSize, s.MaxObjectSize)
	}
	if r := s.RangeCaching; r != nil && r.BlockSize != nil && (*r.BlockSize < minBlockSize || *r.BlockSize > maxBlockSize) {
		return bad("rangeCaching.blockSize must be between %s and %s", Size(minBlockSize), Size(maxBlockSize))
	}
	if c := s.ContentTypes; c != nil {
		for _, t := range append(slices.Clone(c.Allow), c.Deny...) {
			if !validMediaPattern(t) {
				return bad("contentTypes: invalid media type %q", t)
			}
		}
	}
	return nil
}

// validateSettings checks the settings of a level against the known
// policies and webhooks.
func validateSettings(where string, s Settings, policies map[string]Policy, webhooks map[string]bool) error {
	if s.Policy != "" {
		if _, ok := policies[s.Policy]; !ok {
			return fmt.Errorf("%w: %spolicy %q does not exist", ErrInvalid, where, s.Policy)
		}
	}
	if s.Quota != nil && *s.Quota < 0 {
		return fmt.Errorf("%w: %squota must not be negative", ErrInvalid, where)
	}
	for _, w := range s.PurgeWebhooks {
		if !webhooks[w] {
			return fmt.Errorf("%w: %spurgeWebhooks: webhook %q does not exist", ErrInvalid, where, w)
		}
	}
	return validateSpec(where, s.PolicySpec)
}

// validatePolicy checks a policy to store and returns it complete.
func validatePolicy(p Policy) (Policy, error) {
	if !validName(p.Name) {
		return p, fmt.Errorf("%w: policy name %q: lowercase letters, digits, '.', '_' and '-', at most %d", ErrInvalid, p.Name, maxNameLength)
	}
	if len(p.Comment) > 1024 {
		return p, fmt.Errorf("%w: comment over 1024 characters", ErrInvalid)
	}
	if err := validateSpec("", p.PolicySpec); err != nil {
		return p, err
	}
	p.PolicySpec = complete(p.PolicySpec)
	// The completed policy must still be ordered (an override of one
	// bound against the standard others).
	if err := validateSpec("", PolicySpec{TTL: p.TTL}); err != nil {
		return p, err
	}
	p.Builtin = false
	return p, nil
}

// previewOf describes a complete policy.
func previewOf(p Policy) PolicyPreview {
	spec := p.PolicySpec
	out := PolicyPreview{Policy: p, CacheControl: *spec.BrowserCacheControl}
	if out.CacheControl == "" {
		out.Inherited = true
	}
	if boolOf(spec.Bypass.Always, false) {
		out.Summary = "no caching: every request goes to the origin"
		return out
	}
	parts := []string{fmt.Sprintf("fresh %s (min %s, max %s)", spec.TTL.Default, spec.TTL.Min, spec.TTL.Max)}
	if *spec.RespectObjectCacheControl {
		parts = append(parts, "object Cache-Control respected")
	}
	parts = append(parts, "stale-while-revalidate "+spec.StaleWhileRevalidate.String(), "stale-if-error "+spec.StaleIfError.String(),
		"404/403 "+spec.NegativeCaching.String(), "query "+spec.CacheKey.QueryString.String())
	if *spec.Compression.Enabled && len(spec.Compression.Algorithms) > 0 {
		parts = append(parts, "compression "+strings.Join(spec.Compression.Algorithms, "+"))
	}
	if *spec.RangeCaching.Enabled {
		parts = append(parts, "blocks of "+spec.RangeCaching.BlockSize.String())
	}
	out.Summary = strings.Join(parts, ", ")
	return out
}

// matchMedia reports whether the media type ct (parameters ignored)
// matches one of the patterns.
func matchMedia(ct string, patterns []string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	t, sub, _ := strings.Cut(ct, "/")
	for _, p := range patterns {
		pt, psub, _ := strings.Cut(p, "/")
		if (pt == "*" || pt == t) && (psub == "*" || psub == sub || (strings.HasSuffix(psub, "*") && strings.HasPrefix(sub, strings.TrimSuffix(psub, "*")))) {
			return true
		}
	}
	return false
}

// varyHeaders are the request headers the keys of the policy depend on:
// the responses name them in Vary.
func (p *params) varyHeaders() []string {
	if len(p.KeyHeaders) == 0 && len(p.KeyCookies) == 0 {
		return nil
	}
	out := slices.Clone(p.KeyHeaders)
	if len(p.KeyCookies) > 0 {
		out = append(out, "Cookie")
	}
	return out
}

// typeAllowed applies the content type filter of a policy.
func (p *params) typeAllowed(ct string) bool {
	if len(p.DenyTypes) > 0 && matchMedia(ct, p.DenyTypes) {
		return false
	}
	return len(p.AllowTypes) == 0 || matchMedia(ct, p.AllowTypes)
}

// compressible reports whether an object of this type is compressed.
func (p *params) compressible(ct string) bool {
	return p.Compression && len(p.Algorithms) > 0 && matchMedia(ct, p.CompressTypes)
}

// hostMatches reports whether host is the domain d: equal, or below the
// wildcard alias "*.<suffix>".
func hostMatches(d, host string) bool {
	if suffix, ok := strings.CutPrefix(d, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return d == host
}

// pathPatterns are compiled path patterns, matched against the normalized
// request paths (match.Service.Normalize: "/a//b" and "/x/../a/b" are
// "/a/b"), like the behaviors of the distributions.
type pathPatterns struct {
	paths    match.Service
	matchers []match.Matcher
}

// first returns the index of the first pattern matching path (-1: none).
func (p pathPatterns) first(ctx context.Context, path string) int {
	if len(p.matchers) == 0 {
		return -1
	}
	key := p.paths.Normalize(ctx, path)
	for i, m := range p.matchers {
		if m.Match(key).Matched {
			return i
		}
	}
	return -1
}

// any reports whether a pattern matches path.
func (p pathPatterns) any(ctx context.Context, path string) bool { return p.first(ctx, path) >= 0 }

// compilePatterns compiles path patterns.
func compilePatterns(ctx context.Context, m match.Service, ps []distribution.PathPattern, where string) (pathPatterns, error) {
	out := pathPatterns{paths: m, matchers: make([]match.Matcher, 0, len(ps))}
	for i, p := range ps {
		if p.Pattern == "" {
			return pathPatterns{}, fmt.Errorf("%w: %s[%d]: the pattern is required", ErrInvalid, where, i)
		}
		c, err := m.Compile(ctx, p)
		if err != nil {
			return pathPatterns{}, fmt.Errorf("%w: %s[%d]: %v", ErrInvalid, where, i, err)
		}
		out.matchers = append(out.matchers, c)
	}
	return out, nil
}
