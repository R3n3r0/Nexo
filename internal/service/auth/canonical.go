package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
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
)

// Header and query names.
const (
	headerAuthorization  = "Authorization"
	headerAmzDate        = "X-Amz-Date"
	headerDate           = "Date"
	headerContentSHA256  = "X-Amz-Content-Sha256"
	headerSecurityToken  = "X-Amz-Security-Token"
	headerDecodedLength  = "X-Amz-Decoded-Content-Length"
	queryAlgorithm       = "X-Amz-Algorithm"
	queryCredential      = "X-Amz-Credential"
	queryDate            = "X-Amz-Date"
	queryExpires         = "X-Amz-Expires"
	querySignedHeaders   = "X-Amz-SignedHeaders"
	querySignature       = "X-Amz-Signature"
	querySecurityToken   = "X-Amz-Security-Token"
	queryContentSHA256   = "X-Amz-Content-Sha256"
	serviceS3            = "s3"
	serviceSTS           = "sts"
	credentialSeparator  = "/"
	signedHeaderSepartor = ";"
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
	return strings.Join([]string{c.date, c.region, c.service, requestSuffix}, credentialSeparator)
}

func (c credentialScope) String() string {
	return c.accessKey + credentialSeparator + c.scope()
}

// parseCredential parses a credential string. Access keys may themselves
// contain slashes (STS style), so the scope is taken from the end.
func parseCredential(s string) (credentialScope, error) {
	parts := strings.Split(s, credentialSeparator)
	if len(parts) < 5 || parts[len(parts)-1] != requestSuffix {
		return credentialScope{}, malformed("Credential", "expected <access key>/<date>/<region>/<service>/aws4_request")
	}
	n := len(parts)
	c := credentialScope{
		accessKey: strings.Join(parts[:n-4], credentialSeparator),
		date:      parts[n-4],
		region:    parts[n-3],
		service:   parts[n-2],
	}
	if c.accessKey == "" {
		return credentialScope{}, malformed("Credential", "empty access key")
	}
	if _, err := time.Parse(scopeDateFormat, c.date); err != nil {
		return credentialScope{}, malformed("Credential", "invalid date "+c.date)
	}
	if c.service != serviceS3 && c.service != serviceSTS {
		return credentialScope{}, malformed("Credential", "unsupported service "+c.service)
	}
	return c, nil
}

// malformed builds the AuthorizationHeaderMalformed error.
func malformed(what, detail string) error {
	return domain.NewS3Error(domain.ErrSignatureMismatch, "AuthorizationHeaderMalformed", fmt.Sprintf("The authorization header is malformed; %s: %s.", what, detail))
}

// authorization is a parsed Authorization header.
type authorization struct {
	scope         credentialScope
	signedHeaders []string
	signature     string
}

// parseAuthorization parses "AWS4-HMAC-SHA256 Credential=..., SignedHeaders=..., Signature=...".
func parseAuthorization(h string) (authorization, error) {
	if !strings.HasPrefix(h, algorithm+" ") {
		return authorization{}, malformed("algorithm", "unsupported algorithm")
	}
	var out authorization
	seen := map[string]bool{}
	for _, field := range strings.Split(strings.TrimPrefix(h, algorithm+" "), ",") {
		field = strings.TrimSpace(field)
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return authorization{}, malformed("field", field)
		}
		if seen[name] {
			return authorization{}, malformed(name, "duplicate field")
		}
		seen[name] = true
		switch name {
		case "Credential":
			scope, err := parseCredential(value)
			if err != nil {
				return authorization{}, err
			}
			out.scope = scope
		case "SignedHeaders":
			if value == "" {
				return authorization{}, malformed("SignedHeaders", "empty")
			}
			out.signedHeaders = strings.Split(value, signedHeaderSepartor)
		case "Signature":
			if len(value) != 64 || !isHex(value) {
				return authorization{}, malformed("Signature", "expected 64 hex characters")
			}
			out.signature = value
		default:
			return authorization{}, malformed(name, "unknown field")
		}
	}
	if !seen["Credential"] || !seen["SignedHeaders"] || !seen["Signature"] {
		return authorization{}, malformed("header", "missing Credential, SignedHeaders or Signature")
	}
	hasHost := false
	for _, name := range out.signedHeaders {
		if name == "host" {
			hasHost = true
		}
	}
	if !hasHost {
		return authorization{}, malformed("SignedHeaders", "host must be signed")
	}
	return out, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && len(s)%2 == 0
}

