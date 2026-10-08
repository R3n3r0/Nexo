package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestMapS3ErrorTable(t *testing.T) {
	tests := []struct {
		err    error
		code   string
		status int
	}{
		{domain.ErrBucketNotFound, "NoSuchBucket", 404},
		{domain.ErrObjectNotFound, "NoSuchKey", 404},
		{domain.ErrNoSuchVersion, "NoSuchVersion", 404},
		{domain.ErrBucketAlreadyExists, "BucketAlreadyOwnedByYou", 409},
		{domain.ErrBucketNotEmpty, "BucketNotEmpty", 409},
		{domain.ErrAccessDenied, "AccessDenied", 403},
		{domain.ErrInvalidAccessKey, "InvalidAccessKeyId", 403},
		{domain.ErrSignatureMismatch, "SignatureDoesNotMatch", 403},
		{domain.ErrRequestExpired, "RequestTimeTooSkewed", 403},
		{domain.ErrInvalidBucketName, "InvalidBucketName", 400},
		{domain.ErrInvalidObjectName, "XNexoInvalidObjectName", 400},
		{domain.ErrInvalidArgument, "InvalidArgument", 400},
		{domain.ErrMalformedXML, "MalformedXML", 400},
		{domain.ErrNoSuchUpload, "NoSuchUpload", 404},
		{domain.ErrInvalidPart, "InvalidPart", 400},
		{domain.ErrInvalidPartOrder, "InvalidPartOrder", 400},
		{domain.ErrEntityTooSmall, "EntityTooSmall", 400},
		{domain.ErrEntityTooLarge, "EntityTooLarge", 400},
		{domain.ErrPreconditionFailed, "PreconditionFailed", 412},
		{domain.ErrNotModified, "NotModified", 304},
		{domain.ErrInvalidRange, "InvalidRange", 416},
		{domain.ErrNotImplemented, "NotImplemented", 501},
		{domain.ErrDiskNotFound, "SlowDown", 503},
		{domain.ErrFaultyDisk, "SlowDown", 503},
		{domain.ErrReadQuorum, "SlowDown", 503},
		{domain.ErrWriteQuorum, "SlowDown", 503},
		{domain.ErrLockTimeout, "SlowDown", 503},
		{domain.ErrTooManyRequests, "SlowDown", 503},
		{domain.ErrQuotaExceeded, "XNexoQuotaExceeded", 507},
		{domain.ErrStorageFull, "XNexoStorageFull", 507},
		{domain.ErrObjectLocked, "XNexoObjectLocked", 403},
		{domain.ErrUserNotFound, "XNexoAdminNoSuchUser", 404},
		{domain.ErrUserAlreadyExists, "XNexoAdminUserAlreadyExists", 409},
		{domain.ErrPolicyNotFound, "XNexoAdminNoSuchPolicy", 404},
		{domain.ErrGroupNotFound, "XNexoAdminNoSuchGroup", 404},
		{domain.ErrConflict, "XNexoConflict", 409},
		{domain.ErrMethodNotAllowed, "MethodNotAllowed", 405},
		{domain.ErrChecksumMismatch, "BadDigest", 400},
		{domain.ErrUnauthenticated, "AccessDenied", 403},
		{domain.ErrInvalidFormat, "XNexoInvalidFormat", 500},
		{context.DeadlineExceeded, "RequestTimeout", 408},
		{errors.New("something unexpected"), "InternalError", 500},
		{nil, "InternalError", 500},
	}
	for _, tc := range tests {
		name := "nil"
		if tc.err != nil {
			name = tc.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			c := httpx.New()
			code, msg := c.MapS3Error(tc.err)
			assert.Equal(t, tc.code, code.Code)
			assert.Equal(t, tc.status, code.HTTPStatus)
			assert.NotEmpty(t, msg)
			if tc.err != nil {
				// Wrapped errors map identically.
				wrapped, _ := c.MapS3Error(fmt.Errorf("layer: %w", tc.err))
				assert.Equal(t, code, wrapped)
			}
		})
	}
}

