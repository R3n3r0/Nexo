package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
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
	keys *keyCache
}

var _ Verifier = (*verifier)(nil)

// NewVerifier returns the SigV4 verifier. Creds is required: a missing
// lookup is a wiring error (domain.ErrInvalidArgument).
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
	o := applyOptions(opts)
	return &verifier{deps: deps, opts: o, keys: newKeyCache(o.keyCacheSize)}, nil
}

// IsPresigned reports whether the query carries SigV4 presigned
// parameters.
func IsPresigned(q url.Values) bool {
	return q.Has(querySignature) || q.Has(queryAlgorithm) || q.Has(queryCredential)
}

// IsSigned reports whether the request carries a SigV4 Authorization header.
func IsSigned(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get(headerAuthorization), algorithm)
}

// isSigV2Query reports a Signature Version 2 presigned URL.
func isSigV2Query(q url.Values) bool {
	return q.Has(queryV2AccessKey) && q.Has(queryV2Signature)
}

// detectionQuery parses the query to pick the authentication mechanism.
// A malformed query falls back to the lenient standard parser: the signed
// paths parse it again strictly and reject it.
func detectionQuery(raw string) url.Values {
	if q, err := parseQuery(raw); err == nil {
		return q
	}
	q, _ := url.ParseQuery(raw)
	return q
}

// anonymous returns the identity of an unsigned request.
func anonymous() *domain.Identity {
	id := domain.Anonymous()
	id.AuthType = domain.AuthTypeAnonymous
	return id
}

// Verify implements Verifier.
func (v *verifier) Verify(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("%w: auth: nil request", domain.ErrInvalidArgument)
	}
	q := detectionQuery(r.URL.RawQuery)
	authz := r.Header.Get(headerAuthorization)
	switch {
	case IsPresigned(q):
		return v.VerifyPresigned(ctx, r)
	case authz == "" && isSigV2Query(q):
		return nil, errUnsupported()
	case authz == "":
		return v.verifyAnonymous(r)
	case strings.HasPrefix(authz, algorithm):
		return v.verifyHeader(ctx, r, authz)
	default:
		// "AWS <key>:<signature>" (Signature Version 2) or any other scheme.
		return nil, errUnsupported()
	}
}

// verifyAnonymous admits an unsigned request. Its body is decoded when it
// is an unsigned aws-chunked payload (STREAMING-UNSIGNED-PAYLOAD-TRAILER,
// which the SDKs send to public buckets with a trailing checksum): the
// trailing checksum is verified and the decoded length replaces
// Content-Length, as for signed requests. Any other payload is left
// untouched.
func (v *verifier) verifyAnonymous(r *http.Request) (*domain.Identity, error) {
	if r.Header.Get(headerContentSHA256) == streamingUnsignedTrailer {
		if err := v.wrapChunked(r, streamingUnsignedTrailer, nil, credentialScope{}, "", ""); err != nil {
			return nil, err
		}
	}
	return anonymous(), nil
}

