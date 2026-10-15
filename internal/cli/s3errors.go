package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/r3n3r0/nexo/internal/domain"
)

// s3CodeSentinel maps the S3 error codes to the sentinels of domain.
func s3CodeSentinel(code string) error {
	switch code {
	case "NoSuchBucket":
		return domain.ErrBucketNotFound
	case "NoSuchKey":
		return domain.ErrObjectNotFound
	case "NoSuchVersion":
		return domain.ErrNoSuchVersion
	case "NoSuchUpload":
		return domain.ErrNoSuchUpload
	case "NoSuchBucketPolicy":
		return domain.ErrPolicyNotFound
	case "AccessDenied", "AllAccessDisabled", "Forbidden", "AccountProblem":
		return domain.ErrAccessDenied
	case "InvalidAccessKeyId", "InvalidToken", "InvalidClientTokenId":
		return domain.ErrInvalidAccessKey
	case "SignatureDoesNotMatch":
		return domain.ErrSignatureMismatch
	case "ExpiredToken", "RequestExpired", "RequestTimeTooSkewed":
		return domain.ErrRequestExpired
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
		return domain.ErrBucketAlreadyExists
	case "BucketNotEmpty":
		return domain.ErrBucketNotEmpty
	case "InvalidBucketName":
		return domain.ErrInvalidBucketName
	case "KeyTooLongError", "InvalidObjectName":
		return domain.ErrInvalidObjectName
	case "EntityTooLarge":
		return domain.ErrEntityTooLarge
	case "EntityTooSmall":
		return domain.ErrEntityTooSmall
	case "InvalidPart":
		return domain.ErrInvalidPart
	case "InvalidPartOrder":
		return domain.ErrInvalidPartOrder
	case "InvalidRange":
		return domain.ErrInvalidRange
	case "PreconditionFailed":
		return domain.ErrPreconditionFailed
	case "MalformedPolicy":
		return domain.ErrMalformedPolicy
	case "MalformedXML":
		return domain.ErrMalformedXML
	case "InvalidArgument", "InvalidRequest", "AuthorizationQueryParametersError":
		return domain.ErrInvalidArgument
	case "NotImplemented":
		return domain.ErrNotImplemented
	case "MethodNotAllowed":
		return domain.ErrMethodNotAllowed
	case "XNexoQuotaExceeded", "QuotaExceeded":
		return domain.ErrQuotaExceeded
	case "XNexoStorageFull":
		return domain.ErrStorageFull
	case "XNexoObjectLocked", "ObjectLocked":
		return domain.ErrObjectLocked
	case "NoSuchLifecycleConfiguration", "NoSuchCORSConfiguration", "NoSuchTagSet",
		"ServerSideEncryptionConfigurationNotFoundError", "ObjectLockConfigurationNotFoundError", "NoSuchObjectLockConfiguration":
		return domain.ErrNotFound
	case "InvalidBucketState":
		return domain.ErrConflict
	case "BadDigest", "InvalidDigest", "XAmzContentSHA256Mismatch":
		return domain.ErrBadDigest
	case "KMS.NotFoundException":
		return domain.ErrKMSKeyNotFound
	case codeSlowDownWrite:
		return domain.ErrWriteQuorum
	case codeSlowDownRead:
		return domain.ErrReadQuorum
	case codeLockTimeout:
		return domain.ErrLockTimeout
	}
	return nil
}

// The S3 codes of a Nexo deployment without quorum (503): too many drives
// or nodes offline to read, to write or to lock.
const (
	codeSlowDownWrite = "SlowDownWrite"
	codeSlowDownRead  = "SlowDownRead"
	codeLockTimeout   = "XNexoLockTimeout"
)

// quorumHint replaces the message of the errors of a deployment without
// quorum (SlowDownRead, SlowDownWrite): what to check rather than a
// request rate to reduce. lockHint replaces the message of a lock timeout,
// which is first of all a contention: the object is locked by another
// operation (a slow or stalled read or write), the quorum comes second.
const (
	quorumHint = `the deployment has no quorum (too many drives or nodes offline): check "nexoctl admin info"`
	lockHint   = `the object is locked by another operation (a slow or stalled read or write) or too many nodes are offline: retry later, or check "nexoctl admin info"`
)

