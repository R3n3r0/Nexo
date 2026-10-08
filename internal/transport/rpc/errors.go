package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Sentinel errors of the internode transport. Each wraps a domain sentinel
// (docs/12 §12.2 rule 5) and crosses the wire with its own code.
var (
	// ErrInvalidToken reports a missing, malformed, expired or forged
	// internode token.
	ErrInvalidToken = fmt.Errorf("%w: rpc: invalid internode token", domain.ErrUnauthenticated)
	// ErrDeploymentMismatch reports a caller of another deployment.
	ErrDeploymentMismatch = fmt.Errorf("%w: rpc: deployment id mismatch", domain.ErrAccessDenied)
	// ErrIncompatibleVersion reports a caller running an incompatible
	// protocol or major version.
	ErrIncompatibleVersion = fmt.Errorf("%w: rpc: incompatible node version", domain.ErrConflict)
	// ErrProtocol reports a malformed RPC exchange (bad frame, checksum
	// mismatch, unexpected content): the copy of the data is unusable, as
	// from a faulty drive.
	ErrProtocol = fmt.Errorf("%w: rpc: protocol error", domain.ErrFaultyDisk)
)

// errorCode binds a wire code to a sentinel and the HTTP status of the
// error answer.
type errorCode struct {
	code   string
	err    error
	status int
}

// errorCodes lists the sentinels that cross the wire, the most specific
// first: an error is encoded with the first sentinel of the list it wraps,
// so that a sentinel wrapping another one (storage.ErrFileCorrupt wraps
// domain.ErrFaultyDisk) keeps its identity. The codes are part of the
// protocol: never rename one.
func errorCodes() []errorCode {
	const (
		bad      = http.StatusBadRequest
		notFound = http.StatusNotFound
		internal = http.StatusInternalServerError
	)
	return []errorCode{
		{"canceled", context.Canceled, 499},
		{"deadline_exceeded", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"unexpected_eof", io.ErrUnexpectedEOF, bad},
		{"invalid_token", ErrInvalidToken, http.StatusUnauthorized},
		{"deployment_mismatch", ErrDeploymentMismatch, http.StatusForbidden},
		{"incompatible_version", ErrIncompatibleVersion, http.StatusConflict},
		{"protocol_error", ErrProtocol, bad},
		{"file_corrupt", storage.ErrFileCorrupt, internal},
		{"credentials_disabled", domain.ErrCredentialsDisabled, http.StatusForbidden},
		{"malformed_policy", domain.ErrMalformedPolicy, bad},
		{"policy_too_large", domain.ErrPolicyTooLarge, bad},
		{"group_already_exists", domain.ErrGroupAlreadyExists, http.StatusConflict},
		{"access_key_not_found", domain.ErrAccessKeyNotFound, notFound},
		{"access_key_already_exists", domain.ErrAccessKeyAlreadyExists, http.StatusConflict},
		{"bad_digest", domain.ErrBadDigest, bad},
		{"bucket_not_found", domain.ErrBucketNotFound, notFound},
		{"bucket_already_exists", domain.ErrBucketAlreadyExists, http.StatusConflict},
		{"bucket_not_empty", domain.ErrBucketNotEmpty, http.StatusConflict},
		{"invalid_bucket_name", domain.ErrInvalidBucketName, bad},
		{"object_not_found", domain.ErrObjectNotFound, notFound},
		{"invalid_object_name", domain.ErrInvalidObjectName, bad},
		{"no_such_version", domain.ErrNoSuchVersion, notFound},
		{"no_such_upload", domain.ErrNoSuchUpload, notFound},
		{"invalid_part", domain.ErrInvalidPart, bad},
		{"invalid_part_order", domain.ErrInvalidPartOrder, bad},
		{"entity_too_small", domain.ErrEntityTooSmall, bad},
		{"entity_too_large", domain.ErrEntityTooLarge, bad},
		{"precondition_failed", domain.ErrPreconditionFailed, http.StatusPreconditionFailed},
		{"not_modified", domain.ErrNotModified, http.StatusNotModified},
		{"invalid_range", domain.ErrInvalidRange, http.StatusRequestedRangeNotSatisfiable},
		{"access_denied", domain.ErrAccessDenied, http.StatusForbidden},
		{"invalid_access_key", domain.ErrInvalidAccessKey, http.StatusForbidden},
		{"signature_mismatch", domain.ErrSignatureMismatch, http.StatusForbidden},
		{"request_expired", domain.ErrRequestExpired, http.StatusForbidden},
		{"too_many_requests", domain.ErrTooManyRequests, http.StatusTooManyRequests},
		{"unauthenticated", domain.ErrUnauthenticated, http.StatusUnauthorized},
		{"user_not_found", domain.ErrUserNotFound, notFound},
		{"user_already_exists", domain.ErrUserAlreadyExists, http.StatusConflict},
		{"policy_not_found", domain.ErrPolicyNotFound, notFound},
		{"group_not_found", domain.ErrGroupNotFound, notFound},
		{"share_not_found", domain.ErrShareNotFound, notFound},
		{"not_found", domain.ErrNotFound, notFound},
		{"not_implemented", domain.ErrNotImplemented, http.StatusNotImplemented},
		{"storage_full", domain.ErrStorageFull, http.StatusInsufficientStorage},
		{"disk_not_found", domain.ErrDiskNotFound, http.StatusServiceUnavailable},
		{"faulty_disk", domain.ErrFaultyDisk, internal},
		{"read_quorum", domain.ErrReadQuorum, http.StatusServiceUnavailable},
		{"write_quorum", domain.ErrWriteQuorum, http.StatusServiceUnavailable},
		{"lock_timeout", domain.ErrLockTimeout, http.StatusServiceUnavailable},
		{"node_offline", domain.ErrNodeOffline, http.StatusServiceUnavailable},
		{"quota_exceeded", domain.ErrQuotaExceeded, bad},
		{"object_locked", domain.ErrObjectLocked, http.StatusForbidden},
		{"invalid_format", domain.ErrInvalidFormat, internal},
		{"format_mismatch", domain.ErrFormatMismatch, internal},
		{"invalid_layout", domain.ErrInvalidLayout, internal},
		{"invalid_argument", domain.ErrInvalidArgument, bad},
		{"malformed_xml", domain.ErrMalformedXML, bad},
		{"conflict", domain.ErrConflict, http.StatusConflict},
		{"method_not_allowed", domain.ErrMethodNotAllowed, http.StatusMethodNotAllowed},
		{"checksum_mismatch", domain.ErrChecksumMismatch, bad},
	}
}