// verifyHeader authenticates an Authorization header signature.
func (v *verifier) verifyHeader(ctx context.Context, r *http.Request, authz string) (*domain.Identity, error) {
	hdr, err := parseAuthorization(authz)
	if err != nil {
		return nil, errMalformedHeader(err.Error())
	}
	// X-Amz-Date and X-Amz-Content-Sha256 are bound to the signature even
	// when not listed: the first is part of the string to sign, the second
	// is the payload hash line of the canonical request.
	if err := checkSignedAmzHeaders(r.Header, hdr.signedHeaders, lowerAmzDate, lowerContentSHA256); err != nil {
		return nil, err
	}
	reqTime, err := requestTime(r)
	if err != nil {
		return nil, err
	}
	if err := v.checkScope(ctx, hdr.scope, reqTime); err != nil {
		return nil, errMalformedHeader(err.Error())
	}
	now := v.deps.Clock.Now()
	if skew := now.Sub(reqTime); skew > v.opts.maxSkew || skew < -v.opts.maxSkew {
		v.deps.Logger.DebugContext(ctx, "auth: request time too skewed",
			"access_key", hdr.scope.accessKey, "request_time", reqTime, "server_time", now)
		return nil, errSkewed()
	}
	query, err := parseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidArgument, "The query string is not valid URL encoding: "+err.Error())
	}
	creds, identity, err := v.lookup(ctx, hdr.scope.accessKey, r.Header.Get(headerSecurityToken))
	if err != nil {
		return nil, err
	}
	payloadHash, hashed, err := v.payloadHash(r)
	if err != nil {
		return nil, err
	}
	key := v.keys.get(creds.SecretKey, hdr.scope)
	amzDate := reqTime.UTC().Format(amzDateFormat)
	if err := v.checkSignature(ctx, r, query, "", hdr.signedHeaders, payloadHash, key, hdr.scope, amzDate, hdr.signature); err != nil {
		return nil, err
	}
	if !hashed {
		if err := v.wrapBody(r, payloadHash, key, hdr.scope, amzDate, hdr.signature); err != nil {
			return nil, err
		}
	}
	identity.AuthType = domain.AuthTypeHeader
	identity.SignatureAge = signatureAge(now, reqTime)
	identity.SignedHeaders = hdr.signedHeaders
	return &identity, nil
}

// VerifyPresigned implements Verifier.
func (v *verifier) VerifyPresigned(ctx context.Context, r *http.Request) (*domain.Identity, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("%w: auth: nil request", domain.ErrInvalidArgument)
	}
	if r.Header.Get(headerAuthorization) != "" {
		return nil, errAmbiguous()
	}
	q, err := parseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errQuery("The query string is not valid URL encoding: " + err.Error())
	}
	if q.Get(queryAlgorithm) != algorithm {
		return nil, errQuery("X-Amz-Algorithm only supports \"AWS4-HMAC-SHA256\".")
	}
	scope, err := parseCredential(q.Get(queryCredential))
	if err != nil {
		return nil, errQuery("Error parsing the X-Amz-Credential parameter; " + err.Error() + ".")
	}
	reqTime, err := parseAmzDate(q.Get(queryDate))
	if err != nil {
		return nil, errQuery("X-Amz-Date must be in the ISO8601 Long Format \"yyyyMMdd'T'HHmmss'Z'\".")
	}
	expires, err := strconv.ParseInt(q.Get(queryExpires), 10, 64)
	if err != nil || expires < 1 || expires > int64(maxPresignExpiry/time.Second) {
		return nil, errQuery("X-Amz-Expires must be an integer between 1 and 604800 seconds (one week).")
	}
	signedHeaders, err := parseSignedHeaders(q.Get(querySignedHeaders))
	if err != nil {
		return nil, errQuery("Error parsing the X-Amz-SignedHeaders parameter; " + err.Error() + ".")
	}
	signature := q.Get(querySignature)
	if !isSignature(signature) {
		return nil, errQuery("X-Amz-Signature must be 64 hex characters.")
	}
	// Whoever holds the URL must not be able to add x-amz-* headers that
	// change the operation (x-amz-copy-source turns an upload link into a
	// read of anything the signer can read). Exempt are the payload hash
	// header, which is the payload hash line of the canonical request when
	// used, and the token header when the token travels in the signed query
	// (the header is then ignored).
	exempt := []string{lowerContentSHA256}
	if q.Get(querySecurityToken) != "" {
		exempt = append(exempt, lowerSecurityToken)
	}
	if err := checkSignedAmzHeaders(r.Header, signedHeaders, exempt...); err != nil {
		return nil, err
	}
	if err := v.checkScope(ctx, scope, reqTime); err != nil {
		return nil, errQuery("Error parsing the X-Amz-Credential parameter; " + err.Error() + ".")
	}
	// A presigned URL is valid from its date (minus the tolerated skew) for
	// X-Amz-Expires seconds, regardless of when the request is made.
	now := v.deps.Clock.Now()
	if now.Before(reqTime.Add(-v.opts.maxSkew)) {
		return nil, errNotYetValid()
	}
	if now.After(reqTime.Add(time.Duration(expires) * time.Second)) {
		return nil, errExpired("Request has expired.")
	}
	// The payload is UNSIGNED-PAYLOAD unless the URL (or, as some SDKs
	// sign it, the X-Amz-Content-Sha256 header) declares a hash.
	payloadHash := unsignedPayload
	h := q.Get(queryContentSHA256)
	if h == "" {
		h = r.Header.Get(headerContentSHA256)
	}
	if h != "" {
		if h != unsignedPayload && !isSignature(h) {
			return nil, errQuery("X-Amz-Content-Sha256 must be UNSIGNED-PAYLOAD or the hex SHA-256 of the payload.")
		}
		payloadHash = strings.ToLower(h)
		if h == unsignedPayload {
			payloadHash = h
		}
	}
	token := q.Get(querySecurityToken)
	if token == "" {
		token = r.Header.Get(headerSecurityToken)
	}
	creds, identity, err := v.lookup(ctx, scope.accessKey, token)
	if err != nil {
		return nil, err
	}
	key := v.keys.get(creds.SecretKey, scope)
	amzDate := reqTime.Format(amzDateFormat)
	if err := v.checkSignature(ctx, r, q, querySignature, signedHeaders, payloadHash, key, scope, amzDate, signature); err != nil {
		return nil, err
	}
	if err := v.wrapBody(r, payloadHash, key, scope, amzDate, signature); err != nil {
		return nil, err
	}
	identity.AuthType = domain.AuthTypeQuery
	identity.SignatureAge = signatureAge(now, reqTime)
	identity.SignedHeaders = signedHeaders
	return &identity, nil
}

