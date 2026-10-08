// Package auth defines request authentication: AWS Signature V4 for headers,
// presigned query strings, streaming chunked payloads and POST policies, plus
// the signer that produces presigned URLs. NewVerifier and NewSigner are the
// SigV4 implementations.
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
// slog.Default().
type Deps struct {
	Creds  CredentialLookup
	Clock  clock.Clock
	Logger *slog.Logger
}

// Verifier authenticates incoming requests. Every method returns the
// identity of the caller or one of domain.ErrAccessDenied,
// ErrInvalidAccessKey, ErrSignatureMismatch, ErrRequestExpired. Unsigned
// requests yield the anonymous identity with a nil error.
type Verifier interface {
	// Verify authenticates a request signed with the Authorization header
	// (including streaming aws-chunked payloads) or, when the query carries
	// X-Amz-Signature, as a presigned request.
	Verify(ctx context.Context, r *http.Request) (*domain.Identity, error)
	// VerifyPresigned authenticates a presigned URL request and checks its
	// expiry.
	VerifyPresigned(ctx context.Context, r *http.Request) (*domain.Identity, error)
	// VerifyPostPolicy authenticates a browser POST upload: it checks the
	// signature of the base64 policy and returns the decoded policy.
	VerifyPostPolicy(ctx context.Context, form PostForm) (*domain.Identity, *PostPolicy, error)
}

// Signer produces signatures for outgoing requests and presigned URLs.
type Signer interface {
	// Presign returns a presigned URL for the request described by in.
	Presign(ctx context.Context, in PresignInput) (*url.URL, error)
	// Sign adds the Authorization header (and payload hash) to req.
	Sign(ctx context.Context, req *http.Request, creds domain.Credentials, payloadSHA256 string) error
}

// CredentialLookup resolves an access key to its credentials and identity.
// iam.Service satisfies it.
type CredentialLookup interface {
	LookupCredentials(ctx context.Context, accessKey string) (domain.Credentials, domain.Identity, error)
}

// PostForm is the parsed multipart/form-data of a POST object upload.
type PostForm struct {
	Bucket string
	// Fields are the form fields (lowercase keys), file excluded.
	Fields map[string]string
	// FileName is the submitted file name (for ${filename} substitution).
	FileName string
}

// PostPolicy is a decoded POST policy document.
type PostPolicy struct {
	Expiration time.Time
	Conditions []PostCondition
}

// PostCondition is one condition of a POST policy: an exact match
// ("eq"), a prefix match ("starts-with") or a size range
// ("content-length-range", with Min/Max).
type PostCondition struct {
	Operator string
	Key      string
	Value    string
	Min      int64
	Max      int64
}

// PresignInput describes a request to presign.
type PresignInput struct {
	Method  string
	URL     *url.URL
	Headers http.Header
	// Expires is the validity duration (1s to 7 days).
	Expires time.Duration
	Creds   domain.Credentials
	Region  string
	// Now overrides the signing time (tests); zero means the clock.
	Now time.Time
}
