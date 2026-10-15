package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SigV4 constants.
const (
	algorithm                = "AWS4-HMAC-SHA256"
	chunkAlgorithm           = "AWS4-HMAC-SHA256-PAYLOAD"
	trailerAlgorithm         = "AWS4-HMAC-SHA256-TRAILER"
	requestSuffix            = "aws4_request"
	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingPayload         = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingPayloadTrailer  = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	emptySHA256              = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	amzDateFormat            = "20060102T150405Z"
	scopeDateFormat          = "20060102"
	maxPresignExpiry         = 7 * 24 * time.Hour
	signatureHexLength       = 64
)

// Header, query and form field names.
const (
	headerAuthorization   = "Authorization"
	headerAmzDate         = "X-Amz-Date"
	headerDate            = "Date"
	headerContentSHA256   = "X-Amz-Content-Sha256"
	headerSecurityToken   = "X-Amz-Security-Token" // #nosec G101 -- an S3 header name, not a credential
	headerDecodedLength   = "X-Amz-Decoded-Content-Length"
	headerTrailer         = "X-Amz-Trailer"
	headerContentEncoding = "Content-Encoding"
	queryAlgorithm        = "X-Amz-Algorithm"
	queryCredential       = "X-Amz-Credential" // #nosec G101 -- an S3 query parameter name, not a credential
	queryDate             = "X-Amz-Date"
	queryExpires          = "X-Amz-Expires"
	querySignedHeaders    = "X-Amz-SignedHeaders"
	querySignature        = "X-Amz-Signature"
	querySecurityToken    = "X-Amz-Security-Token" // #nosec G101 -- an S3 query parameter name, not a credential
	queryContentSHA256    = "X-Amz-Content-Sha256"
	queryV2AccessKey      = "AWSAccessKeyId"
	queryV2Signature      = "Signature"
	credentialSeparator   = "/"
	signedHeaderSeparator = ";"
	awsChunkedEncoding    = "aws-chunked"
	amzHeaderPrefix       = "x-amz-"
	lowerAmzDate          = "x-amz-date"
	lowerContentSHA256    = "x-amz-content-sha256"
	lowerSecurityToken    = "x-amz-security-token" // #nosec G101 -- an S3 header name, not a credential
)

// credentialScope is the "<access key>/<date>/<region>/<service>/aws4_request"
// credential of a signature.
type credentialScope struct {
	accessKey string
	date      string
	region    string
	service   string
}

// scope returns the scope part ("date/region/service/aws4_request").
func (c credentialScope) scope() string {
	return c.date + credentialSeparator + c.region + credentialSeparator + c.service + credentialSeparator + requestSuffix
}

func (c credentialScope) String() string {
	return c.accessKey + credentialSeparator + c.scope()
}

// parseCredential parses a credential string. Access keys may themselves
// contain slashes, so the scope is taken from the end. The error is a
// plain detail: callers wrap it in the S3 error of their mechanism.
func parseCredential(s string) (credentialScope, error) {
	parts := strings.Split(s, credentialSeparator)
	n := len(parts)
	if n < 5 || parts[n-1] != requestSuffix {
		return credentialScope{}, errors.New("expected <access key>/<date>/<region>/<service>/aws4_request")
	}
	c := credentialScope{
		accessKey: strings.Join(parts[:n-4], credentialSeparator),
		date:      parts[n-4],
		region:    parts[n-3],
		service:   parts[n-2],
	}
	if c.accessKey == "" {
		return credentialScope{}, errors.New("the access key is empty")
	}
	if _, err := time.Parse(scopeDateFormat, c.date); err != nil || len(c.date) != len(scopeDateFormat) {
		return credentialScope{}, fmt.Errorf("invalid credential date %q", c.date)
	}
	if c.region == "" {
		return credentialScope{}, errors.New("the region is empty")
	}
	if c.service != ServiceS3 && c.service != ServiceSTS {
		return credentialScope{}, fmt.Errorf("unsupported service %q", c.service)
	}
	return c, nil
}

// authorization is a parsed Authorization header.
type authorization struct {
	scope         credentialScope
	signedHeaders []string
	signature     string
}

