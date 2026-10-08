package iam

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Package sentinels. Each wraps a domain sentinel so that errors.Is on the
// domain error keeps working (docs/12 §12.2 rule 5).
var (
	// ErrEntryNotFound is returned by Store.Get for a missing key. It wraps
	// domain.ErrObjectNotFound: store entries are persisted documents.
	ErrEntryNotFound = fmt.Errorf("iam store: entry not found: %w", domain.ErrObjectNotFound)
	// ErrWrongKey reports data encrypted with another key: the root
	// secret key changed since the file store or the export was written.
	// It wraps domain.ErrFormatMismatch.
	ErrWrongKey = fmt.Errorf("iam: decryption key does not match (were the root credentials changed?): %w", domain.ErrFormatMismatch)
	// ErrNotBootstrapped reports an operation that needs the root
	// credentials (STS, export, import) before Bootstrap. It wraps
	// domain.ErrInvalidArgument: it is a wiring error.
	ErrNotBootstrapped = fmt.Errorf("iam: root credentials not installed: %w", domain.ErrInvalidArgument)
)

// corrupted builds the error of a damaged document.
func corrupted(what string, err error) error {
	if err != nil {
		return fmt.Errorf("%w: iam: %s is corrupted: %w", domain.ErrInvalidFormat, what, err)
	}
	return fmt.Errorf("%w: iam: %s is corrupted", domain.ErrInvalidFormat, what)
}

func invalidArg(format string, args ...any) error {
	return fmt.Errorf("%w: iam: %s", domain.ErrInvalidArgument, fmt.Sprintf(format, args...))
}