// encodePath URI-encodes a path the way S3 expects in canonical requests:
// every byte except unreserved characters and "/" becomes %XX (upper case).
func encodePath(p string) string {
	if p == "" {
		return "/"
	}
	var sb strings.Builder
	sb.Grow(len(p) + 16)
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '/' || isUnreserved(c) {
			sb.WriteByte(c)
			continue
		}
		fmt.Fprintf(&sb, "%%%02X", c)
	}
	return sb.String()
}

// encodeQueryComponent encodes a query key or value (strict, "/" included).
func encodeQueryComponent(s string) string {
	var sb strings.Builder
	sb.Grow(len(s) + 16)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			sb.WriteByte(c)
			continue
		}
		fmt.Fprintf(&sb, "%%%02X", c)
	}
	return sb.String()
}

func isUnreserved(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~'
}

// canonicalQuery renders the sorted, encoded query string without the
// excluded parameter (X-Amz-Signature for presigned requests).
func canonicalQuery(q url.Values, exclude string) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		if k == exclude {
			continue
		}
		ek := encodeQueryComponent(k)
		if len(vs) == 0 {
			pairs = append(pairs, kv{ek, ""})
		}
		for _, v := range vs {
			pairs = append(pairs, kv{ek, encodeQueryComponent(v)})
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
// host and content-length pseudo headers come from the request fields.
func headerValues(r *http.Request, name string) ([]string, bool) {
	switch name {
	case "host":
		return []string{r.Host}, r.Host != ""
	case "content-length":
		if r.ContentLength >= 0 {
			if vs := r.Header.Values("Content-Length"); len(vs) > 0 {
				return vs, true
			}
			return []string{strconv.FormatInt(r.ContentLength, 10)}, true
		}
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return []string{strings.Join(r.TransferEncoding, ",")}, true
		}
	case "expect":
		if vs := r.Header.Values("Expect"); len(vs) > 0 {
			return vs, true
		}
		return []string{"100-continue"}, true
	}
	vs := r.Header.Values(http.CanonicalHeaderKey(name))
	return vs, len(vs) > 0
}

// canonicalHeaders renders the "name:value\n" lines of the signed headers.
// A signed header missing from the request is an error.
func canonicalHeaders(r *http.Request, signed []string) (string, error) {
	var sb strings.Builder
	for _, name := range signed {
		name = strings.ToLower(name)
		vs, ok := headerValues(r, name)
		if !ok {
			return "", domain.NewS3Error(domain.ErrSignatureMismatch, "SignatureDoesNotMatch",
				"The request signature we calculated does not match the signature you provided: signed header "+name+" is missing.")
		}
		trimmed := make([]string, 0, len(vs))
		for _, v := range vs {
			trimmed = append(trimmed, strings.Join(strings.Fields(v), " "))
		}
		sb.WriteString(name)
		sb.WriteByte(':')
		sb.WriteString(strings.Join(trimmed, ","))
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// canonicalRequest assembles the canonical request string.
func canonicalRequest(method, path, query, headers string, signed []string, payloadHash string) string {
	names := make([]string, len(signed))
	for i, n := range signed {
		names[i] = strings.ToLower(n)
	}
	return strings.Join([]string{method, path, query, headers, strings.Join(names, signedHeaderSepartor), payloadHash}, "\n")
}

// stringToSign assembles the string to sign of a request.
func stringToSign(amzDate string, scope credentialScope, canonical string) string {
	return strings.Join([]string{algorithm, amzDate, scope.scope(), sha256Hex([]byte(canonical))}, "\n")
}

// signingKey derives the per-day signing key.
func signingKey(secret string, scope credentialScope) []byte {
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
func equalSignatures(a, b string) bool {
	return hmac.Equal([]byte(strings.ToLower(a)), []byte(strings.ToLower(b)))
}

// parseAmzDate parses an ISO 8601 basic timestamp (20130524T000000Z).
func parseAmzDate(s string) (time.Time, error) {
	t, err := time.Parse(amzDateFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid X-Amz-Date %q", s)
	}
	return t, nil
}
