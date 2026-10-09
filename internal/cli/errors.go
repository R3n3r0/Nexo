package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ErrUsage reports a command line that cannot be executed as written
// (missing or extra arguments, invalid flag values, malformed targets). It
// wraps domain.ErrInvalidArgument.
var ErrUsage = fmt.Errorf("usage error: %w", domain.ErrInvalidArgument)

// Exit codes of nexoctl (docs/06 TCL-U-04): they are part of the CLI
// contract and never change.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitUsage       = 2
	ExitNotFound    = 3
	ExitAccess      = 4
	ExitInterrupted = 130
)

// UsageError returns an error wrapping ErrUsage whose message is the
// formatted text only.
func UsageError(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// usageError is the error of UsageError.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func (e *usageError) Unwrap() error { return ErrUsage }

// ExitCode maps an error to the exit code of the process: 0 for nil, 2 for
// usage errors, 3 for missing buckets, objects, versions, aliases or local
// files, 4 for authentication and authorization failures (an object
// version protected by Object Lock included), 130 for an interruption
// (Ctrl-C) and 1 for everything else.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, ErrUsage):
		return ExitUsage
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	case isAccessError(err):
		return ExitAccess
	case isNotFound(err):
		return ExitNotFound
	default:
		return ExitError
	}
}

// ErrorCode returns a stable, machine-readable name of the class of err,
// used by the JSON error output.
func ErrorCode(err error) string {
	switch ExitCode(err) {
	case ExitOK:
		return ""
	case ExitUsage:
		return "UsageError"
	case ExitInterrupted:
		return "Interrupted"
	case ExitAccess:
		return "AccessDenied"
	case ExitNotFound:
		return "NotFound"
	default:
		return "Error"
	}
}

// IsAuthorizationDenial reports a request that the server authenticated
// and then refused by policy (AccessDenied): the credentials are valid.
// Unknown access keys, wrong secrets and expired requests are not.
func IsAuthorizationDenial(err error) bool {
	if !errors.Is(err, domain.ErrAccessDenied) {
		return false
	}
	for _, target := range []error{domain.ErrInvalidAccessKey, domain.ErrSignatureMismatch, domain.ErrRequestExpired, domain.ErrUnauthenticated} {
		if errors.Is(err, target) {
			return false
		}
	}
	return true
}

func isAccessError(err error) bool {
	for _, target := range []error{
		domain.ErrAccessDenied, domain.ErrInvalidAccessKey, domain.ErrSignatureMismatch,
		domain.ErrRequestExpired, domain.ErrUnauthenticated, domain.ErrObjectLocked, fs.ErrPermission,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func isNotFound(err error) bool {
	for _, target := range []error{
		domain.ErrBucketNotFound, domain.ErrObjectNotFound, domain.ErrNoSuchVersion,
		domain.ErrNoSuchUpload, domain.ErrPolicyNotFound, domain.ErrUserNotFound, domain.ErrGroupNotFound,
		domain.ErrShareNotFound, domain.ErrNotFound, fs.ErrNotExist,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// AbortedUploadError reports a multipart upload that failed (or was
// interrupted) and has been aborted: no part is left on the server unless
// AbortErr is set.
type AbortedUploadError struct {
	Bucket   string
	Key      string
	UploadID string
	// Err is the failure that caused the abort.
	Err error
	// AbortErr is the failure of the abort itself, if any.
	AbortErr error
}

// Error implements error.
func (e *AbortedUploadError) Error() string {
	if e.AbortErr != nil {
		return fmt.Sprintf("%s/%s: multipart upload %s failed (%v) and could not be aborted: %v", e.Bucket, e.Key, e.UploadID, e.Err, e.AbortErr)
	}
	return fmt.Sprintf("%s/%s: multipart upload %s aborted: %v", e.Bucket, e.Key, e.UploadID, e.Err)
}

// Unwrap returns the cause of the abort.
func (e *AbortedUploadError) Unwrap() error { return e.Err }
