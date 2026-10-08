package domain

import (
	"errors"
	"fmt"
)

// Sentinel errors returned by services. Transports map them to S3 error codes
// and HTTP statuses; services wrap them with fmt.Errorf("...: %w", err) to add
// context while keeping errors.Is comparisons working.
var (
	// Buckets.
	ErrBucketNotFound      = errors.New("bucket not found")
	ErrBucketAlreadyExists = errors.New("bucket already exists")
	ErrBucketNotEmpty      = errors.New("bucket not empty")
	ErrInvalidBucketName   = errors.New("invalid bucket name")

	// Objects and versions.
	ErrObjectNotFound    = errors.New("object not found")
	ErrInvalidObjectName = errors.New("invalid object name")
	ErrNoSuchVersion     = errors.New("no such version")

	// Multipart uploads.
	ErrNoSuchUpload     = errors.New("no such upload")
	ErrInvalidPart      = errors.New("invalid part")
	ErrInvalidPartOrder = errors.New("invalid part order")
	ErrEntityTooSmall   = errors.New("entity too small")
	ErrEntityTooLarge   = errors.New("entity too large")

	// Conditional requests and ranges.
	ErrPreconditionFailed = errors.New("precondition failed")
	ErrNotModified        = errors.New("not modified")
	ErrInvalidRange       = errors.New("invalid range")

	// Authentication and authorization.
	ErrAccessDenied      = errors.New("access denied")
	ErrInvalidAccessKey  = errors.New("invalid access key")
	ErrSignatureMismatch = errors.New("signature does not match")
	ErrRequestExpired    = errors.New("request expired")
	// ErrTooManyRequests reports a caller throttled after too many attempts
	// (failed console logins): HTTP 429, S3 SlowDown.
	ErrTooManyRequests = errors.New("too many requests")
	// ErrUnauthenticated reports a request that needs a valid session or
	// signature and has none (HTTP 401); ErrAccessDenied is the 403 flavor.
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrCredentialsDisabled reports the credentials of a disabled user,
	// access key or parent user: no valid credentials (S3
	// InvalidAccessKeyId, as AWS answers for an inactive key), never the
	// authorization denial of valid ones. It wraps ErrAccessDenied.
	ErrCredentialsDisabled = fmt.Errorf("credentials disabled: %w", ErrAccessDenied)

	// IAM.
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("user already exists")
	ErrPolicyNotFound    = errors.New("policy not found")
	ErrGroupNotFound     = errors.New("group not found")
	// ErrMalformedPolicy reports a policy document that is not valid JSON
	// or violates the policy grammar (S3 MalformedPolicy). It wraps
	// ErrInvalidArgument so that generic mappings keep answering 400.
	ErrMalformedPolicy = fmt.Errorf("malformed policy: %w", ErrInvalidArgument)
	// ErrPolicyTooLarge reports a policy document over the size limit
	// (S3 PolicyTooLarge). It wraps ErrInvalidArgument.
	ErrPolicyTooLarge = fmt.Errorf("policy too large: %w", ErrInvalidArgument)
	// ErrGroupAlreadyExists reports a group name already in use. It wraps
	// ErrConflict.
	ErrGroupAlreadyExists = fmt.Errorf("group already exists: %w", ErrConflict)
	// ErrAccessKeyNotFound reports an unknown service account (access key).
	// It wraps ErrUserNotFound: an access key is a principal.
	ErrAccessKeyNotFound = fmt.Errorf("access key not found: %w", ErrUserNotFound)
	// ErrAccessKeyAlreadyExists reports an access key already used by root,
	// a user, a service account or STS credentials. It wraps
	// ErrUserAlreadyExists.
	ErrAccessKeyAlreadyExists = fmt.Errorf("access key already exists: %w", ErrUserAlreadyExists)

	// Shares.
	// ErrShareNotFound reports an unknown managed share link, or one the
	// caller may not see.
	ErrShareNotFound = errors.New("share not found")

	// ErrNotFound reports a missing resource that has no more specific
	// sentinel (an unknown Admin API endpoint, healing operation...).
	// Resources with their own sentinel (bucket, object, user, group,
	// policy, share...) never use it, so that errors.Is on one of them
	// keeps its meaning.
	ErrNotFound = errors.New("not found")

	// Server and storage.
	ErrNotImplemented = errors.New("not implemented")
	ErrStorageFull    = errors.New("storage full")
	ErrDiskNotFound   = errors.New("disk not found")
	ErrFaultyDisk     = errors.New("faulty disk")
	ErrReadQuorum     = errors.New("read quorum not met")
	ErrWriteQuorum    = errors.New("write quorum not met")
	ErrLockTimeout    = errors.New("lock timeout")
	ErrQuotaExceeded  = errors.New("quota exceeded")
	ErrObjectLocked   = errors.New("object locked")
	// ErrInvalidFormat reports a malformed or unsupported on-disk document
	// (format.json, xl.meta); ErrFormatMismatch reports drives whose formats
	// disagree with each other or with the deployment layout.
	ErrInvalidFormat  = errors.New("invalid on-disk format")
	ErrFormatMismatch = errors.New("on-disk format mismatch")
	// ErrInvalidLayout reports an inconsistent deployment topology.
	ErrInvalidLayout = errors.New("invalid deployment layout")
	// ErrNodeOffline reports a peer node that cannot be reached (connection
	// refused, circuit breaker open). Remote drives of an offline node
	// report ErrDiskNotFound as well.
	ErrNodeOffline = errors.New("node offline")

	// Requests.
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrMalformedXML     = errors.New("malformed xml")
	ErrConflict         = errors.New("conflict")
	ErrMethodNotAllowed = errors.New("method not allowed")
	// ErrChecksumMismatch reports that the uploaded content does not match
	// the checksum declared by the client (Content-MD5, x-amz-content-sha256,
	// x-amz-checksum-*).
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrBadDigest is the checksum mismatch of a declared content digest
	// (Content-MD5, x-amz-checksum-*): S3 BadDigest. It wraps
	// ErrChecksumMismatch so that errors.Is matches both.
	ErrBadDigest = fmt.Errorf("bad digest: %w", ErrChecksumMismatch)
)

// S3Error decorates a sentinel error with an explicit S3 error code, a
// human-readable message and the resource the error refers to. Services use
// it when the generic mapping of a sentinel is not specific enough, for
// example to report which part of a multipart upload is invalid. Transports
// honor Code when set and fall back to the sentinel mapping otherwise.
type S3Error struct {
	// Code is the S3 error code (e.g. "InvalidPart"). Optional.
	Code string
	// Message is the human-readable description sent to the client.
	Message string
	// Resource is the bucket/object the error refers to. Optional.
	Resource string
	// Err is the wrapped sentinel error.
	Err error
}

// Error implements error.
func (e *S3Error) Error() string {
	switch {
	case e.Message != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s", e.Message, e.Err.Error())
	case e.Message != "":
		return e.Message
	case e.Err != nil:
		return e.Err.Error()
	default:
		return e.Code
	}
}

// Unwrap returns the wrapped sentinel error.
func (e *S3Error) Unwrap() error { return e.Err }

// NewS3Error builds an S3Error wrapping sentinel with a message.
func NewS3Error(sentinel error, code, message string) *S3Error {
	return &S3Error{Code: code, Message: message, Err: sentinel}
}

// WithResource returns a copy of the error annotated with the resource.
func (e *S3Error) WithResource(resource string) *S3Error {
	cp := *e
	cp.Resource = resource
	return &cp
}
