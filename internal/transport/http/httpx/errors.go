package httpx

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
	// SlowDownRead and SlowDownWrite report a set that lost its read or
	// write quorum (too many drives or nodes offline), LockTimeout a lock
	// that could not be acquired in time and ServiceUnavailable a drive or
	// node offline: all 503, retryable.
	SlowDownRead       S3Code
	SlowDownWrite      S3Code
	LockTimeout        S3Code
	ServiceUnavailable S3Code
	QuotaExceeded      S3Code
	ObjectLocked       S3Code
	StorageFull        S3Code
	InternalError      S3Code
	MethodNotAllowed   S3Code
	NoSuchUser         S3Code
	UserAlreadyExists  S3Code
	NoSuchPolicy       S3Code
	NoSuchGroup        S3Code
	RequestTimeout     S3Code
	Conflict           S3Code
	BadDigest          S3Code
	InvalidFormat      S3Code

	// Codes raised with an explicit *domain.S3Error code: their HTTP
	// status is the canonical one of the code, whatever the wrapped
	// sentinel.
	InvalidDigest                     S3Code
	KeyTooLongError                   S3Code
	MetadataTooLarge                  S3Code
	MissingContentLength              S3Code
	InvalidPartNumber                 S3Code
	NoSuchTagSet                      S3Code
	NoSuchBucketPolicy                S3Code
	NoSuchLifecycleConfiguration      S3Code
	NoSuchCORSConfiguration           S3Code
	NoSuchEncryptionConfiguration     S3Code
	NoSuchObjectLockConfiguration     S3Code
	PolicyTooLarge                    S3Code
	MalformedPolicy                   S3Code
	InvalidPolicyDocument             S3Code
	InvalidRequest                    S3Code
	XAmzContentSHA256Mismatch         S3Code
	IncompleteBody                    S3Code
	InvalidLocationConstraint         S3Code
	InvalidBucketState                S3Code
	InvalidTag                        S3Code
	InvalidStorageClass               S3Code
	MalformedPOSTRequest              S3Code
	IllegalVersioningConfiguration    S3Code
	AuthorizationHeaderMalformed      S3Code
	AuthorizationQueryParametersError S3Code
	InvalidToken                      S3Code
	ExpiredToken                      S3Code
	MaxMessageLengthExceeded          S3Code
}

