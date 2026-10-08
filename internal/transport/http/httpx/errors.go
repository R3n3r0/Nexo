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

// S3 error codes used by the transport.
var (
	CodeNoSuchBucket          = S3Code{"NoSuchBucket", "The specified bucket does not exist", http.StatusNotFound}
	CodeNoSuchKey             = S3Code{"NoSuchKey", "The specified key does not exist.", http.StatusNotFound}
	CodeNoSuchVersion         = S3Code{"NoSuchVersion", "The specified version does not exist.", http.StatusNotFound}
	CodeBucketAlreadyExists   = S3Code{"BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", http.StatusConflict}
	CodeBucketNotEmpty        = S3Code{"BucketNotEmpty", "The bucket you tried to delete is not empty", http.StatusConflict}
	CodeAccessDenied          = S3Code{"AccessDenied", "Access Denied.", http.StatusForbidden}
	CodeInvalidAccessKeyID    = S3Code{"InvalidAccessKeyId", "The Access Key Id you provided does not exist in our records.", http.StatusForbidden}
	CodeSignatureDoesNotMatch = S3Code{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method.", http.StatusForbidden}
	CodeRequestTimeTooSkewed  = S3Code{"RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.", http.StatusForbidden}
	CodeInvalidBucketName     = S3Code{"InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest}
	CodeInvalidObjectName     = S3Code{"XNexoInvalidObjectName", "Object name contains unsupported characters.", http.StatusBadRequest}
	CodeInvalidArgument       = S3Code{"InvalidArgument", "Invalid Argument", http.StatusBadRequest}
	CodeMalformedXML          = S3Code{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest}
	CodeNoSuchUpload          = S3Code{"NoSuchUpload", "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.", http.StatusNotFound}
	CodeInvalidPart           = S3Code{"InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.", http.StatusBadRequest}
	CodeInvalidPartOrder      = S3Code{"InvalidPartOrder", "The list of parts was not in ascending order. The parts list must be specified in order by part number.", http.StatusBadRequest}
	CodeEntityTooSmall        = S3Code{"EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.", http.StatusBadRequest}
	CodeEntityTooLarge        = S3Code{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", http.StatusBadRequest}
	CodePreconditionFailed    = S3Code{"PreconditionFailed", "At least one of the pre-conditions you specified did not hold", http.StatusPreconditionFailed}
	CodeNotModified           = S3Code{"NotModified", "Not Modified", http.StatusNotModified}
	CodeInvalidRange          = S3Code{"InvalidRange", "The requested range is not satisfiable", http.StatusRequestedRangeNotSatisfiable}
	CodeNotImplemented        = S3Code{"NotImplemented", "A header you provided implies functionality that is not implemented", http.StatusNotImplemented}
	CodeSlowDown              = S3Code{"SlowDown", "Resource requested is unreadable, please reduce your request rate", http.StatusServiceUnavailable}
	CodeQuotaExceeded         = S3Code{"XNexoQuotaExceeded", "Bucket quota exceeded", http.StatusInsufficientStorage}
	CodeObjectLocked          = S3Code{"XNexoObjectLocked", "Object is WORM protected and cannot be overwritten or deleted", http.StatusForbidden}
	CodeStorageFull           = S3Code{"XNexoStorageFull", "Storage backend has reached its minimum free drive threshold. Please delete a few objects to proceed.", http.StatusInsufficientStorage}
	CodeInternalError         = S3Code{"InternalError", "We encountered an internal error, please try again.", http.StatusInternalServerError}
	CodeMethodNotAllowed      = S3Code{"MethodNotAllowed", "The specified method is not allowed against this resource.", http.StatusMethodNotAllowed}
	CodeNoSuchUser            = S3Code{"XNexoAdminNoSuchUser", "The specified user does not exist.", http.StatusNotFound}
	CodeUserAlreadyExists     = S3Code{"XNexoAdminUserAlreadyExists", "The specified user already exists.", http.StatusConflict}
	CodeNoSuchPolicy          = S3Code{"XNexoAdminNoSuchPolicy", "The specified policy does not exist.", http.StatusNotFound}
	CodeNoSuchGroup           = S3Code{"XNexoAdminNoSuchGroup", "The specified group does not exist.", http.StatusNotFound}
	CodeRequestTimeout        = S3Code{"RequestTimeout", "Your socket connection to the server was not read from or written to within the timeout period.", http.StatusRequestTimeout}
	CodeConflict              = S3Code{"XNexoConflict", "The request conflicts with the current state of the resource.", http.StatusConflict}
)

// errorMapping maps a sentinel to its S3 code. The order matters only for
// readability: sentinels are distinct, so at most one entry matches.
type errorMapping struct {
	err  error
	code S3Code
}

var errorMappings = []errorMapping{
	{domain.ErrBucketNotFound, CodeNoSuchBucket},
	{domain.ErrBucketAlreadyExists, CodeBucketAlreadyExists},
	{domain.ErrBucketNotEmpty, CodeBucketNotEmpty},
	{domain.ErrInvalidBucketName, CodeInvalidBucketName},
	{domain.ErrObjectNotFound, CodeNoSuchKey},
	{domain.ErrInvalidObjectName, CodeInvalidObjectName},
	{domain.ErrNoSuchVersion, CodeNoSuchVersion},
	{domain.ErrNoSuchUpload, CodeNoSuchUpload},
	{domain.ErrInvalidPart, CodeInvalidPart},
	{domain.ErrInvalidPartOrder, CodeInvalidPartOrder},
	{domain.ErrEntityTooSmall, CodeEntityTooSmall},
	{domain.ErrEntityTooLarge, CodeEntityTooLarge},
	{domain.ErrPreconditionFailed, CodePreconditionFailed},
	{domain.ErrNotModified, CodeNotModified},
	{domain.ErrInvalidRange, CodeInvalidRange},
	{domain.ErrAccessDenied, CodeAccessDenied},
	{domain.ErrInvalidAccessKey, CodeInvalidAccessKeyID},
	{domain.ErrSignatureMismatch, CodeSignatureDoesNotMatch},
	{domain.ErrRequestExpired, CodeRequestTimeTooSkewed},
	{domain.ErrUserNotFound, CodeNoSuchUser},
	{domain.ErrUserAlreadyExists, CodeUserAlreadyExists},
	{domain.ErrPolicyNotFound, CodeNoSuchPolicy},
	{domain.ErrGroupNotFound, CodeNoSuchGroup},
	{domain.ErrNotImplemented, CodeNotImplemented},
	{domain.ErrStorageFull, CodeStorageFull},
	{domain.ErrDiskNotFound, CodeSlowDown},
	{domain.ErrFaultyDisk, CodeSlowDown},
	{domain.ErrReadQuorum, CodeSlowDown},
	{domain.ErrWriteQuorum, CodeSlowDown},
	{domain.ErrLockTimeout, CodeSlowDown},
	{domain.ErrQuotaExceeded, CodeQuotaExceeded},
	{domain.ErrObjectLocked, CodeObjectLocked},
	{domain.ErrInvalidArgument, CodeInvalidArgument},
	{domain.ErrMalformedXML, CodeMalformedXML},
	{domain.ErrConflict, CodeConflict},
	{context.DeadlineExceeded, CodeRequestTimeout},
}

// MapS3Error maps an error to its S3 code and the message to send. A
// *domain.S3Error with an explicit Code overrides the sentinel mapping; its
// Message, when set, replaces the default description. Unknown errors map
// to InternalError and never leak their text.
func MapS3Error(err error) (S3Code, string) {
	if err == nil {
		return CodeInternalError, CodeInternalError.Description
	}
	code := CodeInternalError
	matched := false
	for _, m := range errorMappings {
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

// problemMapping maps a sentinel to a problem status and code.
type problemMapping struct {
	err    error
	status int
	code   string
}

var problemMappings = []problemMapping{
	{domain.ErrBucketNotFound, http.StatusNotFound, "bucket_not_found"},
	{domain.ErrBucketAlreadyExists, http.StatusConflict, "bucket_already_exists"},
	{domain.ErrBucketNotEmpty, http.StatusConflict, "bucket_not_empty"},
	{domain.ErrInvalidBucketName, http.StatusBadRequest, "invalid_bucket_name"},
	{domain.ErrObjectNotFound, http.StatusNotFound, "object_not_found"},
	{domain.ErrInvalidObjectName, http.StatusBadRequest, "invalid_object_name"},
	{domain.ErrNoSuchVersion, http.StatusNotFound, "version_not_found"},
	{domain.ErrNoSuchUpload, http.StatusNotFound, "upload_not_found"},
	{domain.ErrInvalidPart, http.StatusBadRequest, "invalid_part"},
	{domain.ErrInvalidPartOrder, http.StatusBadRequest, "invalid_part_order"},
	{domain.ErrEntityTooSmall, http.StatusBadRequest, "entity_too_small"},
	{domain.ErrPreconditionFailed, http.StatusPreconditionFailed, "precondition_failed"},
	{domain.ErrNotModified, http.StatusNotModified, "not_modified"},
	{domain.ErrAccessDenied, http.StatusForbidden, "access_denied"},
	{domain.ErrInvalidAccessKey, http.StatusUnauthorized, "invalid_access_key"},
	{domain.ErrSignatureMismatch, http.StatusUnauthorized, "signature_mismatch"},
	{domain.ErrRequestExpired, http.StatusUnauthorized, "request_expired"},
	{domain.ErrUserNotFound, http.StatusNotFound, "user_not_found"},
	{domain.ErrUserAlreadyExists, http.StatusConflict, "user_already_exists"},
	{domain.ErrPolicyNotFound, http.StatusNotFound, "policy_not_found"},
	{domain.ErrGroupNotFound, http.StatusNotFound, "group_not_found"},
	{domain.ErrNotImplemented, http.StatusNotImplemented, "not_implemented"},
	{domain.ErrStorageFull, http.StatusInsufficientStorage, "storage_full"},
	{domain.ErrDiskNotFound, http.StatusServiceUnavailable, "disk_not_found"},
	{domain.ErrFaultyDisk, http.StatusServiceUnavailable, "faulty_disk"},
	{domain.ErrReadQuorum, http.StatusServiceUnavailable, "read_quorum"},
	{domain.ErrWriteQuorum, http.StatusServiceUnavailable, "write_quorum"},
	{domain.ErrLockTimeout, http.StatusServiceUnavailable, "lock_timeout"},
	{domain.ErrQuotaExceeded, http.StatusInsufficientStorage, "quota_exceeded"},
	{domain.ErrObjectLocked, http.StatusForbidden, "object_locked"},
	{domain.ErrInvalidArgument, http.StatusBadRequest, "invalid_argument"},
	{domain.ErrMalformedXML, http.StatusBadRequest, "malformed_xml"},
	{domain.ErrConflict, http.StatusConflict, "conflict"},
	{domain.ErrEntityTooLarge, http.StatusRequestEntityTooLarge, "entity_too_large"},
	{domain.ErrInvalidRange, http.StatusRequestedRangeNotSatisfiable, "invalid_range"},
}

// ProblemFromError maps an error to a Problem. Unknown errors become a
// generic 500 without leaking the error text.
func ProblemFromError(err error) Problem {
	for _, m := range problemMappings {
		if errors.Is(err, m.err) {
			return Problem{Title: http.StatusText(m.status), Status: m.status, Code: m.code, Detail: err.Error()}
		}
	}
	return Problem{Title: http.StatusText(http.StatusInternalServerError), Status: http.StatusInternalServerError, Code: "internal_error"}
}