// checkSignature computes the expected signature of the request for every
// canonical URI candidate and compares it with the provided one.
func (v *verifier) checkSignature(ctx context.Context, r *http.Request, query url.Values, exclude string,
	signed []string, payloadHash string, key []byte, scope credentialScope, amzDate, provided string,
) error {
	headers, missing := canonicalHeaders(r, signed)
	if missing != "" {
		return errSignature("signed header " + missing + " is missing from the request")
	}
	canonicalQ := canonicalQuery(query, exclude)
	for _, uri := range canonicalURIs(r.URL) {
		canonical := canonicalRequest(r.Method, uri, canonicalQ, headers, signed, payloadHash)
		if equalSignatures(signatureHex(key, stringToSign(amzDate, scope, canonical)), provided) {
			return nil
		}
	}
	v.deps.Logger.DebugContext(ctx, "auth: signature mismatch",
		"access_key", scope.accessKey, "method", r.Method, "path", r.URL.Path, "signed_headers", strings.Join(signed, signedHeaderSeparator))
	return errSignature("")
}

// checkSignedAmzHeaders rejects, as AWS does (AccessDenied, "There were
// headers present in the request which were not signed"), the x-amz-*
// request headers missing from the signed headers. signed is sorted and
// lowercase (parseSignedHeaders); exempt lists lowercase names whose value
// is authenticated by other means.
func checkSignedAmzHeaders(h http.Header, signed []string, exempt ...string) error {
	var unsigned []string
	for name := range h {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, amzHeaderPrefix) || slices.Contains(exempt, lower) {
			continue
		}
		if _, found := slices.BinarySearch(signed, lower); !found {
			unsigned = append(unsigned, lower)
		}
	}
	if len(unsigned) == 0 {
		return nil
	}
	slices.Sort(unsigned)
	return errHeadersNotSigned(unsigned)
}

// signatureAge is the age of a signature, never negative.
func signatureAge(now, signed time.Time) time.Duration {
	if age := now.Sub(signed); age > 0 {
		return age
	}
	return 0
}