// parseAuthorization parses
// "AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...".
// The fields may be separated by "," or ", " and appear in any order;
// each must appear exactly once.
func parseAuthorization(h string) (authorization, error) {
	rest, ok := strings.CutPrefix(h, algorithm)
	if !ok || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return authorization{}, errors.New("unsupported algorithm, expected AWS4-HMAC-SHA256")
	}
	var out authorization
	var seenCredential, seenHeaders, seenSignature bool
	for _, field := range strings.Split(rest, ",") {
		field = strings.TrimSpace(field)
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return authorization{}, fmt.Errorf("the field %q is not a key=value pair", field)
		}
		switch name {
		case "Credential":
			if seenCredential {
				return authorization{}, errors.New("duplicate Credential")
			}
			seenCredential = true
			scope, err := parseCredential(value)
			if err != nil {
				return authorization{}, fmt.Errorf("invalid Credential: %w", err)
			}
			out.scope = scope
		case "SignedHeaders":
			if seenHeaders {
				return authorization{}, errors.New("duplicate SignedHeaders")
			}
			seenHeaders = true
			signed, err := parseSignedHeaders(value)
			if err != nil {
				return authorization{}, err
			}
			out.signedHeaders = signed
		case "Signature":
			if seenSignature {
				return authorization{}, errors.New("duplicate Signature")
			}
			seenSignature = true
			if !isSignature(value) {
				return authorization{}, errors.New("the Signature must be 64 hex characters")
			}
			out.signature = value
		default:
			return authorization{}, fmt.Errorf("unknown field %q", name)
		}
	}
	if !seenCredential || !seenHeaders || !seenSignature {
		return authorization{}, errors.New("the Credential, SignedHeaders and Signature fields are required")
	}
	return out, nil
}

// parseSignedHeaders parses a ";"-separated list of header names. The
// names are lowercased, sorted and deduplicated, as in the canonical
// request; host is mandatory.
func parseSignedHeaders(s string) ([]string, error) {
	if s == "" {
		return nil, errors.New("SignedHeaders is empty")
	}
	parts := strings.Split(s, signedHeaderSeparator)
	names := make([]string, 0, len(parts))
	hasHost := false
	for _, p := range parts {
		name := strings.ToLower(strings.TrimSpace(p))
		if name == "" || !isToken(name) {
			return nil, fmt.Errorf("invalid signed header name %q", p)
		}
		if name == "host" {
			hasHost = true
		}
		names = append(names, name)
	}
	if !hasHost {
		return nil, errors.New("the host header must be signed")
	}
	sort.Strings(names)
	out := names[:1]
	for _, n := range names[1:] {
		if n != out[len(out)-1] {
			out = append(out, n)
		}
	}
	return out, nil
}

// isToken reports whether s is a valid HTTP header field name.
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}

// isSignature reports whether s is a 64-character hex string.
func isSignature(s string) bool {
	return len(s) == signatureHexLength && isHex(s)
}

func isHex(s string) bool {
	if len(s)%2 != 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// isUnreserved reports the RFC 3986 unreserved characters, which SigV4
// never encodes.
func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

const upperHex = "0123456789ABCDEF"

// uriEncode percent-encodes every byte but the unreserved characters (and
// "/" when keepSlash), with upper-case hex digits.
func uriEncode(s string, keepSlash bool) string {
	n := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; !isUnreserved(c) && (c != '/' || !keepSlash) {
			n++
		}
	}
	if n == 0 {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s) + 2*n)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) || (c == '/' && keepSlash) {
			sb.WriteByte(c)
			continue
		}
		sb.WriteByte('%')
		sb.WriteByte(upperHex[c>>4])
		sb.WriteByte(upperHex[c&15])
	}
	return sb.String()
}

// encodePath URI-encodes a decoded path the way S3 expects in canonical
// requests: every byte but the unreserved characters and "/" becomes %XX.
// S3 does not normalize the path nor encode it twice.
func encodePath(p string) string {
	if p == "" {
		return "/"
	}
	return uriEncode(p, true)
}

// canonicalURIs returns the canonical URI candidates of a request: the
// S3-style encoding of the decoded path and, when it differs, the path as
// escaped on the wire (clients that encode reserved characters such as
// "/" inside keys differently). Both describe the same decoded path, so
// accepting either does not widen what a signature authorizes.
func canonicalURIs(u *url.URL) []string {
	primary := encodePath(u.Path)
	if u.Opaque != "" {
		return []string{primary}
	}
	// For server requests EscapedPath is the path as sent on the wire.
	if raw := u.EscapedPath(); raw != "" && raw != primary {
		return []string{primary, raw}
	}
	return []string{primary}
}

// parseQuery decodes a raw query string. Unlike url.ParseQuery it accepts
// ";" inside values and fails on invalid percent-encoding instead of
// silently dropping the pair, so that the canonical query covers exactly
// what the client sent. "+" decodes to a space, as in AWS SDKs.
func parseQuery(raw string) (url.Values, error) {
	q := url.Values{}
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(k)
		if err != nil {
			return nil, fmt.Errorf("invalid query parameter name %q: %w", k, err)
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("invalid value of query parameter %q: %w", key, err)
		}
		q[key] = append(q[key], value)
	}
	return q, nil
}

