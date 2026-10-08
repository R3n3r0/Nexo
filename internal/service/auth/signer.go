package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// signer produces SigV4 signatures, presigned URLs and POST policies.
type signer struct {
	deps Deps
	opts options
	keys *keyCache
}

var _ Signer = (*signer)(nil)

// NewSigner returns the SigV4 signer. Creds is not needed: callers pass
// the credentials to sign with.
func NewSigner(deps Deps, opts ...Option) Signer {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := applyOptions(opts)
	return &signer{deps: deps, opts: o, keys: newKeyCache(o.keyCacheSize)}
}

// region returns the signing region: the requested one, the configured one
// or us-east-1.
func (s *signer) region(requested string) string {
	switch {
	case requested != "":
		return requested
	case s.opts.region != "":
		return s.opts.region
	default:
		return defaultRegion
	}
}

// signingTime returns the signing time: the override or the clock, in UTC
// and truncated to the second (the precision of X-Amz-Date).
func (s *signer) signingTime(override time.Time) time.Time {
	now := override
	if now.IsZero() {
		now = s.deps.Clock.Now()
	}
	return now.UTC().Truncate(time.Second)
}

func validCreds(c domain.Credentials) bool {
	return c.AccessKey != "" && c.SecretKey != ""
}

func validExpiry(d time.Duration) bool {
	return d >= time.Second && d <= maxPresignExpiry
}

// Presign implements Signer.
func (s *signer) Presign(_ context.Context, in PresignInput) (*url.URL, error) {
	if in.URL == nil || in.URL.Host == "" {
		return nil, fmt.Errorf("%w: presign requires an absolute URL", domain.ErrInvalidArgument)
	}
	if !validExpiry(in.Expires) {
		return nil, fmt.Errorf("%w: presign expiry must be between 1 second and 7 days", domain.ErrInvalidArgument)
	}
	if !validCreds(in.Creds) {
		return nil, fmt.Errorf("%w: presign requires credentials", domain.ErrInvalidArgument)
	}
	method := in.Method
	if method == "" {
		method = http.MethodGet
	}
	now := s.signingTime(in.Now)
	scope := credentialScope{accessKey: in.Creds.AccessKey, date: now.Format(scopeDateFormat), region: s.region(in.Region), service: ServiceS3}

	req := &http.Request{Method: method, URL: in.URL, Host: in.URL.Host, Header: http.Header{}}
	signed := []string{"host"}
	for name, values := range in.Headers {
		lower := strings.ToLower(name)
		if lower == "host" || len(values) == 0 {
			continue
		}
		req.Header[http.CanonicalHeaderKey(name)] = values
		signed = append(signed, lower)
	}
	sort.Strings(signed)

	q, err := parseQuery(in.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: presign: %w", domain.ErrInvalidArgument, err)
	}
	for _, name := range []string{queryAlgorithm, queryCredential, queryDate, queryExpires, querySignedHeaders, querySignature, querySecurityToken} {
		q.Del(name)
	}
	q.Set(queryAlgorithm, algorithm)
	q.Set(queryCredential, scope.String())
	q.Set(queryDate, now.Format(amzDateFormat))
	q.Set(queryExpires, strconv.FormatInt(int64(in.Expires/time.Second), 10))
	q.Set(querySignedHeaders, strings.Join(signed, signedHeaderSeparator))
	if in.Creds.SessionToken != "" {
		q.Set(querySecurityToken, in.Creds.SessionToken)
	}

	headers, missing := canonicalHeaders(req, signed)
	if missing != "" {
		return nil, fmt.Errorf("%w: presign: header %s has no value", domain.ErrInvalidArgument, missing)
	}
	query := canonicalQuery(q, "")
	path := encodePath(in.URL.Path)
	canonical := canonicalRequest(method, path, query, headers, signed, unsignedPayload)
	sig := signatureHex(s.keys.get(in.Creds.SecretKey, scope), stringToSign(now.Format(amzDateFormat), scope, canonical))

	out := *in.URL
	out.Path = in.URL.Path
	if out.Path == "" {
		out.Path = "/"
	}
	out.RawPath = encodePath(out.Path)
	out.RawQuery = query + "&" + querySignature + "=" + sig
	return &out, nil
}