func TestMapS3ErrorEverySentinelIsMapped(t *testing.T) {
	sentinels := []error{
		domain.ErrBucketNotFound, domain.ErrBucketAlreadyExists, domain.ErrBucketNotEmpty, domain.ErrInvalidBucketName,
		domain.ErrObjectNotFound, domain.ErrInvalidObjectName, domain.ErrNoSuchVersion, domain.ErrNoSuchUpload,
		domain.ErrInvalidPart, domain.ErrInvalidPartOrder, domain.ErrEntityTooSmall, domain.ErrEntityTooLarge,
		domain.ErrPreconditionFailed, domain.ErrNotModified, domain.ErrInvalidRange, domain.ErrAccessDenied,
		domain.ErrInvalidAccessKey, domain.ErrSignatureMismatch, domain.ErrRequestExpired, domain.ErrUserNotFound,
		domain.ErrUserAlreadyExists, domain.ErrPolicyNotFound, domain.ErrGroupNotFound, domain.ErrNotImplemented,
		domain.ErrStorageFull, domain.ErrDiskNotFound, domain.ErrFaultyDisk, domain.ErrReadQuorum, domain.ErrWriteQuorum,
		domain.ErrLockTimeout, domain.ErrQuotaExceeded, domain.ErrObjectLocked, domain.ErrInvalidArgument,
		domain.ErrMalformedXML, domain.ErrConflict, domain.ErrMethodNotAllowed, domain.ErrChecksumMismatch,
		domain.ErrUnauthenticated, domain.ErrInvalidFormat, domain.ErrFormatMismatch, domain.ErrInvalidLayout,
		domain.ErrShareNotFound, domain.ErrTooManyRequests,
	}
	c := httpx.New()
	for _, s := range sentinels {
		code, _ := c.MapS3Error(s)
		assert.NotEqual(t, c.Codes().InternalError, code, "sentinel %v has no S3 mapping", s)
		p := c.ProblemFromError(s)
		assert.NotEqual(t, "internal_error", p.Code, "sentinel %v has no problem mapping", s)
	}
	// The code table is a copy: mutating it does not affect the codec.
	codes := c.Codes()
	codes.NoSuchBucket.HTTPStatus = 500
	code, _ := c.MapS3Error(domain.ErrBucketNotFound)
	assert.Equal(t, 404, code.HTTPStatus)
}

func TestMapS3ErrorHonoursS3Error(t *testing.T) {
	c := httpx.New()
	err := fmt.Errorf("complete: %w", domain.NewS3Error(domain.ErrInvalidPart, "InvalidPartNumber", "part 7 is odd").WithResource("/b/k"))
	code, msg := c.MapS3Error(err)
	assert.Equal(t, "InvalidPartNumber", code.Code)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, code.HTTPStatus, "a known code brings its canonical status")
	assert.Equal(t, "part 7 is odd", msg)
	assert.Equal(t, "/b/k", httpx.S3ErrorResource(err))
	assert.Empty(t, httpx.S3ErrorResource(domain.ErrConflict))

	// A code with no sentinel defaults to 400.
	code, msg = c.MapS3Error(&domain.S3Error{Code: "XNexoCustom"})
	assert.Equal(t, http.StatusBadRequest, code.HTTPStatus)
	assert.Equal(t, "XNexoCustom", code.Code)
	assert.Equal(t, c.Codes().InvalidRequest.Description, msg, "no internal text leaks")
	// An unknown code keeps the status of the wrapped sentinel.
	code, _ = c.MapS3Error(domain.NewS3Error(domain.ErrBucketNotFound, "XNexoGone", ""))
	assert.Equal(t, "XNexoGone", code.Code)
	assert.Equal(t, http.StatusNotFound, code.HTTPStatus)
	// A message without a code keeps the sentinel code.
	code, msg = c.MapS3Error(&domain.S3Error{Message: "bucket gone", Err: domain.ErrBucketNotFound})
	assert.Equal(t, "NoSuchBucket", code.Code)
	assert.Equal(t, "bucket gone", msg)
}

func TestProblemFromError(t *testing.T) {
	c := httpx.New()
	p := c.ProblemFromError(fmt.Errorf("x: %w", domain.ErrAccessDenied))
	assert.Equal(t, 403, p.Status)
	assert.Equal(t, "access_denied", p.Code)
	assert.Equal(t, "Forbidden", p.Title)
	assert.Contains(t, p.Detail, "access denied")

	p = c.ProblemFromError(fmt.Errorf("share abc: %w", domain.ErrShareNotFound))
	assert.Equal(t, 404, p.Status)
	assert.Equal(t, "share_not_found", p.Code)

	p = c.ProblemFromError(fmt.Errorf("heal h1: %w", domain.ErrNotFound))
	assert.Equal(t, 404, p.Status)
	assert.Equal(t, "not_found", p.Code)

	p = c.ProblemFromError(fmt.Errorf("login: %w", domain.ErrTooManyRequests))
	assert.Equal(t, 429, p.Status)
	assert.Equal(t, "too_many_requests", p.Code)

	p = c.ProblemFromError(errors.New("secret internal detail"))
	assert.Equal(t, 500, p.Status)
	assert.Equal(t, "internal_error", p.Code)
	assert.Empty(t, p.Detail, "internal details must not leak")
	require.Equal(t, "Internal Server Error", p.Title)
}

