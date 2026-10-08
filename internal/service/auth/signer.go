package auth

import (
	"context"
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

// signer produces SigV4 signatures and presigned URLs.
type signer struct {
	deps Deps
	opts options
}

var _ Signer = (*signer)(nil)

// NewSigner returns the SigV4 signer. Creds is not needed: callers pass the
// credentials to sign with.
func NewSigner(deps Deps, opts ...Option) Signer {
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
	return &signer{deps: deps, opts: o}
}

func (s *signer) region(requested string) string {
	switch {
	case requested != "":
		return requested
	case s.opts.region != "":
		return s.opts.region
	default:
		return "us-east-1"
	}
}

// Presign implements Signer.
func (s *signer) Presign(_ context.Context, in PresignInput) (*url.URL, error) {
	if in.URL == nil || in.URL.Host == "" {
		return nil, fmt.Errorf("%w: presign requires an absolute URL", domain.ErrInvalidArgument)
	}
	if in.Expires < time.Second || in.Expires > maxPresignExpiry {
		return nil, fmt.Errorf("%w: expiry must be between 1 second and 7 days", domain.ErrInvalidArgument)
	}
	if in.Creds.AccessKey == "" || in.Creds.SecretKey == "" {
		return nil, fmt.Errorf("%w: presign requires credentials", domain.ErrInvalidArgument)
	}
	method := in.Method
	if method == "" {
		method = http.MethodGet
	}
	now := in.Now
	if now.IsZero() {
		now = s.deps.Clock.Now()
	}
	now = now.UTC()
	scope := credentialScope{accessKey: in.Creds.AccessKey, date: now.Format(scopeDateFormat), region: s.region(in.Region), service: serviceS3}

	req := &http.Request{Method: method, URL: in.URL, Host: in.URL.Host, Header: http.Header{}}
	signed := []string{"host"}
	for name, values := range in.Headers {
		lower := strings.ToLower(name)
		if lower == "host" {
			continue
		}
		req.Header[http.CanonicalHeaderKey(name)] = values
		signed = append(signed, lower)
	}
	sort.Strings(signed)

	q := in.URL.Query()
	q.Set(queryAlgorithm, algorithm)
	q.Set(queryCredential, scope.String())
	q.Set(queryDate, now.Format(amzDateFormat))
	q.Set(queryExpires, strconv.FormatInt(int64(in.Expires/time.Second), 10))
	q.Set(querySignedHeaders, strings.Join(signed, signedHeaderSepartor))
	if in.Creds.SessionToken != "" {
		q.Set(querySecurityToken, in.Creds.SessionToken)
	}
	q.Del(querySignature)

	headers, err := canonicalHeaders(req, signed)
	if err != nil {
		return nil, err
	}
	query := canonicalQuery(q, "")
	canonical := canonicalRequest(method, encodePath(in.URL.Path), query, headers, signed, unsignedPayload)
	sig := signatureHex(signingKey(in.Creds.SecretKey, scope), stringToSign(now.Format(amzDateFormat), scope, canonical))

	out := *in.URL
	out.RawQuery = query + "&" + querySignature + "=" + sig
	return &out, nil
}

// Sign implements Signer.
func (s *signer) Sign(_ context.Context, req *http.Request, creds domain.Credentials, payloadSHA256 string) error {
	if req == nil || req.URL == nil {
		return fmt.Errorf("%w: nil request", domain.ErrInvalidArgument)
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return fmt.Errorf("%w: sign requires credentials", domain.ErrInvalidArgument)
	}
	if req.Header == nil {
		req.Header = http.Header{}
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}
	now := s.deps.Clock.Now().UTC()
	if payloadSHA256 == "" {
		payloadSHA256 = unsignedPayload
	}
	req.Header.Set(headerAmzDate, now.Format(amzDateFormat))
	req.Header.Set(headerContentSHA256, payloadSHA256)
	if creds.SessionToken != "" {
		req.Header.Set(headerSecurityToken, creds.SessionToken)
	}
	scope := credentialScope{accessKey: creds.AccessKey, date: now.Format(scopeDateFormat), region: s.region(""), service: serviceS3}

	signed := []string{"host"}
	for name := range req.Header {
		lower := strings.ToLower(name)
		switch {
		case strings.HasPrefix(lower, "x-amz-"), lower == "content-type", lower == "content-md5", lower == "range":
			signed = append(signed, lower)
		}
	}
	if req.ContentLength > 0 {
		signed = append(signed, "content-length")
	}
	sort.Strings(signed)
	headers, err := canonicalHeaders(req, signed)
	if err != nil {
		return err
	}
	canonical := canonicalRequest(req.Method, encodePath(req.URL.Path), canonicalQuery(req.URL.Query(), ""), headers, signed, payloadSHA256)
	sig := signatureHex(signingKey(creds.SecretKey, scope), stringToSign(now.Format(amzDateFormat), scope, canonical))
	req.Header.Set(headerAuthorization, fmt.Sprintf("%s Credential=%s, SignedHeaders=%s, Signature=%s",
		algorithm, scope.String(), strings.Join(signed, signedHeaderSepartor), sig))
	return nil
}
