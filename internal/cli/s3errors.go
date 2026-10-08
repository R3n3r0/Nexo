package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
	}
	return nil
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
		}
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
