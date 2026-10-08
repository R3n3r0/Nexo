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
	// ErrUnauthenticated reports a request that needs a valid session or
	// signature and has none (HTTP 401); ErrAccessDenied is the 403 flavor.
	ErrUnauthenticated = errors.New("unauthenticated")

	// IAM.
	ErrUserNotFound      = errors.New("user not found")
	ErrUserAlreadyExists = errors.New("user already exists")
	ErrPolicyNotFound    = errors.New("policy not found")
	ErrGroupNotFound     = errors.New("group not found")

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

	// Requests.
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrMalformedXML     = errors.New("malformed xml")
	ErrConflict         = errors.New("conflict")
	ErrMethodNotAllowed = errors.New("method not allowed")
	// ErrChecksumMismatch reports that the uploaded content does not match
	// the checksum declared by the client (Content-MD5, x-amz-content-sha256,
	// x-amz-checksum-*).
	ErrChecksumMismatch = errors.New("checksum mismatch")
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
