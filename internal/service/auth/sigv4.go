package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// verifier authenticates AWS Signature Version 4 requests.
type verifier struct {
	deps Deps
	opts options
}

var _ Verifier = (*verifier)(nil)

// NewVerifier returns the SigV4 verifier. Creds is required.
func NewVerifier(deps Deps, opts ...Option) (Verifier, error) {
	if deps.Creds == nil {
		return nil, fmt.Errorf("%w: auth: Creds dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &verifier{deps: deps, opts: o}, nil
}

// IsPresigned reports whether the request carries a presigned signature.
func IsPresigned(q url.Values) bool {
	return q.Has(querySignature) || q.Has(queryAlgorithm)
}

// IsSigned reports whether the request carries a SigV4 Authorization header.
func IsSigned(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get(headerAuthorization), algorithm)
}

// Verify implements Verifier.
func (v *verifier) Verify(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	if IsPresigned(r.URL.Query()) {
		return v.VerifyPresigned(ctx, r)
	}
	auth := r.Header.Get(headerAuthorization)
	if auth == "" {
		return domain.Anonymous(), nil
	}
	if !strings.HasPrefix(auth, algorithm) {
		return nil, domain.NewS3Error(domain.ErrSignatureMismatch, "InvalidRequest", "Only AWS Signature Version 4 (AWS4-HMAC-SHA256) is supported.")
	}
	hdr, err := parseAuthorization(auth)
	if err != nil {
		return nil, err
	}
	reqTime, err := requestTime(r)
	if err != nil {
		return nil, err
	}
	if err := v.checkScope(hdr.scope, reqTime); err != nil {
		return nil, err
	}
	creds, identity, err := v.lookup(ctx, hdr.scope.accessKey, r.Header.Get(headerSecurityToken))
	if err != nil {
		return nil, err
	}
	payloadHash := r.Header.Get(headerContentSHA256)
	if payloadHash == "" {
		payloadHash = emptySHA256
	}
	headers, err := canonicalHeaders(r, hdr.signedHeaders)
	if err != nil {
		return nil, err
	}
	canonical := canonicalRequest(r.Method, encodePath(r.URL.Path), canonicalQuery(r.URL.Query(), ""), headers, hdr.signedHeaders, payloadHash)
	key := signingKey(creds.SecretKey, hdr.scope)
	expected := signatureHex(key, stringToSign(reqTime.Format(amzDateFormat), hdr.scope, canonical))
	if !equalSignatures(expected, hdr.signature) {
		v.deps.Logger.DebugContext(ctx, "auth: signature mismatch", "access_key", hdr.scope.accessKey, "method", r.Method, "path", r.URL.Path)
		return nil, fmt.Errorf("%w: header signature", domain.ErrSignatureMismatch)
	}
	if err := v.wrapBody(r, payloadHash, key, hdr.scope, reqTime, hdr.signature); err != nil {
		return nil, err
	}
	return &identity, nil
}

// VerifyPresigned implements Verifier.
func (v *verifier) VerifyPresigned(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	q := r.URL.Query()
	if q.Get(queryAlgorithm) != algorithm {
		return nil, queryError("X-Amz-Algorithm only supports \"AWS4-HMAC-SHA256\"")
	}
	scope, err := parseCredential(q.Get(queryCredential))
	if err != nil {
		return nil, queryError("Error parsing the X-Amz-Credential parameter; " + err.Error())
	}
	reqTime, err := parseAmzDate(q.Get(queryDate))
	if err != nil {
		return nil, queryError("X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\"")
	}
	expires, err := strconv.ParseInt(q.Get(queryExpires), 10, 64)
	if err != nil || expires < 1 || time.Duration(expires)*time.Second > maxPresignExpiry {
		return nil, queryError("X-Amz-Expires must be an integer between 1 and 604800 seconds")
	}
	signedHeaders := strings.Split(q.Get(querySignedHeaders), signedHeaderSepartor)
	if q.Get(querySignedHeaders) == "" || !contains(signedHeaders, "host") {
		return nil, queryError("X-Amz-SignedHeaders must include host")
	}
	signature := q.Get(querySignature)
	if len(signature) != 64 || !isHex(signature) {
		return nil, queryError("X-Amz-Signature must be 64 hex characters")
	}
	if err := v.checkCredential(scope, reqTime); err != nil {
		return nil, err
	}
	// A presigned URL is valid from its date (minus the tolerated skew) for
	// X-Amz-Expires seconds, regardless of when the request is made.
	now := v.deps.Clock.Now()
	if now.Before(reqTime.Add(-v.opts.maxSkew)) {
		return nil, domain.NewS3Error(domain.ErrRequestExpired, "AccessDenied", "Request is not yet valid")
	}
	if now.After(reqTime.Add(time.Duration(expires) * time.Second)) {
		return nil, domain.NewS3Error(domain.ErrRequestExpired, "AccessDenied", "Request has expired")
	}
	token := q.Get(querySecurityToken)
	if token == "" {
		token = r.Header.Get(headerSecurityToken)
	}
	creds, identity, err := v.lookup(ctx, scope.accessKey, token)
	if err != nil {
		return nil, err
	}
	payloadHash := q.Get(queryContentSHA256)
	if payloadHash == "" {
		payloadHash = unsignedPayload
	}
	headers, err := canonicalHeaders(r, signedHeaders)
	if err != nil {
		return nil, err
	}
	canonical := canonicalRequest(r.Method, encodePath(r.URL.Path), canonicalQuery(q, querySignature), headers, signedHeaders, payloadHash)
	key := signingKey(creds.SecretKey, scope)
	expected := signatureHex(key, stringToSign(reqTime.Format(amzDateFormat), scope, canonical))
	if !equalSignatures(expected, signature) {
		v.deps.Logger.DebugContext(ctx, "auth: presigned signature mismatch", "access_key", scope.accessKey, "method", r.Method, "path", r.URL.Path)
		return nil, fmt.Errorf("%w: presigned signature", domain.ErrSignatureMismatch)
	}
	if payloadHash != unsignedPayload {
		if err := v.wrapBody(r, payloadHash, key, scope, reqTime, signature); err != nil {
			return nil, err
		}
	}
	return &identity, nil
}

func queryError(detail string) error {
	return domain.NewS3Error(domain.ErrSignatureMismatch, "AuthorizationQueryParametersError", detail)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// requestTime reads the request time from X-Amz-Date or Date.
func requestTime(r *http.Request) (time.Time, error) {
	if s := r.Header.Get(headerAmzDate); s != "" {
		t, err := parseAmzDate(s)
		if err != nil {
			return time.Time{}, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "AWS authentication requires a valid Date or x-amz-date header")
		}
		return t, nil
	}
	if s := r.Header.Get(headerDate); s != "" {
		t, err := http.ParseTime(s)
		if err != nil {
			return time.Time{}, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "AWS authentication requires a valid Date or x-amz-date header")
		}
		return t.UTC(), nil
	}
	return time.Time{}, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "AWS authentication requires a valid Date or x-amz-date header")
}

