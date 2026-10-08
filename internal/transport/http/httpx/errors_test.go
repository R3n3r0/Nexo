package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
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
			c := New()
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
	}
	c := New()
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
	c := New()
	err := fmt.Errorf("complete: %w", domain.NewS3Error(domain.ErrInvalidPart, "InvalidPartNumber", "part 7 is odd").WithResource("/b/k"))
	code, msg := c.MapS3Error(err)
	assert.Equal(t, "InvalidPartNumber", code.Code)
	assert.Equal(t, http.StatusBadRequest, code.HTTPStatus)
	assert.Equal(t, "part 7 is odd", msg)
	assert.Equal(t, "/b/k", S3ErrorResource(err))
	assert.Empty(t, S3ErrorResource(domain.ErrConflict))

	// A code with no sentinel defaults to 400.
	code, _ = c.MapS3Error(&domain.S3Error{Code: "XNexoCustom"})
	assert.Equal(t, http.StatusBadRequest, code.HTTPStatus)
	// A message without a code keeps the sentinel code.
	code, msg = c.MapS3Error(&domain.S3Error{Message: "bucket gone", Err: domain.ErrBucketNotFound})
	assert.Equal(t, "NoSuchBucket", code.Code)
	assert.Equal(t, "bucket gone", msg)
}

func TestProblemFromError(t *testing.T) {
	c := New()
	p := c.ProblemFromError(fmt.Errorf("x: %w", domain.ErrAccessDenied))
	assert.Equal(t, 403, p.Status)
	assert.Equal(t, "access_denied", p.Code)
	assert.Equal(t, "Forbidden", p.Title)
	assert.Contains(t, p.Detail, "access denied")

	p = c.ProblemFromError(errors.New("secret internal detail"))
	assert.Equal(t, 500, p.Status)
	assert.Equal(t, "internal_error", p.Code)
	assert.Empty(t, p.Detail, "internal details must not leak")
	require.Equal(t, "Internal Server Error", p.Title)
}