// all lists every code of the table; the codec indexes them by name.
func (c S3Codes) all() []S3Code {
	return []S3Code{
		c.NoSuchBucket, c.NoSuchKey, c.NoSuchVersion, c.BucketAlreadyExists, c.BucketNotEmpty, c.AccessDenied,
		c.InvalidAccessKeyID, c.SignatureDoesNotMatch, c.RequestTimeTooSkewed, c.InvalidBucketName,
		c.InvalidObjectName, c.InvalidArgument, c.MalformedXML, c.NoSuchUpload, c.InvalidPart, c.InvalidPartOrder,
		c.EntityTooSmall, c.EntityTooLarge, c.PreconditionFailed, c.NotModified, c.InvalidRange, c.NotImplemented,
		c.SlowDown, c.SlowDownRead, c.SlowDownWrite, c.LockTimeout, c.ServiceUnavailable, c.QuotaExceeded, c.ObjectLocked, c.StorageFull, c.InternalError, c.MethodNotAllowed,
		c.NoSuchUser, c.UserAlreadyExists, c.NoSuchPolicy, c.NoSuchGroup, c.RequestTimeout, c.Conflict,
		c.BadDigest, c.InvalidFormat,
		c.InvalidDigest, c.KeyTooLongError, c.MetadataTooLarge, c.MissingContentLength, c.InvalidPartNumber,
		c.NoSuchTagSet, c.NoSuchBucketPolicy, c.NoSuchLifecycleConfiguration, c.NoSuchCORSConfiguration,
		c.NoSuchEncryptionConfiguration, c.NoSuchObjectLockConfiguration, c.PolicyTooLarge, c.MalformedPolicy,
		c.InvalidPolicyDocument, c.InvalidRequest, c.XAmzContentSHA256Mismatch, c.IncompleteBody,
		c.InvalidLocationConstraint, c.InvalidBucketState, c.InvalidTag, c.InvalidStorageClass,
		c.MalformedPOSTRequest, c.IllegalVersioningConfiguration, c.AuthorizationHeaderMalformed,
		c.AuthorizationQueryParametersError, c.InvalidToken, c.ExpiredToken, c.MaxMessageLengthExceeded,
	}
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
		SlowDown:              S3Code{"SlowDown", "Please reduce your request rate.", http.StatusServiceUnavailable},
		SlowDownRead:          S3Code{"SlowDownRead", "The deployment has lost its read quorum (too many drives or nodes offline), please retry later.", http.StatusServiceUnavailable},
		SlowDownWrite:         S3Code{"SlowDownWrite", "The deployment has lost its write quorum (too many drives or nodes offline), please retry later.", http.StatusServiceUnavailable},
		LockTimeout:           S3Code{"XNexoLockTimeout", "The resource is locked by another operation, or too many nodes are offline to lock it, please retry later.", http.StatusServiceUnavailable},
		ServiceUnavailable:    S3Code{"ServiceUnavailable", "A drive or node needed by the request is offline, please retry later.", http.StatusServiceUnavailable},
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

		InvalidDigest:                     S3Code{"InvalidDigest", "The Content-MD5 you specified is not valid.", http.StatusBadRequest},
		KeyTooLongError:                   S3Code{"KeyTooLongError", "Your key is too long.", http.StatusBadRequest},
		MetadataTooLarge:                  S3Code{"MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size.", http.StatusBadRequest},
		MissingContentLength:              S3Code{"MissingContentLength", "You must provide the Content-Length HTTP header.", http.StatusLengthRequired},
		InvalidPartNumber:                 S3Code{"InvalidPartNumber", "The requested partnumber is not satisfiable.", http.StatusRequestedRangeNotSatisfiable},
		NoSuchTagSet:                      S3Code{"NoSuchTagSet", "The TagSet does not exist.", http.StatusNotFound},
		NoSuchBucketPolicy:                S3Code{"NoSuchBucketPolicy", "The bucket policy does not exist.", http.StatusNotFound},
		NoSuchLifecycleConfiguration:      S3Code{"NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist.", http.StatusNotFound},
		NoSuchCORSConfiguration:           S3Code{"NoSuchCORSConfiguration", "The CORS configuration does not exist.", http.StatusNotFound},
		NoSuchEncryptionConfiguration:     S3Code{"ServerSideEncryptionConfigurationNotFoundError", "The server side encryption configuration was not found.", http.StatusNotFound},
		NoSuchObjectLockConfiguration:     S3Code{"ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket.", http.StatusNotFound},
		PolicyTooLarge:                    S3Code{"PolicyTooLarge", "Policy exceeds the maximum allowed document size.", http.StatusBadRequest},
		MalformedPolicy:                   S3Code{"MalformedPolicy", "Policy has invalid resource.", http.StatusBadRequest},
		InvalidPolicyDocument:             S3Code{"InvalidPolicyDocument", "The content of the form does not meet the conditions specified in the policy document.", http.StatusBadRequest},
		InvalidRequest:                    S3Code{"InvalidRequest", "Invalid Request.", http.StatusBadRequest},
		XAmzContentSHA256Mismatch:         S3Code{"XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.", http.StatusBadRequest},
		IncompleteBody:                    S3Code{"IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.", http.StatusBadRequest},
		InvalidLocationConstraint:         S3Code{"InvalidLocationConstraint", "The specified location constraint is not valid.", http.StatusBadRequest},
		InvalidBucketState:                S3Code{"InvalidBucketState", "The request is not valid with the current state of the bucket.", http.StatusConflict},
		InvalidTag:                        S3Code{"InvalidTag", "The tag provided was not a valid tag.", http.StatusBadRequest},
		InvalidStorageClass:               S3Code{"InvalidStorageClass", "The storage class you specified is not valid.", http.StatusBadRequest},
		MalformedPOSTRequest:              S3Code{"MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data.", http.StatusBadRequest},
		IllegalVersioningConfiguration:    S3Code{"IllegalVersioningConfigurationException", "The versioning configuration specified in the request is invalid.", http.StatusBadRequest},
		AuthorizationHeaderMalformed:      S3Code{"AuthorizationHeaderMalformed", "The authorization header is malformed.", http.StatusBadRequest},
		AuthorizationQueryParametersError: S3Code{"AuthorizationQueryParametersError", "Error parsing the X-Amz-Credential parameter.", http.StatusBadRequest},
		InvalidToken:                      S3Code{"InvalidToken", "The provided token is malformed or otherwise invalid.", http.StatusBadRequest},
		ExpiredToken:                      S3Code{"ExpiredToken", "The provided token has expired.", http.StatusBadRequest},
		MaxMessageLengthExceeded:          S3Code{"MaxMessageLengthExceeded", "Your request was too big.", http.StatusBadRequest},
	}
}