func TestMapS3ErrorExplicitCodes(t *testing.T) {
	tests := []struct {
		sentinel error
		code     string
		status   int
	}{
		{domain.ErrChecksumMismatch, "BadDigest", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "InvalidDigest", http.StatusBadRequest},
		{domain.ErrInvalidObjectName, "KeyTooLongError", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "MetadataTooLarge", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "MissingContentLength", http.StatusLengthRequired},
		{domain.ErrInvalidRange, "InvalidPartNumber", http.StatusRequestedRangeNotSatisfiable},
		{domain.ErrPolicyNotFound, "NoSuchTagSet", http.StatusNotFound},
		{domain.ErrPolicyNotFound, "NoSuchBucketPolicy", http.StatusNotFound},
		{domain.ErrPolicyNotFound, "NoSuchLifecycleConfiguration", http.StatusNotFound},
		{domain.ErrPolicyNotFound, "NoSuchCORSConfiguration", http.StatusNotFound},
		{domain.ErrPolicyNotFound, "ServerSideEncryptionConfigurationNotFoundError", http.StatusNotFound},
		{domain.ErrPolicyNotFound, "ObjectLockConfigurationNotFoundError", http.StatusNotFound},
		{domain.ErrInvalidArgument, "PolicyTooLarge", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "MalformedPolicy", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "InvalidRequest", http.StatusBadRequest},
		{domain.ErrMethodNotAllowed, "MethodNotAllowed", http.StatusMethodNotAllowed},
		{domain.ErrChecksumMismatch, "XAmzContentSHA256Mismatch", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "IncompleteBody", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "InvalidLocationConstraint", http.StatusBadRequest},
		{domain.ErrConflict, "InvalidBucketState", http.StatusConflict},
		{domain.ErrInvalidArgument, "InvalidTag", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "InvalidStorageClass", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "MalformedPOSTRequest", http.StatusBadRequest},
		{domain.ErrMalformedXML, "IllegalVersioningConfigurationException", http.StatusBadRequest},
		{domain.ErrSignatureMismatch, "AuthorizationHeaderMalformed", http.StatusBadRequest},
		{domain.ErrSignatureMismatch, "AuthorizationQueryParametersError", http.StatusBadRequest},
		{domain.ErrAccessDenied, "InvalidToken", http.StatusBadRequest},
		{domain.ErrAccessDenied, "ExpiredToken", http.StatusBadRequest},
		{domain.ErrRequestExpired, "AccessDenied", http.StatusForbidden},
		{domain.ErrMalformedXML, "MaxMessageLengthExceeded", http.StatusBadRequest},
		{domain.ErrInvalidArgument, "InvalidPolicyDocument", http.StatusBadRequest},
	}
	c := httpx.New()
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			code, msg := c.MapS3Error(fmt.Errorf("layer: %w", domain.NewS3Error(tc.sentinel, tc.code, "")))
			assert.Equal(t, tc.code, code.Code)
			assert.Equal(t, tc.status, code.HTTPStatus)
			assert.NotEmpty(t, msg)
			// The default description is the one of the code, not of the sentinel.
			assert.Equal(t, code.Description, msg)
		})
	}
	// ErrBadDigest maps to BadDigest on its own.
	code, _ := c.MapS3Error(fmt.Errorf("put: %w", domain.ErrBadDigest))
	assert.Equal(t, "BadDigest", code.Code)
}

func TestS3CodesAreUnique(t *testing.T) {
	c := httpx.New().Codes()
	seen := map[string]bool{}
	for _, code := range []httpx.S3Code{
		c.InvalidDigest, c.KeyTooLongError, c.MetadataTooLarge, c.MissingContentLength, c.InvalidPartNumber,
		c.NoSuchTagSet, c.NoSuchBucketPolicy, c.PolicyTooLarge, c.MalformedPolicy, c.InvalidRequest,
		c.XAmzContentSHA256Mismatch, c.IncompleteBody, c.InvalidLocationConstraint, c.InvalidBucketState,
		c.MethodNotAllowed, c.BadDigest,
	} {
		assert.False(t, seen[code.Code], "duplicate code %s", code.Code)
		seen[code.Code] = true
		assert.NotEmpty(t, code.Description)
		assert.NotZero(t, code.HTTPStatus)
	}
}
