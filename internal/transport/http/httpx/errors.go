package httpx

import (
	"context"
	"errors"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
)

// S3Code describes an S3 error: code, default message and HTTP status.
type S3Code struct {
	Code        string
	Description string
	HTTPStatus  int
}

// S3Codes is the table of S3 error codes used by the transport. It is
// returned by value (DefaultS3Codes) and owned by each codec, so that no
// package-level state can be mutated by importers.
type S3Codes struct {
	NoSuchBucket          S3Code
	NoSuchKey             S3Code
	NoSuchVersion         S3Code
	BucketAlreadyExists   S3Code
	BucketNotEmpty        S3Code
	AccessDenied          S3Code
	InvalidAccessKeyID    S3Code
	SignatureDoesNotMatch S3Code
	RequestTimeTooSkewed  S3Code
	InvalidBucketName     S3Code
	InvalidObjectName     S3Code
	InvalidArgument       S3Code
	MalformedXML          S3Code
	NoSuchUpload          S3Code
	InvalidPart           S3Code
	InvalidPartOrder      S3Code
	EntityTooSmall        S3Code
	EntityTooLarge        S3Code
	PreconditionFailed    S3Code
	NotModified           S3Code
	InvalidRange          S3Code
	NotImplemented        S3Code
	SlowDown              S3Code
	QuotaExceeded         S3Code
	ObjectLocked          S3Code
	StorageFull           S3Code
	InternalError         S3Code
	MethodNotAllowed      S3Code
	NoSuchUser            S3Code
	UserAlreadyExists     S3Code
	NoSuchPolicy          S3Code
	NoSuchGroup           S3Code
	RequestTimeout        S3Code
	Conflict              S3Code
	BadDigest             S3Code
	InvalidFormat         S3Code
}