// errorMapping maps a sentinel to its S3 code and problem+json equivalent.
type errorMapping struct {
	err     error
	code    S3Code
	status  int
	problem string
	// detail is the message of the problem document: a curated text,
	// never the chain of the error (its internal context stays in the
	// logs, with the request id).
	detail string
	// reason tells how the problem reports the cause a service phrased
	// for the caller after the sentinel ("%w: <reason>": the invalid
	// field, the missing permission, the unknown resource...).
	reason reasonMode
}

// reasonMode is how a problem reports the reason of its error.
type reasonMode int

const (
	// noReason: the curated message only (the reason is internal
	// context, or the class needs no more).
	noReason reasonMode = iota
	// reasonAlone: the reason replaces the curated message (it explains
	// the error on its own: "logger.level: verbose is not a level").
	reasonAlone
	// reasonQualified: the reason completes the curated message ("The
	// resource does not exist: healing job h1").
	reasonQualified
)

// mappings builds the sentinel table from a code table. Sentinels are
// distinct, so at most one entry matches an error.
func mappings(c S3Codes) []errorMapping {
	const reason, qualified, curated = reasonAlone, reasonQualified, noReason
	return []errorMapping{
		{domain.ErrBucketNotFound, c.NoSuchBucket, http.StatusNotFound, "bucket_not_found", "The bucket does not exist.", curated},
		{domain.ErrBucketAlreadyExists, c.BucketAlreadyExists, http.StatusConflict, "bucket_already_exists", "A bucket with this name already exists.", curated},
		{domain.ErrBucketNotEmpty, c.BucketNotEmpty, http.StatusConflict, "bucket_not_empty", "The bucket is not empty.", curated},
		{domain.ErrInvalidBucketName, c.InvalidBucketName, http.StatusBadRequest, "invalid_bucket_name", "The bucket name is not valid.", reason},
		{domain.ErrObjectNotFound, c.NoSuchKey, http.StatusNotFound, "object_not_found", "The object does not exist.", curated},
		{domain.ErrInvalidObjectName, c.InvalidObjectName, http.StatusBadRequest, "invalid_object_name", "The object name is not valid.", reason},
		{domain.ErrNoSuchVersion, c.NoSuchVersion, http.StatusNotFound, "version_not_found", "The version does not exist.", curated},
		{domain.ErrNoSuchUpload, c.NoSuchUpload, http.StatusNotFound, "upload_not_found", "The multipart upload does not exist.", curated},
		{domain.ErrInvalidPart, c.InvalidPart, http.StatusBadRequest, "invalid_part", "One or more parts are missing or do not match their entity tag.", reason},
		{domain.ErrInvalidPartOrder, c.InvalidPartOrder, http.StatusBadRequest, "invalid_part_order", "The parts are not in ascending order.", reason},
		{domain.ErrEntityTooSmall, c.EntityTooSmall, http.StatusBadRequest, "entity_too_small", "The upload is smaller than the minimum allowed size.", reason},
		{domain.ErrEntityTooLarge, c.EntityTooLarge, http.StatusRequestEntityTooLarge, "entity_too_large", "The upload exceeds the maximum allowed size.", reason},
		{domain.ErrPreconditionFailed, c.PreconditionFailed, http.StatusPreconditionFailed, "precondition_failed", "A precondition of the request does not hold.", reason},
		{domain.ErrNotModified, c.NotModified, http.StatusNotModified, "not_modified", "Not modified.", curated},
		{domain.ErrInvalidRange, c.InvalidRange, http.StatusRequestedRangeNotSatisfiable, "invalid_range", "The requested range is not satisfiable.", reason},
		{domain.ErrAccessDenied, c.AccessDenied, http.StatusForbidden, "access_denied", "Access denied.", reason},
		{domain.ErrUnauthenticated, c.AccessDenied, http.StatusUnauthorized, "unauthenticated", "Authentication required.", reason},
		{domain.ErrInvalidAccessKey, c.InvalidAccessKeyID, http.StatusUnauthorized, "invalid_access_key", "The access key does not exist.", curated},
		{domain.ErrSignatureMismatch, c.SignatureDoesNotMatch, http.StatusUnauthorized, "signature_mismatch", "The request signature does not match: check the secret key.", curated},
		{domain.ErrRequestExpired, c.RequestTimeTooSkewed, http.StatusUnauthorized, "request_expired", "The request has expired, or the clocks of the client and of the server differ too much.", curated},
		{domain.ErrTooManyRequests, c.SlowDown, http.StatusTooManyRequests, "too_many_requests", "Too many requests, please retry later.", reason},
		{domain.ErrUserNotFound, c.NoSuchUser, http.StatusNotFound, "user_not_found", "The user or access key does not exist.", curated},
		{domain.ErrUserAlreadyExists, c.UserAlreadyExists, http.StatusConflict, "user_already_exists", "The user or access key already exists.", curated},
		{domain.ErrPolicyNotFound, c.NoSuchPolicy, http.StatusNotFound, "policy_not_found", "The policy does not exist.", curated},
		{domain.ErrGroupNotFound, c.NoSuchGroup, http.StatusNotFound, "group_not_found", "The group does not exist.", curated},
		{domain.ErrShareNotFound, c.NoSuchKey, http.StatusNotFound, "share_not_found", "The share link does not exist.", curated},
		{domain.ErrNotFound, c.NoSuchKey, http.StatusNotFound, "not_found", "The resource does not exist.", qualified},
		{domain.ErrNotImplemented, c.NotImplemented, http.StatusNotImplemented, "not_implemented", "The operation is not implemented by this deployment.", reason},
		{domain.ErrStorageFull, c.StorageFull, http.StatusInsufficientStorage, "storage_full", "The drives have reached their minimum free space: delete some objects to proceed.", curated},
		{domain.ErrDiskNotFound, c.ServiceUnavailable, http.StatusServiceUnavailable, "disk_not_found", "A drive needed by the request is offline, please retry later.", curated},
		{domain.ErrNodeOffline, c.ServiceUnavailable, http.StatusServiceUnavailable, "node_offline", "A node needed by the request is offline, please retry later.", curated},
		{domain.ErrServiceUnavailable, c.ServiceUnavailable, http.StatusServiceUnavailable, "service_unavailable", "An external service needed by the request is unavailable, please retry later.", reason},
		{domain.ErrFaultyDisk, c.ServiceUnavailable, http.StatusServiceUnavailable, "faulty_disk", "A drive needed by the request is faulty, please retry later.", curated},
		{domain.ErrReadQuorum, c.SlowDownRead, http.StatusServiceUnavailable, "read_quorum", c.SlowDownRead.Description, curated},
		{domain.ErrWriteQuorum, c.SlowDownWrite, http.StatusServiceUnavailable, "write_quorum", c.SlowDownWrite.Description, curated},
		{domain.ErrLockTimeout, c.LockTimeout, http.StatusServiceUnavailable, "lock_timeout", c.LockTimeout.Description, curated},
		{domain.ErrQuotaExceeded, c.QuotaExceeded, http.StatusInsufficientStorage, "quota_exceeded", "The bucket quota is exceeded.", reason},
		{domain.ErrObjectLocked, c.ObjectLocked, http.StatusForbidden, "object_locked", "The object is protected by Object Lock.", reason},
		{domain.ErrInvalidFormat, c.InvalidFormat, http.StatusInternalServerError, "invalid_format", "The on-disk format is invalid or inconsistent.", curated},
		{domain.ErrFormatMismatch, c.InvalidFormat, http.StatusInternalServerError, "format_mismatch", "The on-disk format is invalid or inconsistent.", curated},
		{domain.ErrInvalidLayout, c.InvalidFormat, http.StatusInternalServerError, "invalid_layout", "The deployment layout is invalid.", curated},
		{domain.ErrInvalidArgument, c.InvalidArgument, http.StatusBadRequest, "invalid_argument", "Invalid argument.", reason},
		{domain.ErrMalformedXML, c.MalformedXML, http.StatusBadRequest, "malformed_xml", "The XML document is not well-formed.", reason},
		{domain.ErrConflict, c.Conflict, http.StatusConflict, "conflict", "The request conflicts with the current state of the resource.", reason},
		{domain.ErrMethodNotAllowed, c.MethodNotAllowed, http.StatusMethodNotAllowed, "method_not_allowed", "The method is not allowed on this resource.", reason},
		{domain.ErrBadDigest, c.BadDigest, http.StatusBadRequest, "bad_digest", "The checksum does not match the content.", reason},
		{domain.ErrChecksumMismatch, c.BadDigest, http.StatusBadRequest, "checksum_mismatch", "The checksum does not match the content.", reason},
		{context.DeadlineExceeded, c.RequestTimeout, http.StatusRequestTimeout, "request_timeout", "The request timed out.", curated},
	}
}

