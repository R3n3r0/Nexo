package iam

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Limits of the IAM names and credentials.
const (
	minAccessKeyLen   = 3
	maxAccessKeyLen   = 20
	minSecretKeyLen   = 8
	maxSecretKeyLen   = 40
	maxGroupNameLen   = 128
	maxPolicyNameLen  = 128
	maxKeyNameLen     = 64
	maxDescriptionLen = 256
	minSessionNameLen = 2
	maxSessionNameLen = 64
)

// validateAccessKey checks an access key: 3 to 20 letters, digits or
// "-_.@+".
func validateAccessKey(ak string) error {
	if len(ak) < minAccessKeyLen || len(ak) > maxAccessKeyLen {
		return invalidArg("access key must be %d to %d characters", minAccessKeyLen, maxAccessKeyLen)
	}
	for i := 0; i < len(ak); i++ {
		c := ak[i]
		if !isAlnumASCII(c) && !strings.ContainsRune("-_.@+", rune(c)) {
			return invalidArg("access key %q contains the invalid character %q", ak, c)
		}
	}
	return nil
}

// validateSecretKey checks a secret key: 8 to 40 printable characters.
func validateSecretKey(sk string) error {
	n := utf8.RuneCountInString(sk)
	if !utf8.ValidString(sk) || n < minSecretKeyLen || n > maxSecretKeyLen {
		return invalidArg("secret key must be %d to %d characters", minSecretKeyLen, maxSecretKeyLen)
	}
	if strings.IndexFunc(sk, unicode.IsControl) >= 0 {
		return invalidArg("secret key contains control characters")
	}
	return nil
}

// validateName checks a group or policy name: 1 to max printable
// characters, no comma (the separator of name lists).
func validateName(kind, name string, maxLen int) error {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxLen {
		return invalidArg("%s name must be 1 to %d characters", kind, maxLen)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 || strings.ContainsRune(name, ',') {
		return invalidArg("%s name %q contains invalid characters", kind, name)
	}
	if strings.TrimSpace(name) != name {
		return invalidArg("%s name %q has leading or trailing spaces", kind, name)
	}
	return nil
}

// normalizePolicyNames validates policy names and returns them sorted and
// deduplicated; nil when empty.
func normalizePolicyNames(names []string) ([]string, error) {
	for _, n := range names {
		if err := validateName("policy", n, maxPolicyNameLen); err != nil {
			return nil, err
		}
	}
	return sortedUnique(names), nil
}

// parseStatus validates a status; empty means def.
func parseStatus(s domain.CredentialStatus, def domain.CredentialStatus) (domain.CredentialStatus, error) {
	if s == "" {
		if def == "" {
			return "", invalidArg("status is required")
		}
		return def, nil
	}
	st, err := domain.ParseCredentialStatus(string(s))
	if err != nil {
		return "", fmt.Errorf("iam: %w", err)
	}
	return st, nil
}

// validateInlinePolicy performs the structural checks of an inline policy;
// the policy engine validates documents in depth before they reach IAM.
func validateInlinePolicy(p *domain.Policy) error {
	if p == nil {
		return nil
	}
	if p.Version != domain.PolicyVersion {
		return fmt.Errorf("%w: iam: inline policy Version must be %s", domain.ErrMalformedPolicy, domain.PolicyVersion)
	}
	if len(p.Statement) == 0 {
		return fmt.Errorf("%w: iam: inline policy has no statement", domain.ErrMalformedPolicy)
	}
	for i, st := range p.Statement {
		if !st.Effect.IsValid() {
			return fmt.Errorf("%w: iam: inline policy statement %d: Effect must be Allow or Deny", domain.ErrMalformedPolicy, i+1)
		}
	}
	return nil
}

// validateSessionName checks an STS role session name.
func validateSessionName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) < minSessionNameLen || len(name) > maxSessionNameLen {
		return invalidArg("role session name must be %d to %d characters", minSessionNameLen, maxSessionNameLen)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !isAlnumASCII(c) && !strings.ContainsRune("_+=,.@-", rune(c)) {
			return invalidArg("role session name contains the invalid character %q", c)
		}
	}
	return nil
}

func isAlnumASCII(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// sortedUnique returns the sorted distinct values; nil when empty.
func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// union returns the sorted distinct values of a and b.
func union(a, b []string) []string {
	return sortedUnique(append(slices.Clone(a), b...))
}

// difference returns the values of a not in b, sorted; nil when empty.
func difference(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return sortedUnique(out)
}
