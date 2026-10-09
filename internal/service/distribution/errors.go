package distribution

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinels of the package; they wrap the domain ones, so that errors.Is
// on domain.ErrNotFound, domain.ErrConflict, domain.ErrInvalidArgument and
// domain.ErrAccessDenied keeps working.
var (
	// ErrDistributionNotFound reports an unknown distribution.
	ErrDistributionNotFound = fmt.Errorf("distribution not found: %w", domain.ErrNotFound)
	// ErrKeyGroupNotFound reports an unknown key group.
	ErrKeyGroupNotFound = fmt.Errorf("key group not found: %w", domain.ErrNotFound)
	// ErrKeyNotFound reports an unknown key of a key group.
	ErrKeyNotFound = fmt.Errorf("key not found: %w", domain.ErrNotFound)
	// ErrAliasConflict reports an alias used by another distribution.
	ErrAliasConflict = fmt.Errorf("alias already used by another distribution: %w", domain.ErrConflict)
	// ErrAlreadyExists reports an id already taken.
	ErrAlreadyExists = fmt.Errorf("already exists: %w", domain.ErrConflict)
	// ErrInvalid reports an invalid distribution, key group or key.
	ErrInvalid = fmt.Errorf("invalid distribution configuration: %w", domain.ErrInvalidArgument)
	// ErrBadSignature reports a signed URL or cookie that does not verify.
	ErrBadSignature = fmt.Errorf("invalid signature: %w", domain.ErrAccessDenied)
	// ErrSignatureExpired reports an expired or not yet valid signature.
	ErrSignatureExpired = fmt.Errorf("signature expired or not yet valid: %w", domain.ErrAccessDenied)
	// ErrMissingSignature reports a request to a signed behavior without
	// signature.
	ErrMissingSignature = fmt.Errorf("missing signature: %w", domain.ErrAccessDenied)
)