// requestTime reads the request time from X-Amz-Date or, failing that,
// Date.
func requestTime(r *http.Request) (time.Time, error) {
	if s := r.Header.Get(headerAmzDate); s != "" {
		t, err := parseAmzDate(s)
		if err != nil {
			return time.Time{}, errMissingDate()
		}
		return t, nil
	}
	if s := r.Header.Get(headerDate); s != "" {
		t, err := http.ParseTime(s)
		if err != nil {
			return time.Time{}, errMissingDate()
		}
		return t.UTC(), nil
	}
	return time.Time{}, errMissingDate()
}

// checkScope validates the credential scope against the request date, the
// configured region and the service the request is addressed to
// (ExpectedService). The error is a plain detail.
func (v *verifier) checkScope(ctx context.Context, scope credentialScope, reqTime time.Time) error {
	if scope.date != reqTime.UTC().Format(scopeDateFormat) {
		return fmt.Errorf("the credential date %s does not match the request date %s", scope.date, reqTime.UTC().Format(scopeDateFormat))
	}
	if v.opts.region != "" && scope.region != v.opts.region {
		return fmt.Errorf("the region '%s' is wrong; expecting '%s'", scope.region, v.opts.region)
	}
	if want := ExpectedService(ctx); scope.service != want {
		return fmt.Errorf("the service '%s' is wrong; expecting '%s'", scope.service, want)
	}
	return nil
}

// lookup resolves and validates the credentials of an access key and the
// session token of temporary credentials.
func (v *verifier) lookup(ctx context.Context, accessKey, token string) (domain.Credentials, domain.Identity, error) {
	creds, identity, err := v.deps.Creds.LookupCredentials(ctx, accessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) || errors.Is(err, domain.ErrUserNotFound) {
			return domain.Credentials{}, domain.Identity{}, errInvalidAccessKey(accessKey)
		}
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("auth: lookup credentials of %s: %w", accessKey, err)
	}
	if creds.SecretKey == "" {
		return domain.Credentials{}, domain.Identity{}, errInvalidAccessKey(accessKey)
	}
	if !creds.IsEnabled() {
		return domain.Credentials{}, domain.Identity{}, &domain.S3Error{
			Code: codeInvalidAccessKey, Message: "Your account is disabled; please contact your administrator.",
			Err: wrapDetail(domain.ErrAccessDenied, "access key "+accessKey+" is disabled"),
		}
	}
	if creds.IsExpired(v.deps.Clock.Now()) {
		if creds.SessionToken != "" {
			return domain.Credentials{}, domain.Identity{}, domain.NewS3Error(domain.ErrAccessDenied, codeExpiredToken, "The provided token has expired.")
		}
		return domain.Credentials{}, domain.Identity{}, &domain.S3Error{
			Code: codeInvalidAccessKey, Message: "The access key has expired.",
			Err: wrapDetail(domain.ErrAccessDenied, "access key "+accessKey+" has expired"),
		}
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(creds.SessionToken)) != 1 {
		return domain.Credentials{}, domain.Identity{}, domain.NewS3Error(domain.ErrAccessDenied, codeInvalidToken, "The provided token is malformed or otherwise invalid.")
	}
	identity.AccessKey = accessKey
	identity.IsAnonymous = false
	return creds, identity, nil
}

// payloadHash returns the payload hash of a header-signed request. It is
// the X-Amz-Content-Sha256 header, validated; without the header (generic
// SigV4 clients, STS form posts) the body is read, up to the configured
// bound, and hashed, in which case hashed is true and r.Body replays it.
func (v *verifier) payloadHash(r *http.Request) (hash string, hashed bool, err error) {
	if h := r.Header.Get(headerContentSHA256); h != "" {
		switch h {
		case unsignedPayload, streamingPayload, streamingPayloadTrailer, streamingUnsignedTrailer:
			return h, false, nil
		}
		if !isSignature(h) {
			return "", false, errInvalidContentSHA256()
		}
		return strings.ToLower(h), false, nil
	}
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return emptySHA256, true, nil
	}
	if r.ContentLength > v.opts.maxUnhashedBody {
		return "", false, errMissingContentSHA256()
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, v.opts.maxUnhashedBody+1))
	if err != nil {
		return "", false, fmt.Errorf("auth: read request body: %w", err)
	}
	if int64(len(data)) > v.opts.maxUnhashedBody {
		return "", false, errMissingContentSHA256()
	}
	r.Body = &bufferedBody{Reader: bytes.NewReader(data), closer: r.Body}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true, nil
}