// checkScope validates the credential scope against the request time, the
// configured region and, for header-signed requests, the clock skew.
func (v *verifier) checkScope(scope credentialScope, reqTime time.Time) error {
	if err := v.checkCredential(scope, reqTime); err != nil {
		return err
	}
	now := v.deps.Clock.Now()
	skew := now.Sub(reqTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > v.opts.maxSkew {
		return fmt.Errorf("%w: request time %s, server time %s", domain.ErrRequestExpired, reqTime.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

// checkCredential validates the scope date and region.
func (v *verifier) checkCredential(scope credentialScope, reqTime time.Time) error {
	if scope.date != reqTime.UTC().Format(scopeDateFormat) {
		return malformed("Credential", fmt.Sprintf("the date %s does not match the request date", scope.date))
	}
	if v.opts.region != "" && scope.region != v.opts.region {
		return malformed("Credential", fmt.Sprintf("the region '%s' is wrong; expecting '%s'", scope.region, v.opts.region))
	}
	return nil
}

// lookup resolves and validates the credentials of an access key.
func (v *verifier) lookup(ctx context.Context, accessKey, token string) (domain.Credentials, domain.Identity, error) {
	creds, identity, err := v.deps.Creds.LookupCredentials(ctx, accessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) || errors.Is(err, domain.ErrUserNotFound) {
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %s", domain.ErrInvalidAccessKey, accessKey)
		}
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("auth: lookup %s: %w", accessKey, err)
	}
	if !creds.IsEnabled() {
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: access key %s is disabled", domain.ErrAccessDenied, accessKey)
	}
	if creds.IsExpired(v.deps.Clock.Now()) {
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: access key %s has expired", domain.ErrAccessDenied, accessKey)
	}
	if creds.SessionToken != "" && token != creds.SessionToken {
		return domain.Credentials{}, domain.Identity{}, domain.NewS3Error(domain.ErrAccessDenied, "InvalidToken", "The provided token is malformed or otherwise invalid.")
	}
	identity.AccessKey = accessKey
	identity.IsAnonymous = false
	return creds, identity, nil
}

// wrapBody installs the payload verification matching the declared hash:
// aws-chunked decoding (with or without chunk signatures) or a SHA-256
// check of the whole body.
func (v *verifier) wrapBody(r *http.Request, payloadHash string, key []byte, scope credentialScope, reqTime time.Time, seed string) error {
	switch payloadHash {
	case unsignedPayload:
		return nil
	case streamingPayload, streamingPayloadTrailer, streamingUnsignedTrailer:
		decoded, err := strconv.ParseInt(r.Header.Get(headerDecodedLength), 10, 64)
		if err != nil || decoded < 0 {
			return domain.NewS3Error(domain.ErrInvalidArgument, "MissingContentLength", "You must provide the x-amz-decoded-content-length header for aws-chunked payloads.")
		}
		if r.Body == nil || r.Body == http.NoBody {
			return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody", "An aws-chunked payload was declared but no body was sent.")
		}
		r.Body = newChunkedReader(r.Body, chunkedConfig{
			signed:  payloadHash != streamingUnsignedTrailer,
			trailer: payloadHash != streamingPayload,
			key:     key,
			scope:   scope,
			amzDate: reqTime.Format(amzDateFormat),
			seed:    seed,
		})
		r.ContentLength = decoded
		return nil
	default:
		if len(payloadHash) != 64 || !isHex(payloadHash) {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "x-amz-content-sha256 must be UNSIGNED-PAYLOAD, a streaming marker or the hex SHA-256 of the payload")
		}
		if r.Body == nil || r.Body == http.NoBody {
			if !strings.EqualFold(payloadHash, emptySHA256) {
				return sha256Mismatch()
			}
			return nil
		}
		r.Body = newSHA256Verifier(r.Body, payloadHash)
		return nil
	}
}

func sha256Mismatch() error {
	return domain.NewS3Error(domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
}