// isUnsignedHeader reports the headers Sign leaves out of the signature:
// hop-by-hop or rewritten by proxies and HTTP stacks (as the AWS SDKs).
func isUnsignedHeader(lower string) bool {
	switch lower {
	case "authorization", "user-agent", "x-amzn-trace-id", "expect", "transfer-encoding", "connection", "content-length":
		return true
	}
	return false
}

// Sign implements Signer.
func (s *signer) Sign(_ context.Context, req *http.Request, creds domain.Credentials, payloadSHA256 string) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("%w: nil request", domain.ErrInvalidArgument)
	}
	if !validCreds(creds) {
		return fmt.Errorf("%w: sign requires credentials", domain.ErrInvalidArgument)
	}
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	if req.Host == "" {
		return fmt.Errorf("%w: sign requires a host", domain.ErrInvalidArgument)
	}
	switch {
	case payloadSHA256 == "":
		payloadSHA256 = unsignedPayload
	case payloadSHA256 == unsignedPayload, payloadSHA256 == streamingPayload,
		payloadSHA256 == streamingPayloadTrailer, payloadSHA256 == streamingUnsignedTrailer:
	case isSignature(payloadSHA256):
		payloadSHA256 = strings.ToLower(payloadSHA256)
	default:
		return fmt.Errorf("%w: invalid payload hash %q", domain.ErrInvalidArgument, payloadSHA256)
	}
	q, err := parseQuery(req.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("%w: sign: %w", domain.ErrInvalidArgument, err)
	}
	now := s.signingTime(time.Time{})
	req.Header.Del(headerAuthorization)
	req.Header.Set(headerAmzDate, now.Format(amzDateFormat))
	req.Header.Set(headerContentSHA256, payloadSHA256)
	if creds.SessionToken != "" {
		req.Header.Set(headerSecurityToken, creds.SessionToken)
	}
	scope := credentialScope{accessKey: creds.AccessKey, date: now.Format(scopeDateFormat), region: s.region(""), service: ServiceS3}

	signed := []string{"host"}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if lower != "host" && !isUnsignedHeader(lower) {
			signed = append(signed, lower)
		}
	}
	if req.ContentLength > 0 {
		signed = append(signed, "content-length")
	}
	sort.Strings(signed)
	headers, missing := canonicalHeaders(req, signed)
	if missing != "" {
		return fmt.Errorf("%w: sign: header %s has no value", domain.ErrInvalidArgument, missing)
	}
	canonical := canonicalRequest(req.Method, encodePath(req.URL.Path), canonicalQuery(q, ""), headers, signed, payloadSHA256)
	sig := signatureHex(s.keys.get(creds.SecretKey, scope), stringToSign(now.Format(amzDateFormat), scope, canonical))
	req.Header.Set(headerAuthorization, algorithm+" Credential="+scope.String()+", SignedHeaders="+strings.Join(signed, signedHeaderSeparator)+", Signature="+sig)
	return nil
}

