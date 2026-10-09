package accessrule

import (
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinels of the access rules; each wraps a domain sentinel.
var (
	// ErrInvalidRule is a rule (or a document of rules) that does not
	// validate.
	ErrInvalidRule = fmt.Errorf("invalid access rule: %w", domain.ErrInvalidArgument)
	// ErrTooManyRules is a bucket with more rules than allowed.
	ErrTooManyRules = fmt.Errorf("too many access rules: %w", domain.ErrInvalidArgument)
	// ErrPublicAccessBlocked is an allow rule for everyone refused by the
	// block public access (reject mode).
	ErrPublicAccessBlocked = fmt.Errorf("public access is blocked: %w", domain.ErrAccessDenied)
	// ErrConfirmationRequired is a public-write preset applied without
	// confirmation.
	ErrConfirmationRequired = fmt.Errorf("explicit confirmation required: %w", domain.ErrInvalidArgument)
	// ErrSecretNotFound is a secret the store does not know.
	ErrSecretNotFound = fmt.Errorf("secret not found: %w", domain.ErrNotFound)
)

// Error implements error.
func (e *RuleError) Error() string {
	var where []string
	if e.Index >= 0 {
		s := fmt.Sprintf("rule %d", e.Index+1)
		if e.ID != "" {
			s += fmt.Sprintf(" (%q)", e.ID)
		}
		where = append(where, s)
	}
	if e.Field != "" {
		where = append(where, e.Field)
	}
	if e.Position > 0 {
		where = append(where, fmt.Sprintf("position %d", e.Position))
	}
	if len(where) == 0 {
		return e.Msg
	}
	return strings.Join(where, ", ") + ": " + e.Msg
}

// Unwrap returns the sentinel of the problem.
func (e *RuleError) Unwrap() error {
	if e.err == nil {
		return ErrInvalidRule
	}
	return e.err
}

// ruleError builds the validation error of rule i (0-based, -1 for the
// whole list).
func ruleError(i int, id, field, format string, args ...any) *RuleError {
	return &RuleError{Index: i, ID: id, Field: field, Msg: fmt.Sprintf(format, args...)}
}
