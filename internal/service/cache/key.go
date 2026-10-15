package cache

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// keyVersion prefixes the cache keys: a change of the key format changes
// it, so that no entry of another format is ever matched (the disk tier
// drops the entries of another format at startup).
const keyVersion = "v2"

// trackingParam reports a tracking parameter, dropped by the
// normalization of the query.
func trackingParam(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "fbclid", "gclid", "dclid", "msclkid", "mc_eid":
		return true
	}
	return strings.HasPrefix(n, "utm_")
}

// signatureParam reports a parameter of a signature (signed URLs of the
// distributions, presigned S3 URLs): verified before the lookup, never
// part of the key.
func signatureParam(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "expires", "signature", "key-pair-id", "policy":
		return true
	}
	return strings.HasPrefix(n, "x-amz-")
}

// queryPair is a parameter of the query, decoded.
type queryPair struct{ name, value string }

// parseQuery splits a raw query into its decoded parameters, in order.
func parseQuery(raw string) []queryPair {
	var out []queryPair
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		if n, err := url.QueryUnescape(name); err == nil {
			name = n
		}
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		out = append(out, queryPair{name: name, value: value})
	}
	return out
}

// queryKey is the part of the query that keys the entries (TCC-03).
func queryKey(p *params, raw string) string {
	if p.Query == QueryNone || raw == "" {
		return ""
	}
	var kept []queryPair
	for _, q := range parseQuery(raw) {
		lower := strings.ToLower(q.name)
		switch {
		case signatureParam(q.name), slices.Contains(p.BypassQuery, lower):
			continue
		case p.Normalize && trackingParam(q.name):
			continue
		case p.Query == QueryAllowlist && !slices.Contains(p.QueryParams, lower):
			continue
		case p.Query == QueryDenylist && slices.Contains(p.QueryParams, lower):
			continue
		}
		if p.Normalize {
			q.name = lower
		}
		kept = append(kept, q)
	}
	if p.Normalize {
		slices.SortStableFunc(kept, func(a, b queryPair) int {
			if c := strings.Compare(a.name, b.name); c != 0 {
				return c
			}
			return strings.Compare(a.value, b.value)
		})
	}
	parts := make([]string, 0, len(kept))
	for _, q := range kept {
		parts = append(parts, url.QueryEscape(q.name)+"="+url.QueryEscape(q.value))
	}
	return strings.Join(parts, "&")
}

// cookiesOf parses the cookies of a request.
func cookiesOf(h http.Header) []*http.Cookie {
	if len(h.Values("Cookie")) == 0 {
		return nil
	}
	return (&http.Request{Header: h}).Cookies()
}

// keyOf is the cache key of an object for a request: the scope (the host,
// or the distribution when the aliases share the entries), the object,
// then the query, headers and cookies the policy selects, and the
// credential of a request cached per credential.
//
// Every component is escaped so that none can contain the separators of
// the key ('|', '?', '#', '=', '&'): an object key holding "?v=1" or
// "|u:AK" never shares the key of another object read with that query or
// that credential (TCC-03, TCC-11), and the suffixes of the blocks
// ("#b<n>") and variants ("#e:<enc>") never collide with an object.
func keyOf(p *params, scope, bucket, object string, req *Request, credential string) string {
	var b strings.Builder
	b.Grow(len(scope) + len(bucket) + len(object) + 16)
	b.WriteString(keyVersion)
	b.WriteByte('|')
	b.WriteString(url.QueryEscape(scope))
	b.WriteByte('|')
	b.WriteString(url.QueryEscape(bucket))
	b.WriteByte('/')
	b.WriteString(escapeObject(object))
	if req == nil {
		return b.String()
	}
	if q := queryKey(p, req.RawQuery); q != "" {
		b.WriteByte('?')
		b.WriteString(q)
	}
	for _, h := range p.KeyHeaders {
		v := strings.Join(req.Header.Values(h), ",")
		if p.Normalize {
			v = strings.ToLower(strings.Join(strings.Fields(v), " "))
		}
		b.WriteString("|h:")
		b.WriteString(url.QueryEscape(strings.ToLower(h)))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
	}
	if len(p.KeyCookies) > 0 {
		cookies := cookiesOf(req.Header)
		for _, name := range p.KeyCookies {
			v := ""
			for _, c := range cookies {
				if c.Name == name {
					v = c.Value
					break
				}
			}
			b.WriteString("|c:")
			b.WriteString(url.QueryEscape(name))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	if credential != "" {
		b.WriteString("|u:")
		b.WriteString(url.QueryEscape(credential))
	}
	return b.String()
}

// escapeObject escapes an object key for a cache key: every byte but the
// unreserved ones and the slashes (kept readable) is percent-encoded.
func escapeObject(object string) string {
	return strings.ReplaceAll(url.PathEscape(object), "%2F", "/")
}

// scopeOf is the scope part of a key: the host, or the distribution.
func scopeOf(p *params, dist, host string) string {
	if p.IncludeHost {
		return "h:" + host
	}
	return "d:" + dist
}

// bypassReason tells why a request skips the cache ("" when it does
// not).
func bypassReason(p *params, req *Request) string {
	switch {
	case req.Method != http.MethodGet && req.Method != http.MethodHead:
		return "method " + req.Method
	case !p.Enabled:
		return "cache disabled"
	case req.Credentials && p.BypassAuth:
		return "credentials"
	}
	if len(p.BypassQuery) > 0 && req.RawQuery != "" {
		for _, q := range parseQuery(req.RawQuery) {
			if slices.Contains(p.BypassQuery, strings.ToLower(q.name)) {
				return "query " + q.name
			}
		}
	}
	if len(p.BypassCookies) > 0 {
		for _, c := range cookiesOf(req.Header) {
			if slices.Contains(p.BypassCookies, "*") || slices.Contains(p.BypassCookies, c.Name) {
				return "cookie " + c.Name
			}
		}
	}
	for _, h := range p.BypassHeaders {
		if req.Header.Get(h) != "" {
			return "header " + h
		}
	}
	return ""
}

// credentialOf is the identity of the credentials of a request (the
// access key of its signature), for the policies that cache such requests
// per credential.
func credentialOf(req *Request) string {
	if req.Credential != "" {
		return req.Credential
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		if _, rest, ok := strings.Cut(auth, "Credential="); ok {
			ak, _, _ := strings.Cut(rest, "/")
			return ak
		}
		return fingerprint(auth)
	}
	for _, q := range parseQuery(req.RawQuery) {
		if strings.EqualFold(q.name, "X-Amz-Credential") {
			ak, _, _ := strings.Cut(q.value, "/")
			return ak
		}
	}
	return "anonymous"
}
