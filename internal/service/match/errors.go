package match

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Error implements error.
func (e *PatternError) Error() string {
	where := string(e.Type) + " pattern"
	if e.Index >= 0 {
		where = fmt.Sprintf("pattern %d (%s)", e.Index+1, e.Type)
	}
	if e.Position > 0 {
		return fmt.Sprintf("%s %q: position %d: %s", where, e.Pattern, e.Position, e.Msg)
	}
	return fmt.Sprintf("%s %q: %s", where, e.Pattern, e.Msg)
}

// Unwrap returns domain.ErrInvalidArgument.
func (e *PatternError) Unwrap() error { return domain.ErrInvalidArgument }

// patternError builds the error of a single pattern.
func patternError(p Pattern, pos int, format string, args ...any) *PatternError {
	return &PatternError{Index: -1, Type: p.Type, Pattern: p.Pattern, Position: pos, Msg: fmt.Sprintf(format, args...)}
}