// canonicalQuery renders the sorted, encoded query string without the
// excluded parameter (X-Amz-Signature for presigned requests; "" excludes
// nothing).
func canonicalQuery(q url.Values, exclude string) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		if exclude != "" && k == exclude {
			continue
		}
		ek := uriEncode(k, false)
		if len(vs) == 0 {
			pairs = append(pairs, kv{ek, ""})
		}
		for _, v := range vs {
			pairs = append(pairs, kv{ek, uriEncode(v, false)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(p.k)
		sb.WriteByte('=')
		sb.WriteString(p.v)
	}
	return sb.String()
}

// headerValues returns the values of a signed header from the request. The
// host, content-length and transfer-encoding headers come from the request
// fields, where net/http moves them.
func headerValues(r *http.Request, name string) ([]string, bool) {
	switch name {
	case "host":
		host := r.Host
		if host == "" && r.URL != nil {
			host = r.URL.Host
		}
		return []string{host}, host != ""
	case "content-length":
		if vs := r.Header.Values("Content-Length"); len(vs) > 0 {
			return vs, true
		}
		if r.ContentLength >= 0 {
			return []string{strconv.FormatInt(r.ContentLength, 10)}, true
		}
		return nil, false
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return []string{strings.Join(r.TransferEncoding, ",")}, true
		}
	case "expect":
		// Proxies may strip "Expect: 100-continue"; clients sign it with
		// that value.
		if vs := r.Header.Values("Expect"); len(vs) > 0 {
			return vs, true
		}
		return []string{"100-continue"}, true
	}
	vs := r.Header.Values(name)
	return vs, len(vs) > 0
}

// trimHeaderValue applies the SigV4 "trimall": leading and trailing
// whitespace removed, runs of spaces collapsed into one.
func trimHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "  ") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v))
	space := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' {
			if space {
				continue
			}
			space = true
		} else {
			space = false
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// canonicalHeaders renders the "name:value\n" lines of the signed headers
// (already lowercase and sorted). It returns the name of the first signed
// header missing from the request.
func canonicalHeaders(r *http.Request, signed []string) (string, string) {
	var sb strings.Builder
	for _, name := range signed {
		vs, ok := headerValues(r, name)
		if !ok {
			return "", name
		}
		sb.WriteString(name)
		sb.WriteByte(':')
		for i, v := range vs {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(trimHeaderValue(v))
		}
		sb.WriteByte('\n')
	}
	return sb.String(), ""
}

// canonicalRequest assembles the canonical request string.
func canonicalRequest(method, uri, query, headers string, signed []string, payloadHash string) string {
	var sb strings.Builder
	sb.Grow(len(method) + len(uri) + len(query) + len(headers) + 64*2)
	sb.WriteString(method)
	sb.WriteByte('\n')
	sb.WriteString(uri)
	sb.WriteByte('\n')
	sb.WriteString(query)
	sb.WriteByte('\n')
	sb.WriteString(headers)
	sb.WriteByte('\n')
	sb.WriteString(strings.Join(signed, signedHeaderSeparator))
	sb.WriteByte('\n')
	sb.WriteString(payloadHash)
	return sb.String()
}

// stringToSign assembles the string to sign of a request.
func stringToSign(amzDate string, scope credentialScope, canonical string) string {
	return algorithm + "\n" + amzDate + "\n" + scope.scope() + "\n" + sha256Hex([]byte(canonical))
}

// deriveSigningKey derives the per-day signing key.
func deriveSigningKey(secret string, scope credentialScope) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(scope.date))
	k = hmacSHA256(k, []byte(scope.region))
	k = hmacSHA256(k, []byte(scope.service))
	return hmacSHA256(k, []byte(requestSuffix))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func signatureHex(key []byte, s string) string {
	return hex.EncodeToString(hmacSHA256(key, []byte(s)))
}

// equalSignatures compares two hex signatures in constant time.
func equalSignatures(expected, provided string) bool {
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(provided)))
}

// parseAmzDate parses an ISO 8601 basic timestamp (20130524T000000Z).
func parseAmzDate(s string) (time.Time, error) {
	if len(s) != len(amzDateFormat) {
		return time.Time{}, fmt.Errorf("invalid ISO 8601 basic timestamp %q", s)
	}
	t, err := time.Parse(amzDateFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid ISO 8601 basic timestamp %q", s)
	}
	return t, nil
}