// noQuorumRetry stops the retries of the SDK on the errors of a
// deployment without quorum (a retry.IsErrorRetryableFunc): the server
// already waited for its lock timeout, retrying multiplies the wait
// without changing the answer.
func noQuorumRetry(err error) aws.Ternary {
	if isCode(err, codeSlowDownWrite, codeSlowDownRead, codeLockTimeout) {
		return aws.FalseTernary
	}
	return aws.UnknownTernary
}

// UnreachableError reports a server that could not be contacted: the
// connection was refused, the host name does not resolve, the network is
// down... (exit code 1).
type UnreachableError struct {
	// Endpoint is the URL of the server (scheme and host).
	Endpoint string
	// Reason is the cause of the network error ("connection refused").
	Reason string
	// Hint, when set, says what to check ("set the Admin API URL...").
	Hint string
	// Err is the error of the SDK.
	Err error
}

// Error implements error.
func (e *UnreachableError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("cannot reach %s: %s (%s)", e.Endpoint, e.Reason, e.Hint)
	}
	return fmt.Sprintf("cannot reach %s: %s", e.Endpoint, e.Reason)
}

// Unwrap returns the error of the SDK.
func (e *UnreachableError) Unwrap() error { return e.Err }

// unreachable recognizes a network failure in an error of the SDK (a
// *net.OpError or a *net.DNSError in its chain) and reports it as an
// *UnreachableError naming the endpoint of the request.
func unreachable(err error) (*UnreachableError, bool) {
	var reason string
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		reason = "lookup " + dnsErr.Name + ": " + dnsErr.Err
		if dnsErr.IsNotFound {
			reason = "lookup " + dnsErr.Name + ": no such host"
		}
	case errors.As(err, &opErr):
		reason = opErr.Err.Error()
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			reason = sysErr.Err.Error()
		}
		if opErr.Timeout() {
			reason = "i/o timeout"
		}
	default:
		return nil, false
	}
	endpoint := ""
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if u, perr := url.Parse(urlErr.URL); perr == nil {
			endpoint = u.Scheme + "://" + u.Host
		}
	}
	if endpoint == "" && opErr != nil && opErr.Addr != nil {
		endpoint = opErr.Addr.String()
	}
	return &UnreachableError{Endpoint: endpoint, Reason: reason, Err: err}, true
}

// mapS3Error converts an error of aws-sdk-go-v2 into an error wrapping the
// matching sentinel of domain, so that the commands report stable exit
// codes. bucket and key tell which resource a bare 404 (HEAD) is about.
// An interruption is reported as context.Canceled.
func mapS3Error(err error, bucket, key string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		if u, ok := unreachable(err); ok {
			return u
		}
		return err
	}
	status := 0
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		status = respErr.HTTPStatusCode()
	}
	return codeError(apiErr.ErrorCode(), apiErr.ErrorMessage(), status, bucket, key)
}

// codeError builds the error of an S3 error code and message.
func codeError(code, message string, status int, bucket, key string) error {
	sentinel := s3CodeSentinel(code)
	if sentinel == nil {
		switch status {
		case http.StatusNotFound:
			if key == "" {
				sentinel = domain.ErrBucketNotFound
			} else {
				sentinel = domain.ErrObjectNotFound
			}
		case http.StatusForbidden:
			sentinel = domain.ErrAccessDenied
		case http.StatusUnauthorized:
			sentinel = domain.ErrUnauthenticated
		case http.StatusPreconditionFailed:
			sentinel = domain.ErrPreconditionFailed
		case http.StatusBadRequest:
			// A HEAD answers without a body: the request was refused
			// as invalid (an SSE-C object read without its key, ...).
			sentinel = domain.ErrInvalidArgument
		}
	}
	switch code {
	case codeSlowDownWrite, codeSlowDownRead:
		message = quorumHint
	case codeLockTimeout:
		message = lockHint
	}
	detail := message
	if detail == "" {
		detail = code
	} else if code != "" {
		detail = fmt.Sprintf("%s (%s)", message, code)
	}
	if sentinel == nil {
		if detail == "" {
			detail = fmt.Sprintf("HTTP status %d", status)
		}
		return errors.New(detail)
	}
	if detail == "" || code == "NotFound" || code == "Forbidden" {
		return sentinel
	}
	return fmt.Errorf("%w: %s", sentinel, detail)
}

// isCode reports whether err is an S3 error with one of the codes.
func isCode(err error, codes ...string) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	for _, c := range codes {
		if apiErr.ErrorCode() == c {
			return true
		}
	}
	return false
}
