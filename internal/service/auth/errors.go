package auth

import (
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// S3 error codes produced by the verifier. Every error wraps a domain
// sentinel, which decides the HTTP status; the code and message are those
// AWS S3 returns in the same situation.
const (
	codeSignatureDoesNotMatch = "SignatureDoesNotMatch"
	codeHeaderMalformed       = "AuthorizationHeaderMalformed"
	codeQueryParameters       = "AuthorizationQueryParametersError"
	codeInvalidRequest        = "InvalidRequest"
	codeInvalidArgument       = "InvalidArgument"
	codeAccessDenied          = "AccessDenied"
	codeInvalidAccessKey      = "InvalidAccessKeyId"
	codeTimeTooSkewed         = "RequestTimeTooSkewed"
	codeInvalidToken          = "InvalidToken"
	codeExpiredToken          = "ExpiredToken"
	codeSHA256Mismatch        = "XAmzContentSHA256Mismatch"
	codeBadDigest             = "BadDigest"
	codeIncompleteBody        = "IncompleteBody"
	codeChunkEncoding         = "InvalidChunkEncoding"
	codeMalformedTrailer      = "MalformedTrailerError"
	codeMissingContentLength  = "MissingContentLength"
	codeMissingSecurityHeader = "MissingSecurityHeader"
	codeInvalidPolicy         = "InvalidPolicyDocument"
)

// errSignature is the generic signature mismatch. The detail never reaches
// the client: it only distinguishes the cases in logs and tests.
func errSignature(detail string) error {
	return &domain.S3Error{
		Code:    codeSignatureDoesNotMatch,
		Message: "The request signature we calculated does not match the signature you provided. Check your key and signing method.",
		Err:     wrapDetail(domain.ErrSignatureMismatch, detail),
	}
}

// errMalformedHeader reports a syntactically invalid Authorization header
// or credential scope.
func errMalformedHeader(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeHeaderMalformed, "The authorization header is malformed; "+detail+".")
}

// errQuery reports invalid presigned query parameters.
func errQuery(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeQueryParameters, detail)
}

// errHeadersNotSigned reports x-amz-* headers the signature does not
// cover. The names reach the logs, not the client.
func errHeadersNotSigned(names []string) error {
	return &domain.S3Error{
		Code:    codeAccessDenied,
		Message: "There were headers present in the request which were not signed",
		Err:     wrapDetail(domain.ErrAccessDenied, "headers not signed: "+strings.Join(names, ", ")),
	}
}

// errUnsupported reports an authentication mechanism other than SigV4.
func errUnsupported() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidRequest,
		"The authorization mechanism you have provided is not supported. Please use AWS4-HMAC-SHA256.")
}

// errAmbiguous reports a request signed both in the header and the query.
func errAmbiguous() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidArgument,
		"Only one auth mechanism allowed; only the X-Amz-Algorithm query parameter, Signature query string parameter or the Authorization header should be specified.")
}

// errMissingDate reports a header-signed request without a usable date.
func errMissingDate() error {
	return domain.NewS3Error(domain.ErrAccessDenied, codeAccessDenied, "AWS authentication requires a valid Date or x-amz-date header.")
}

// errSkewed reports a header signature outside the tolerated clock skew.
func errSkewed() error {
	return domain.NewS3Error(domain.ErrRequestExpired, codeTimeTooSkewed, "The difference between the request time and the server's time is too large.")
}

// errExpired reports an expired presigned URL or POST policy.
func errExpired(message string) error {
	return domain.NewS3Error(domain.ErrRequestExpired, codeAccessDenied, message)
}

// errNotYetValid reports a presigned URL dated in the future.
func errNotYetValid() error {
	return domain.NewS3Error(domain.ErrAccessDenied, codeAccessDenied, "Request is not yet valid.")
}

// errInvalidAccessKey reports an unknown access key.
func errInvalidAccessKey(accessKey string) error {
	return &domain.S3Error{
		Code:    codeInvalidAccessKey,
		Message: "The AWS Access Key Id you provided does not exist in our records.",
		Err:     wrapDetail(domain.ErrInvalidAccessKey, accessKey),
	}
}

// errSHA256Mismatch reports a body whose SHA-256 differs from the declared
// X-Amz-Content-Sha256.
func errSHA256Mismatch() error {
	return domain.NewS3Error(domain.ErrChecksumMismatch, codeSHA256Mismatch, "The provided 'x-amz-content-sha256' header does not match what was computed.")
}

// errInvalidContentSHA256 reports a malformed X-Amz-Content-Sha256 value.
func errInvalidContentSHA256() error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidArgument,
		"x-amz-content-sha256 must be UNSIGNED-PAYLOAD, a STREAMING-* marker or the hex SHA-256 of the payload.")
}

// errMalformedChunk reports an invalid aws-chunked framing.
func errMalformedChunk(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeChunkEncoding, "The chunk encoding of the payload is invalid: "+detail+".")
}

// errIncomplete reports a truncated payload.
func errIncomplete(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeIncompleteBody, "The request body is incomplete: "+detail+".")
}

// errMalformedTrailer reports missing, undeclared or invalid trailers.
func errMalformedTrailer(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeMalformedTrailer,
		"The request contained trailing data that was not well-formed or did not conform to our published schema: "+detail+".")
}

// errBadDigest reports a trailing checksum that does not match the payload.
func errBadDigest(algorithm string) error {
	return domain.NewS3Error(domain.ErrBadDigest, codeBadDigest, "The "+algorithm+" you specified did not match the calculated checksum.")
}

// errPolicy reports a POST form that violates its policy.
func errPolicy(detail string) error {
	return domain.NewS3Error(domain.ErrAccessDenied, codeAccessDenied, "Invalid according to Policy: "+detail)
}

// errPolicyDocument reports a malformed POST policy document.
func errPolicyDocument(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidPolicy, detail)
}

// errPostArgument reports a missing or malformed POST form field.
func errPostArgument(detail string) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, codeInvalidArgument, detail)
}

// wrapDetail annotates a sentinel with a detail, keeping errors.Is.
func wrapDetail(sentinel error, detail string) error {
	if detail == "" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, detail)
}