// Codes returns a copy of the code table of the codec.
func (c *codec) Codes() S3Codes { return c.codes }

// MapS3Error implements Encoder. The sentinel wrapped by err selects the
// code; a *domain.S3Error with an explicit Code overrides it: a code of the
// table brings its canonical HTTP status (MissingContentLength is 411
// whatever the sentinel), an unknown code keeps the status of the sentinel
// (400 without one). The S3Error Message, when set, replaces the default
// description. Unknown errors map to InternalError and never leak their
// text.
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
	if !errors.As(err, &s3err) {
		return code, code.Description
	}
	if s3err.Code != "" && s3err.Code != code.Code {
		if known, ok := c.byCode[s3err.Code]; ok {
			code = known
		} else {
			code.Code = s3err.Code
			if !matched {
				code.HTTPStatus = http.StatusBadRequest
				code.Description = c.codes.InvalidRequest.Description
			}
		}
	}
	if s3err.Message != "" {
		return code, s3err.Message
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

// ProblemFromError implements Encoder. The detail is curated (problemDetail):
// the chain of the error, with its internal context (packages, paths,
// drives), is never sent; unknown errors become a generic 500 without
// detail.
func (c *codec) ProblemFromError(err error) Problem {
	for _, m := range c.mappings {
		if errors.Is(err, m.err) {
			return Problem{Title: http.StatusText(m.status), Status: m.status, Code: m.problem, Detail: problemDetail(err, m)}
		}
	}
	return Problem{Title: http.StatusText(http.StatusInternalServerError), Status: http.StatusInternalServerError, Code: "internal_error"}
}

// problemDetail is the detail of the problem of err, which matches m: the
// message of a *domain.S3Error, the reason the service phrased after the
// sentinel for the classes that report it (m.reason), the curated message
// of the class otherwise. A reason that is a bare name (a quoted
// identifier, no sentence) always completes the curated message.
func problemDetail(err error, m errorMapping) string {
	var s3err *domain.S3Error
	if errors.As(err, &s3err) && s3err.Message != "" {
		return s3err.Message
	}
	if m.reason == noReason {
		return m.detail
	}
	r := reasonOf(err.Error(), m.err.Error())
	switch {
	case r == "":
		return m.detail
	case m.reason == reasonQualified || !strings.ContainsAny(r, " \t"):
		return strings.TrimSuffix(m.detail, ".") + ": " + r
	default:
		return r
	}
}

// reasonOf returns the text that follows the last "<sentinel>: " of msg,
// "" when there is none: services phrase the cause of an error for the
// caller after the sentinel they wrap ("%w: <reason>").
func reasonOf(msg, sentinel string) string {
	i := strings.LastIndex(msg, sentinel+": ")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(msg[i+len(sentinel)+2:])
}