// DefaultS3Codes returns a fresh copy of the standard code table.
func DefaultS3Codes() S3Codes {
	return S3Codes{
		NoSuchBucket:          S3Code{"NoSuchBucket", "The specified bucket does not exist", http.StatusNotFound},
		NoSuchKey:             S3Code{"NoSuchKey", "The specified key does not exist.", http.StatusNotFound},
		NoSuchVersion:         S3Code{"NoSuchVersion", "The specified version does not exist.", http.StatusNotFound},
		BucketAlreadyExists:   S3Code{"BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", http.StatusConflict},
		BucketNotEmpty:        S3Code{"BucketNotEmpty", "The bucket you tried to delete is not empty", http.StatusConflict},
		AccessDenied:          S3Code{"AccessDenied", "Access Denied.", http.StatusForbidden},
		InvalidAccessKeyID:    S3Code{"InvalidAccessKeyId", "The Access Key Id you provided does not exist in our records.", http.StatusForbidden},
		SignatureDoesNotMatch: S3Code{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method.", http.StatusForbidden},
		RequestTimeTooSkewed:  S3Code{"RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.", http.StatusForbidden},
		InvalidBucketName:     S3Code{"InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest},
		InvalidObjectName:     S3Code{"XNexoInvalidObjectName", "Object name contains unsupported characters.", http.StatusBadRequest},
		InvalidArgument:       S3Code{"InvalidArgument", "Invalid Argument", http.StatusBadRequest},
		MalformedXML:          S3Code{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest},
		NoSuchUpload:          S3Code{"NoSuchUpload", "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.", http.StatusNotFound},
		InvalidPart:           S3Code{"InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", http.StatusBadRequest},
		InvalidPartOrder:      S3Code{"InvalidPartOrder", "The list of parts was not in ascending order. The parts list must be specified in order by part number.", http.StatusBadRequest},
		EntityTooSmall:        S3Code{"EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.", http.StatusBadRequest},
		EntityTooLarge:        S3Code{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", http.StatusBadRequest},
		PreconditionFailed:    S3Code{"PreconditionFailed", "At least one of the pre-conditions you specified did not hold", http.StatusPreconditionFailed},
		NotModified:           S3Code{"NotModified", "Not Modified", http.StatusNotModified},
		InvalidRange:          S3Code{"InvalidRange", "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable},
		NotImplemented:        S3Code{"NotImplemented", "A header you provided implies functionality that is not implemented", http.StatusNotImplemented},
		SlowDown:              S3Code{"SlowDown", "Resource requested is unreadable, please reduce your request rate", http.StatusServiceUnavailable},
		QuotaExceeded:         S3Code{"XNexoQuotaExceeded", "Bucket quota exceeded", http.StatusInsufficientStorage},
		ObjectLocked:          S3Code{"XNexoObjectLocked", "Object is WORM protected and cannot be overwritten or deleted", http.StatusForbidden},
		StorageFull:           S3Code{"XNexoStorageFull", "Storage backend has reached its minimum free drive threshold. Please delete a few objects to proceed.", http.StatusInsufficientStorage},
		InternalError:         S3Code{"InternalError", "We encountered an internal error, please try again.", http.StatusInternalServerError},
		MethodNotAllowed:      S3Code{"MethodNotAllowed", "The specified method is not allowed against this resource.", http.StatusMethodNotAllowed},
		NoSuchUser:            S3Code{"XNexoAdminNoSuchUser", "The specified user does not exist.", http.StatusNotFound},
		UserAlreadyExists:     S3Code{"XNexoAdminUserAlreadyExists", "The specified user already exists.", http.StatusConflict},
		NoSuchPolicy:          S3Code{"XNexoAdminNoSuchPolicy", "The specified policy does not exist.", http.StatusNotFound},
		NoSuchGroup:           S3Code{"XNexoAdminNoSuchGroup", "The specified group does not exist.", http.StatusNotFound},
		RequestTimeout:        S3Code{"RequestTimeout", "Your socket connection to the server was not read from or written to within the timeout period.", http.StatusRequestTimeout},
		Conflict:              S3Code{"XNexoConflict", "The request conflicts with the current state of the resource.", http.StatusConflict},
		BadDigest:             S3Code{"BadDigest", "The Content-MD5 or checksum you specified did not match what we received.", http.StatusBadRequest},
		InvalidFormat:         S3Code{"XNexoInvalidFormat", "The on-disk format is invalid or inconsistent.", http.StatusInternalServerError},
	}
}

// errorMapping maps a sentinel to its S3 code and problem+json equivalent.
type errorMapping struct {
	err     error
	code    S3Code
	status  int
	problem string
}

// mappings builds the sentinel table from a code table. Sentinels are
// distinct, so at most one entry matches an error.
func mappings(c S3Codes) []errorMapping {
	return []errorMapping{
		{domain.ErrBucketNotFound, c.NoSuchBucket, http.StatusNotFound, "bucket_not_found"},
		{domain.ErrBucketAlreadyExists, c.BucketAlreadyExists, http.StatusConflict, "bucket_already_exists"},
		{domain.ErrBucketNotEmpty, c.BucketNotEmpty, http.StatusConflict, "bucket_not_empty"},
		{domain.ErrInvalidBucketName, c.InvalidBucketName, http.StatusBadRequest, "invalid_bucket_name"},
		{domain.ErrObjectNotFound, c.NoSuchKey, http.StatusNotFound, "object_not_found"},
		{domain.ErrInvalidObjectName, c.InvalidObjectName, http.StatusBadRequest, "invalid_object_name"},
		{domain.ErrNoSuchVersion, c.NoSuchVersion, http.StatusNotFound, "version_not_found"},
		{domain.ErrNoSuchUpload, c.NoSuchUpload, http.StatusNotFound, "upload_not_found"},
		{domain.ErrInvalidPart, c.InvalidPart, http.StatusBadRequest, "invalid_part"},
		{domain.ErrInvalidPartOrder, c.InvalidPartOrder, http.StatusBadRequest, "invalid_part_order"},
		{domain.ErrEntityTooSmall, c.EntityTooSmall, http.StatusBadRequest, "entity_too_small"},
		{domain.ErrEntityTooLarge, c.EntityTooLarge, http.StatusRequestEntityTooLarge, "entity_too_large"},
		{domain.ErrPreconditionFailed, c.PreconditionFailed, http.StatusPreconditionFailed, "precondition_failed"},
		{domain.ErrNotModified, c.NotModified, http.StatusNotModified, "not_modified"},
		{domain.ErrInvalidRange, c.InvalidRange, http.StatusRequestedRangeNotSatisfiable, "invalid_range"},
		{domain.ErrAccessDenied, c.AccessDenied, http.StatusForbidden, "access_denied"},
		{domain.ErrUnauthenticated, c.AccessDenied, http.StatusUnauthorized, "unauthenticated"},
		{domain.ErrInvalidAccessKey, c.InvalidAccessKeyID, http.StatusUnauthorized, "invalid_access_key"},
		{domain.ErrSignatureMismatch, c.SignatureDoesNotMatch, http.StatusUnauthorized, "signature_mismatch"},
		{domain.ErrRequestExpired, c.RequestTimeTooSkewed, http.StatusUnauthorized, "request_expired"},
		{domain.ErrUserNotFound, c.NoSuchUser, http.StatusNotFound, "user_not_found"},
		{domain.ErrUserAlreadyExists, c.UserAlreadyExists, http.StatusConflict, "user_already_exists"},
		{domain.ErrPolicyNotFound, c.NoSuchPolicy, http.StatusNotFound, "policy_not_found"},
		{domain.ErrGroupNotFound, c.NoSuchGroup, http.StatusNotFound, "group_not_found"},
		{domain.ErrNotImplemented, c.NotImplemented, http.StatusNotImplemented, "not_implemented"},
		{domain.ErrStorageFull, c.StorageFull, http.StatusInsufficientStorage, "storage_full"},
		{domain.ErrDiskNotFound, c.SlowDown, http.StatusServiceUnavailable, "disk_not_found"},
		{domain.ErrFaultyDisk, c.SlowDown, http.StatusServiceUnavailable, "faulty_disk"},
		{domain.ErrReadQuorum, c.SlowDown, http.StatusServiceUnavailable, "read_quorum"},
		{domain.ErrWriteQuorum, c.SlowDown, http.StatusServiceUnavailable, "write_quorum"},
		{domain.ErrLockTimeout, c.SlowDown, http.StatusServiceUnavailable, "lock_timeout"},
		{domain.ErrQuotaExceeded, c.QuotaExceeded, http.StatusInsufficientStorage, "quota_exceeded"},
		{domain.ErrObjectLocked, c.ObjectLocked, http.StatusForbidden, "object_locked"},
		{domain.ErrInvalidFormat, c.InvalidFormat, http.StatusInternalServerError, "invalid_format"},
		{domain.ErrFormatMismatch, c.InvalidFormat, http.StatusInternalServerError, "format_mismatch"},
		{domain.ErrInvalidLayout, c.InvalidFormat, http.StatusInternalServerError, "invalid_layout"},
		{domain.ErrInvalidArgument, c.InvalidArgument, http.StatusBadRequest, "invalid_argument"},
		{domain.ErrMalformedXML, c.MalformedXML, http.StatusBadRequest, "malformed_xml"},
		{domain.ErrConflict, c.Conflict, http.StatusConflict, "conflict"},
		{domain.ErrMethodNotAllowed, c.MethodNotAllowed, http.StatusMethodNotAllowed, "method_not_allowed"},
		{domain.ErrChecksumMismatch, c.BadDigest, http.StatusBadRequest, "checksum_mismatch"},
		{context.DeadlineExceeded, c.RequestTimeout, http.StatusRequestTimeout, "request_timeout"},
	}
}

// Codes returns a copy of the code table of the codec.
func (c *codec) Codes() S3Codes { return c.codes }

// MapS3Error implements Encoder. A *domain.S3Error with an explicit Code
// overrides the sentinel mapping; its Message, when set, replaces the
// default description. Unknown errors map to InternalError and never leak
// their text.
func (c *codec) MapS3Error(err error) (S3Code, string) {
	if err == nil {
		return c.codes.InternalError, c.codes.InternalError.Description
	}
	code := c.codes.InternalError
	matched := false
	for _, m := range c.mappings {
		if errors.Is(err, m.err) {
			code, matched = m.code, true
			break
		}
	}
	var s3err *domain.S3Error
	if errors.As(err, &s3err) {
		if s3err.Code != "" {
			code.Code = s3err.Code
			if !matched && s3err.Err == nil {
				code.HTTPStatus = http.StatusBadRequest
			}
		}
		if s3err.Message != "" {
			return code, s3err.Message
		}
	}
	return code, code.Description
}

// S3ErrorResource returns the resource recorded in a *domain.S3Error, or "".
func S3ErrorResource(err error) string {
	var s3err *domain.S3Error
	if errors.As(err, &s3err) {
		return s3err.Resource
	}
	return ""
}

// Problem is an RFC 9457 problem details document with a stable code.
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	// Code is the stable machine-readable error code (snake_case).
	Code string `json:"code"`
	// RequestID correlates the problem with the server logs.
	RequestID string `json:"requestId,omitempty"`
}

// ProblemFromError implements Encoder. Unknown errors become a generic 500
// without leaking the error text.
func (c *codec) ProblemFromError(err error) Problem {
	for _, m := range c.mappings {
		if errors.Is(err, m.err) {
			return Problem{Title: http.StatusText(m.status), Status: m.status, Code: m.problem, Detail: err.Error()}
		}
	}
	return Problem{Title: http.StatusText(http.StatusInternalServerError), Status: http.StatusInternalServerError, Code: "internal_error"}
}
