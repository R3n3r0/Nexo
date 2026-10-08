package policy

import (
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// S3 error codes of invalid policy documents.
const (
	codeMalformedPolicy = "MalformedPolicy"
	codePolicyTooLarge  = "PolicyTooLarge"
)

// ValidationError locates why a policy document is invalid. It unwraps to
// a *domain.S3Error (code MalformedPolicy or PolicyTooLarge) that wraps
// domain.ErrMalformedPolicy or domain.ErrPolicyTooLarge, so errors.Is and
// errors.As work on every level.
type ValidationError struct {
	// Statement is the 1-based index of the offending statement; 0 when the
	// error concerns the whole document.
	Statement int
	// Sid is the Sid of the offending statement, when it has one.
	Sid string
	// Element is the policy element at fault (e.g. "Action",
	// "Condition.StringEquals"); empty for syntax errors.
	Element string
	// Line and Column locate the error in the JSON text (1-based); zero
	// when unknown (decoded documents, semantic errors without a source).
	Line, Column int
	// Msg describes the problem.
	Msg string

	tooLarge bool
}

// Error implements error.
func (e *ValidationError) Error() string {
	var where []string
	if e.Statement > 0 {
		s := fmt.Sprintf("statement %d", e.Statement)
		if e.Sid != "" {
			s += fmt.Sprintf(" (Sid %q)", e.Sid)
		}
		where = append(where, s)
	}
	if e.Line > 0 {
		where = append(where, fmt.Sprintf("line %d, column %d", e.Line, e.Column))
	}
	msg := e.Msg
	if e.Element != "" {
		msg = e.Element + ": " + msg
	}
	if len(where) == 0 {
		return msg
	}
	return strings.Join(where, ", ") + ": " + msg
}

// Unwrap returns the S3 error carrying the code and the domain sentinel.
func (e *ValidationError) Unwrap() error {
	if e.tooLarge {
		return domain.NewS3Error(domain.ErrPolicyTooLarge, codePolicyTooLarge, e.Error())
	}
	return domain.NewS3Error(domain.ErrMalformedPolicy, codeMalformedPolicy, e.Error())
}

// malformed builds a document-level validation error.
func malformed(format string, args ...any) *ValidationError {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// stmtError builds a validation error of statement i (0-based).
func stmtError(i int, sid, element, format string, args ...any) *ValidationError {
	return &ValidationError{Statement: i + 1, Sid: sid, Element: element, Msg: fmt.Sprintf(format, args...)}
}
