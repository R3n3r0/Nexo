// Package auth defines request authentication: AWS Signature V4 for headers,
// presigned query strings, streaming aws-chunked payloads (signed chunks,
// unsigned chunks with trailers) and POST policies, plus the signer that
// produces signed requests, presigned URLs and POST policy forms.
// NewVerifier and NewSigner are the SigV4 implementations.
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the SigV4 verifier and signer. Creds is
// required by the verifier; Clock and Logger default to the wall clock and
// slog.Default(). The region is set with WithRegion.
type Deps struct {
	Creds  CredentialLookup
	Clock  clock.Clock
	Logger *slog.Logger
}

// Verifier authenticates incoming requests.
//
// Every method returns the identity of the caller, with AuthType and
// SignatureAge set, or an error wrapping one of the domain sentinels
// (usually as a *domain.S3Error carrying the S3 error code):
//   - domain.ErrSignatureMismatch: SignatureDoesNotMatch;
//   - domain.ErrInvalidAccessKey: InvalidAccessKeyId;
//   - domain.ErrRequestExpired: RequestTimeTooSkewed (header signatures out
//     of the tolerated clock skew) or AccessDenied "Request has expired"
//     (presigned URLs, POST policies);
//   - domain.ErrAccessDenied: disabled or expired credentials, wrong
//     session token, POST policy conditions not met, missing date;
//   - domain.ErrInvalidArgument: malformed authorization data
//     (AuthorizationHeaderMalformed, AuthorizationQueryParametersError) and
//     unsupported mechanisms such as Signature Version 2 (InvalidRequest).
//
// The credential scope of a signature must name the configured region (if
// any) and the service the request is addressed to: ServiceS3 unless the
// context says otherwise (WithExpectedService, ServiceSTS for the STS API);
// another service is refused as malformed authorization data.
//
// Unsigned requests yield the anonymous identity (IsAnonymous, AuthType
// ANONYMOUS) with a nil error: authorization is the policy engine's job.
//
// When the request carries a payload hash the verifier replaces r.Body with
// a reader that checks it while streaming: the SHA-256 of the whole body
// (XAmzContentSHA256Mismatch, wrapping domain.ErrChecksumMismatch), the
// signature of every aws-chunked chunk and of the trailer, the decoded
// length and the x-amz-checksum-* trailer (BadDigest, wrapping
// domain.ErrBadDigest). The error surfaces from the Read that completes the
// faulty unit, so callers must treat any read error as a failed upload. For
// aws-chunked payloads r.ContentLength becomes the decoded length,
// "aws-chunked" is removed from Content-Encoding and the decoded trailing
// headers are stored in r.Trailer once the body has been read.
type Verifier interface {
	// Verify authenticates a request signed with the Authorization header
	// (including streaming aws-chunked payloads) or, when the query carries
	// X-Amz-Algorithm/X-Amz-Signature, as a presigned request.
	Verify(ctx context.Context, r *http.Request) (*domain.Identity, error)
	// VerifyPresigned authenticates a presigned URL request: it checks the
	// X-Amz-* query parameters, the expiry (1 s to 7 days, relative to
	// X-Amz-Date) and the signature, computed with UNSIGNED-PAYLOAD unless
	// the query carries X-Amz-Content-Sha256.
	VerifyPresigned(ctx context.Context, r *http.Request) (*domain.Identity, error)
	// VerifyPostPolicy authenticates a browser POST upload: it checks the
	// signature of the base64 policy, its expiration and every condition
	// against the form fields, and returns the decoded policy so that the
	// caller can enforce the content-length-range while streaming the file.
	VerifyPostPolicy(ctx context.Context, form PostForm) (*domain.Identity, *PostPolicy, error)
	// VerifyBearerToken authenticates a bearer token of the metrics
	// endpoints (Authorization: Bearer): a JWT signed with HMAC-SHA2 by the
	// secret key of its subject (an access key), with the issuer
	// BearerTokenIssuer and an expiry, as MinIO's "mc admin prometheus
	// generate" produces. The identity is the one of the access key, with
	// AuthType domain.AuthTypeBearer; the S3 API never accepts it.
	VerifyBearerToken(ctx context.Context, token string) (*domain.Identity, error)
}