func errMissingContentSHA256() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeMissingSecurityHeader,
		"Your request was missing a required header: x-amz-content-sha256.")
}

// wrapBody installs the payload verification matching the declared hash:
// aws-chunked decoding (with or without chunk signatures and trailers) or
// a SHA-256 check of the whole body.
func (v *verifier) wrapBody(r *http.Request, payloadHash string, key []byte, scope credentialScope, amzDate, seed string) error {
	switch payloadHash {
	case unsignedPayload:
		return nil
	case streamingPayload, streamingPayloadTrailer, streamingUnsignedTrailer:
		return v.wrapChunked(r, payloadHash, key, scope, amzDate, seed)
	}
	if r.Body == nil || r.Body == http.NoBody {
		if payloadHash != emptySHA256 {
			return errSHA256Mismatch()
		}
		return nil
	}
	r.Body = newSHA256Verifier(r.Body, payloadHash, r.ContentLength)
	return nil
}

// wrapChunked installs the aws-chunked decoder.
func (v *verifier) wrapChunked(r *http.Request, marker string, key []byte, scope credentialScope, amzDate, seed string) error {
	decoded, err := strconv.ParseInt(r.Header.Get(headerDecodedLength), 10, 64)
	if err != nil || decoded < 0 {
		return domain.NewS3Error(domain.ErrInvalidArgument, codeMissingContentLength,
			"You must provide a valid x-amz-decoded-content-length header for aws-chunked payloads.")
	}
	if r.Body == nil || r.Body == http.NoBody {
		return errIncomplete("an aws-chunked payload was declared but no body was sent")
	}
	cfg := chunkedConfig{
		signed:        marker != streamingUnsignedTrailer,
		trailer:       marker != streamingPayload,
		key:           key,
		scope:         scope.scope(),
		amzDate:       amzDate,
		seed:          strings.ToLower(seed),
		decodedLength: decoded,
	}
	if cfg.trailer {
		checksums, err := declaredTrailers(r.Header.Values(headerTrailer))
		if err != nil {
			return err
		}
		cfg.checksums = checksums
		if r.Trailer == nil {
			r.Trailer = http.Header{}
		}
		cfg.trailers = r.Trailer
	}
	r.Body = newChunkedReader(r.Body, cfg)
	r.ContentLength = decoded
	removeAWSChunked(r.Header)
	return nil
}

// declaredTrailers parses X-Amz-Trailer: a comma-separated list of
// x-amz-checksum-* names whose values are verified at the end of the
// payload.
func declaredTrailers(values []string) (map[string]checksumState, error) {
	out := map[string]checksumState{}
	for _, v := range values {
		for _, name := range strings.Split(v, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "" {
				continue
			}
			h, algo, ok := newChecksum(name)
			if !ok {
				return nil, errMalformedTrailer("unsupported trailer " + name + " in x-amz-trailer")
			}
			out[name] = checksumState{hash: h, algorithm: algo}
		}
	}
	return out, nil
}

// removeAWSChunked drops the aws-chunked token from Content-Encoding: the
// verifier has decoded the payload, so the remaining encodings (if any)
// describe the object.
func removeAWSChunked(h http.Header) {
	values := h.Values(headerContentEncoding)
	if len(values) == 0 {
		return
	}
	var kept []string
	for _, v := range values {
		for _, enc := range strings.Split(v, ",") {
			enc = strings.TrimSpace(enc)
			if enc != "" && !strings.EqualFold(enc, awsChunkedEncoding) {
				kept = append(kept, enc)
			}
		}
	}
	if len(kept) == 0 {
		h.Del(headerContentEncoding)
		return
	}
	h.Set(headerContentEncoding, strings.Join(kept, ","))
}
