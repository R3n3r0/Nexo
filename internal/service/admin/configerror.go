package admin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ConfigError reports the invalid rules of a bucket configuration
// (lifecycle, notification, CORS) written through the console: every
// problem found, rule by rule. It wraps domain.ErrInvalidArgument; the
// transport reports the problems with their rule.
type ConfigError struct {
	// Kind names the configuration: "lifecycle", "notification" or "cors".
	Kind     string
	Problems []RuleProblem
}

// Error implements error.
func (e *ConfigError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		switch {
		case p.Rule == 0:
			parts = append(parts, p.Message)
		case p.ID != "":
			parts = append(parts, fmt.Sprintf("rule %d (%s): %s", p.Rule, p.ID, p.Message))
		default:
			parts = append(parts, fmt.Sprintf("rule %d: %s", p.Rule, p.Message))
		}
	}
	return fmt.Sprintf("%s: invalid %s configuration: %s", domain.ErrInvalidArgument, e.Kind, strings.Join(parts, "; "))
}

// Unwrap returns domain.ErrInvalidArgument.
func (e *ConfigError) Unwrap() error { return domain.ErrInvalidArgument }

// RuleProblemsOf returns the problems of a *ConfigError in the chain of
// err.
func RuleProblemsOf(err error) ([]RuleProblem, bool) {
	var ce *ConfigError
	if !errors.As(err, &ce) {
		return nil, false
	}
	return ce.Problems, true
}

// xmlSchemaPrefix is the generic beginning of the S3 MalformedXML
// messages, meaningless for a configuration sent as JSON.
const xmlSchemaPrefix = "The XML you provided was not well-formed or did not validate against our published schema"

// ruleProblem describes the failure of the validation of one rule (index
// 1-based, 0 for the whole configuration): its S3 code and message, the
// generic XML wording removed.
func ruleProblem(index int, id string, err error) RuleProblem {
	p := RuleProblem{Rule: index, ID: id, Code: "InvalidArgument", Message: err.Error()}
	var s3err *domain.S3Error
	if errors.As(err, &s3err) {
		if s3err.Code != "" {
			p.Code = s3err.Code
		}
		if s3err.Message != "" {
			p.Message = s3err.Message
		}
	}
	if rest, ok := strings.CutPrefix(p.Message, xmlSchemaPrefix); ok {
		if rest = strings.TrimLeft(rest, ":. "); rest != "" {
			p.Message = rest
		}
	}
	return p
}

// duplicateIDs reports the rules whose ID an earlier rule has (ids[i] is
// the ID of rule i, "" when it has none).
func duplicateIDs(ids []string, what string) []RuleProblem {
	var out []RuleProblem
	seen := map[string]int{}
	for i, id := range ids {
		if id == "" {
			continue
		}
		if first, dup := seen[id]; dup {
			out = append(out, RuleProblem{Rule: i + 1, ID: id, Code: "InvalidArgument",
				Message: fmt.Sprintf("the %s ID %q is already used by rule %d", what, id, first)})
			continue
		}
		seen[id] = i + 1
	}
	return out
}