// codeInternal is the code of an error that wraps no known sentinel.
const codeInternal = "internal"

// wireError is the encoded form of an error: the codes of the sentinels it
// wraps (the outermost first), its text for the logs, and the S3
// decoration of a domain.S3Error.
type wireError struct {
	Code      string   `msgpack:"c"`
	Also      []string `msgpack:"a,omitempty"`
	Message   string   `msgpack:"m,omitempty"`
	S3Code    string   `msgpack:"s3c,omitempty"`
	S3Message string   `msgpack:"s3m,omitempty"`
	Resource  string   `msgpack:"r,omitempty"`
}

// encodeError turns err into its wire form and the HTTP status of the
// answer. Every sentinel of the error tree is kept, so that errors.Is
// answers on the caller's side what it answers here (a write refused as
// domain.ErrInvalidObjectName because of a domain.ErrObjectNotFound below
// it matches both).
func encodeError(err error) (wireError, int) {
	codes := errorCodes()
	we := wireError{Code: codeInternal, Message: err.Error()}
	status := http.StatusInternalServerError
	var found []int
	seen := map[int]bool{}
	var walk func(e error)
	walk = func(e error) {
		if e == nil {
			return
		}
		for i, c := range codes {
			if e == c.err && !seen[i] {
				seen[i] = true
				found = append(found, i)
				break
			}
		}
		switch x := e.(type) {
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range x.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	if len(found) == 0 {
		// Errors matching a sentinel through an Is method of their own.
		for i, c := range codes {
			if errors.Is(err, c.err) {
				found = append(found, i)
				break
			}
		}
	}
	for n, i := range found {
		if n == 0 {
			we.Code, status = codes[i].code, codes[i].status
			continue
		}
		we.Also = append(we.Also, codes[i].code)
	}
	var s3err *domain.S3Error
	if errors.As(err, &s3err) {
		we.S3Code, we.S3Message, we.Resource = s3err.Code, s3err.Message, s3err.Resource
	}
	return we, status
}

// remoteError is an error answered by a node: its text is the remote one,
// it unwraps to the sentinels of its codes.
type remoteError struct {
	node    string
	message string
	errs    []error
}

func (e *remoteError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("node %s: %v", e.node, errors.Join(e.errs...))
	}
	return fmt.Sprintf("node %s: %s", e.node, e.message)
}

func (e *remoteError) Unwrap() []error { return e.errs }

// decodeError rebuilds the error answered by node.
func decodeError(node string, we wireError) error {
	codes := errorCodes()
	var sentinels []error
	for _, code := range append([]string{we.Code}, we.Also...) {
		for _, c := range codes {
			if c.code == code {
				sentinels = append(sentinels, c.err)
				break
			}
		}
	}
	err := error(&remoteError{node: node, message: we.Message, errs: sentinels})
	if len(sentinels) == 0 {
		err = fmt.Errorf("node %s: %s (code %s)", node, we.Message, we.Code)
	}
	if we.S3Code != "" || we.S3Message != "" || we.Resource != "" {
		return &domain.S3Error{Code: we.S3Code, Message: we.S3Message, Resource: we.Resource, Err: err}
	}
	return err
}