// PresignPostPolicy implements Signer.
func (s *signer) PresignPostPolicy(_ context.Context, in PostPolicyInput) (*PostPolicyForm, error) {
	if in.Bucket == "" {
		return nil, fmt.Errorf("%w: post policy requires a bucket", domain.ErrInvalidArgument)
	}
	if !validExpiry(in.Expires) {
		return nil, fmt.Errorf("%w: post policy expiry must be between 1 second and 7 days", domain.ErrInvalidArgument)
	}
	if !validCreds(in.Creds) {
		return nil, fmt.Errorf("%w: post policy requires credentials", domain.ErrInvalidArgument)
	}
	key := in.Key
	if key == "" {
		if in.KeyPrefix == "" {
			return nil, fmt.Errorf("%w: post policy requires a key or a key prefix", domain.ErrInvalidArgument)
		}
		key = in.KeyPrefix + filenameVariable
	}
	if in.KeyPrefix != "" && !strings.HasPrefix(key, in.KeyPrefix) {
		return nil, fmt.Errorf("%w: post policy key %q does not start with the prefix %q", domain.ErrInvalidArgument, key, in.KeyPrefix)
	}

	now := s.signingTime(in.Now)
	scope := credentialScope{accessKey: in.Creds.AccessKey, date: now.Format(scopeDateFormat), region: s.region(in.Region), service: ServiceS3}
	expiration := now.Add(in.Expires)
	fields := map[string]string{
		fieldKey:        key,
		fieldAlgorithm:  algorithm,
		fieldCredential: scope.String(),
		fieldDate:       now.Format(amzDateFormat),
	}
	conditions := []PostCondition{
		{Operator: ConditionEq, Key: fieldBucket, Value: in.Bucket},
		{Operator: ConditionEq, Key: fieldAlgorithm, Value: algorithm},
		{Operator: ConditionEq, Key: fieldCredential, Value: scope.String()},
		{Operator: ConditionEq, Key: fieldDate, Value: fields[fieldDate]},
	}
	if in.KeyPrefix != "" {
		conditions = append(conditions, PostCondition{Operator: ConditionStartsWith, Key: fieldKey, Value: in.KeyPrefix})
	} else {
		conditions = append(conditions, PostCondition{Operator: ConditionEq, Key: fieldKey, Value: key})
	}
	if in.Creds.SessionToken != "" {
		fields[fieldSecurityToken] = in.Creds.SessionToken
		conditions = append(conditions, PostCondition{Operator: ConditionEq, Key: fieldSecurityToken, Value: in.Creds.SessionToken})
	}
	names := make([]string, 0, len(in.Fields))
	for name := range in.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lower := strings.ToLower(name)
		if _, reserved := fields[lower]; reserved || lower == fieldPolicy || lower == fieldSignature || lower == fieldBucket || lower == fieldFile {
			return nil, fmt.Errorf("%w: post policy field %s is reserved", domain.ErrInvalidArgument, name)
		}
		fields[name] = in.Fields[name]
		conditions = append(conditions, PostCondition{Operator: ConditionEq, Key: lower, Value: in.Fields[name]})
	}
	for _, c := range in.Conditions {
		c.Key = strings.ToLower(strings.TrimPrefix(c.Key, "$"))
		switch c.Operator {
		case ConditionEq, ConditionStartsWith:
			if c.Key == "" {
				return nil, fmt.Errorf("%w: post policy condition %s needs a field", domain.ErrInvalidArgument, c.Operator)
			}
		case ConditionLengthRange:
			if c.Min < 0 || c.Max < c.Min {
				return nil, fmt.Errorf("%w: invalid content-length-range [%d, %d]", domain.ErrInvalidArgument, c.Min, c.Max)
			}
		default:
			return nil, fmt.Errorf("%w: unknown post policy operator %q", domain.ErrInvalidArgument, c.Operator)
		}
		conditions = append(conditions, c)
	}

	doc, err := encodePolicy(expiration, conditions)
	if err != nil {
		return nil, err
	}
	policyB64 := base64.StdEncoding.EncodeToString(doc)
	fields[fieldPolicy] = policyB64
	fields[fieldSignature] = signatureHex(s.keys.get(in.Creds.SecretKey, scope), policyB64)
	return &PostPolicyForm{Fields: fields, Expiration: expiration, Policy: PostPolicy{Expiration: expiration, Conditions: conditions}}, nil
}

// encodePolicy renders a POST policy document.
func encodePolicy(expiration time.Time, conditions []PostCondition) ([]byte, error) {
	conds := make([]any, 0, len(conditions))
	for _, c := range conditions {
		switch c.Operator {
		case ConditionLengthRange:
			conds = append(conds, []any{ConditionLengthRange, c.Min, c.Max})
		default:
			conds = append(conds, []any{c.Operator, "$" + c.Key, c.Value})
		}
	}
	doc, err := json.Marshal(map[string]any{
		"expiration": expiration.UTC().Format("2006-01-02T15:04:05.000Z"),
		"conditions": conds,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: encode post policy: %w", err)
	}
	return doc, nil
}