// Signer produces signatures for outgoing requests, presigned URLs and POST
// policy forms (CLI, console, share links).
type Signer interface {
	// Presign returns a presigned URL for the request described by in. The
	// payload is UNSIGNED-PAYLOAD; in.Headers are signed and must be sent
	// unchanged by the client.
	Presign(ctx context.Context, in PresignInput) (*url.URL, error)
	// Sign adds the Authorization header (plus X-Amz-Date,
	// X-Amz-Content-Sha256 and, for temporary credentials,
	// X-Amz-Security-Token) to req. An empty payloadSHA256 means
	// UNSIGNED-PAYLOAD; the streaming markers are accepted as well.
	Sign(ctx context.Context, req *http.Request, creds domain.Credentials, payloadSHA256 string) error
	// PresignPostPolicy builds and signs a POST policy and returns the form
	// fields a browser must submit (the file excluded).
	PresignPostPolicy(ctx context.Context, in PostPolicyInput) (*PostPolicyForm, error)
	// SignBearerToken returns a bearer token of the metrics endpoints for
	// creds (HS512, subject the access key, issuer BearerTokenIssuer),
	// valid for ttl.
	SignBearerToken(ctx context.Context, creds domain.Credentials, ttl time.Duration) (string, error)
}

// CredentialLookup resolves an access key to its credentials and identity.
// For temporary (STS) credentials Credentials.SessionToken is the token the
// request must carry in X-Amz-Security-Token. Unknown keys are reported
// with domain.ErrInvalidAccessKey (or domain.ErrUserNotFound). iam.Service
// satisfies it.
type CredentialLookup interface {
	LookupCredentials(ctx context.Context, accessKey string) (domain.Credentials, domain.Identity, error)
}

// PostForm is the parsed multipart/form-data of a POST object upload.
type PostForm struct {
	// Bucket is the bucket of the request (from the path or host).
	Bucket string
	// Fields are the form fields preceding the file; keys are matched
	// case-insensitively.
	Fields map[string]string
	// FileName is the submitted file name (for ${filename} substitution).
	FileName string
}

// PostPolicy is a decoded POST policy document. Check validates a field
// value against its conditions and ContentLengthRange returns the size
// bounds of the file.
type PostPolicy struct {
	Expiration time.Time
	Conditions []PostCondition
}

// PostCondition is one condition of a POST policy: an exact match
// ("eq"), a prefix match ("starts-with") or a size range
// ("content-length-range", with Min/Max). Key is the lowercase field name
// without the leading "$".
type PostCondition struct {
	Operator string
	Key      string
	Value    string
	Min      int64
	Max      int64
}

// PresignInput describes a request to presign.
type PresignInput struct {
	// Method defaults to GET.
	Method string
	// URL is the absolute URL of the request (path-style or
	// virtual-host-style); its query (e.g. versionId, response-*) is
	// signed.
	URL *url.URL
	// Headers are additional headers to sign (Content-Type, x-amz-meta-*,
	// SSE-C keys...).
	Headers http.Header
	// Expires is the validity duration (1s to 7 days).
	Expires time.Duration
	Creds   domain.Credentials
	// Region overrides the signer region.
	Region string
	// Now overrides the signing time (tests); zero means the clock.
	Now time.Time
}

// PostPolicyInput describes a browser upload form to sign.
type PostPolicyInput struct {
	// Bucket is the destination bucket (required).
	Bucket string
	// Key is the key form value; "${filename}" is replaced by the name of
	// the uploaded file. Without KeyPrefix the policy requires this exact
	// key.
	Key string
	// KeyPrefix, when set, lets the browser choose any key starting with
	// it (starts-with condition). An empty Key then defaults to
	// KeyPrefix + "${filename}".
	KeyPrefix string
	// Fields are additional form fields, each bound by an exact-match
	// condition (Content-Type, success_action_status,
	// success_action_redirect, x-amz-meta-*, ...).
	Fields map[string]string
	// Conditions are additional conditions (starts-with on Content-Type,
	// content-length-range, ...).
	Conditions []PostCondition
	// Expires is the validity of the policy (1s to 7 days).
	Expires time.Duration
	Creds   domain.Credentials
	// Region overrides the signer region.
	Region string
	// Now overrides the signing time (tests); zero means the clock.
	Now time.Time
}

// PostPolicyForm is a signed POST policy form.
type PostPolicyForm struct {
	// Fields are the form fields to submit before the file: key, policy,
	// x-amz-algorithm, x-amz-credential, x-amz-date, x-amz-signature,
	// x-amz-security-token (temporary credentials) and the input Fields.
	Fields map[string]string
	// Expiration is the expiration of the policy.
	Expiration time.Time
	// Policy is the decoded policy that was signed.
	Policy PostPolicy
}
