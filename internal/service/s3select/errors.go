package s3select

import (
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The errors of S3 Select carry the codes of S3: the request errors are
// answered before the records, the evaluation errors as an error event.

func requestError(code, format string, args ...any) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, code, fmt.Sprintf(format, args...))
}

func parseError(format string, args ...any) error {
	return requestError("ParseUnexpectedToken", format, args...)
}

func unsupported(format string, args ...any) error {
	return requestError("UnsupportedSyntax", format, args...)
}

func evalError(code, format string, args ...any) error {
	return requestError(code, format, args...)
}

// isS3Error reports an error that already carries an S3 code.
func isS3Error(err error) bool {
	var s3err *domain.S3Error
	return errors.As(err, &s3err)
}
