package domain

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits of a bucket CORS configuration (S3 PutBucketCors).
const (
	// MaxCORSRules is the maximum number of rules of a configuration.
	MaxCORSRules = 100
	// MaxCORSRuleIDLength is the maximum length of a rule ID.
	MaxCORSRuleIDLength = 255
)

// CORSMethods returns the HTTP methods a CORS rule may allow.
func CORSMethods() []string { return []string{"GET", "PUT", "POST", "DELETE", "HEAD"} }

// CORSRule is one rule of a bucket CORS configuration: the cross-origin
// requests it allows (origins, methods, request headers) and what the
// browser is told (exposed headers, preflight cache duration).
type CORSRule struct {
	ID string
	// AllowedOrigins are origins ("https://app.example.com"), each with at
	// most one "*" wildcard ("*" alone allows every origin).
	AllowedOrigins []string
	// AllowedMethods are among CORSMethods.
	AllowedMethods []string
	// AllowedHeaders are the request headers a preflight may announce
	// (Access-Control-Request-Headers), each with at most one "*",
	// compared case-insensitively.
	AllowedHeaders []string
	// ExposeHeaders are the response headers the browser may read.
	ExposeHeaders []string
	// MaxAgeSeconds is how long the browser may cache the answer of a
	// preflight; nil leaves the browser default.
	MaxAgeSeconds *int
}

// AllowsAnyOrigin reports whether the rule allows every origin ("*"): the
// answers then carry "Access-Control-Allow-Origin: *" and no credentials.
func (r CORSRule) AllowsAnyOrigin() bool {
	for _, o := range r.AllowedOrigins {
		if o == "*" {
			return true
		}
	}
	return false
}

// MatchesOrigin reports whether the rule allows origin.
func (r CORSRule) MatchesOrigin(origin string) bool {
	for _, o := range r.AllowedOrigins {
		if wildcardMatch(o, origin, false) {
			return true
		}
	}
	return false
}

// MatchesMethod reports whether the rule allows method.
func (r CORSRule) MatchesMethod(method string) bool {
	for _, m := range r.AllowedMethods {
		if m == method {
			return true
		}
	}
	return false
}

// MatchesHeaders reports whether the rule allows every request header of
// headers (case-insensitive, empty names ignored).
func (r CORSRule) MatchesHeaders(headers []string) bool {
	for _, h := range headers {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		allowed := false
		for _, pattern := range r.AllowedHeaders {
			if wildcardMatch(pattern, h, true) {
				allowed = true
				break
			}
		}
		if !allowed {
			return false
		}
	}
	return true
}

// CORSConfiguration is the CORS configuration of a bucket.
type CORSConfiguration struct {
	Rules []CORSRule
}

// Validate applies the S3 rules of a CORS configuration: 1 to
// MaxCORSRules rules, each with at least one origin and one method, the
// methods among CORSMethods, at most one wildcard per origin and per
// header, IDs of at most MaxCORSRuleIDLength characters and non-negative
// MaxAgeSeconds. Errors are *S3Error (MalformedXML, InvalidRequest).
func (c CORSConfiguration) Validate() error {
	if len(c.Rules) == 0 {
		return NewS3Error(ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: a CORS configuration needs at least one CORSRule")
	}
	if len(c.Rules) > MaxCORSRules {
		return NewS3Error(ErrInvalidArgument, "InvalidRequest", "The CORS configuration must have at most "+strconv.Itoa(MaxCORSRules)+" rules")
	}
	for i, r := range c.Rules {
		if err := r.validate(i + 1); err != nil {
			return err
		}
	}
	return nil
}

func (r CORSRule) validate(n int) error {
	rule := "CORSRule " + strconv.Itoa(n)
	if utf8.RuneCountInString(r.ID) > MaxCORSRuleIDLength {
		return NewS3Error(ErrInvalidArgument, "InvalidArgument", rule+": the ID must be at most "+strconv.Itoa(MaxCORSRuleIDLength)+" characters")
	}
	if len(r.AllowedOrigins) == 0 || len(r.AllowedMethods) == 0 {
		return NewS3Error(ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: "+rule+" needs an AllowedOrigin and an AllowedMethod")
	}
	for _, m := range r.AllowedMethods {
		known := false
		for _, allowed := range CORSMethods() {
			if m == allowed {
				known = true
			}
		}
		if !known {
			return NewS3Error(ErrInvalidArgument, "InvalidRequest", "Found unsupported HTTP method in CORS config. Unsupported method is "+m)
		}
	}
	for _, o := range r.AllowedOrigins {
		if o == "" || strings.Count(o, "*") > 1 {
			return NewS3Error(ErrInvalidArgument, "InvalidRequest", "AllowedOrigin \""+o+"\" can not have more than one wildcard.")
		}
	}
	for _, h := range r.AllowedHeaders {
		if strings.Count(h, "*") > 1 {
			return NewS3Error(ErrInvalidArgument, "InvalidRequest", "AllowedHeader \""+h+"\" can not have more than one wildcard.")
		}
	}
	if r.MaxAgeSeconds != nil && *r.MaxAgeSeconds < 0 {
		return NewS3Error(ErrInvalidArgument, "InvalidArgument", rule+": MaxAgeSeconds must not be negative")
	}
	return nil
}

// Match returns the first rule allowing a cross-origin request from
// origin with method and the request headers (the preflight's
// Access-Control-Request-Headers; none for an actual request), as S3
// evaluates CORS rules: in order, the first match wins.
func (c CORSConfiguration) Match(origin, method string, headers []string) (CORSRule, bool) {
	for _, r := range c.Rules {
		if r.MatchesOrigin(origin) && r.MatchesMethod(method) && r.MatchesHeaders(headers) {
			return r, true
		}
	}
	return CORSRule{}, false
}

// wildcardMatch matches s against a pattern with at most one "*" (any
// sequence of characters).
func wildcardMatch(pattern, s string, foldCase bool) bool {
	if foldCase {
		pattern, s = strings.ToLower(pattern), strings.ToLower(s)
	}
	prefix, suffix, wild := strings.Cut(pattern, "*")
	if !wild {
		return pattern == s
	}
	return len(s) >= len(prefix)+len(suffix) && strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix)
}
