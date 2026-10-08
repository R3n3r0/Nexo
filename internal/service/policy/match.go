package policy

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// matchWildcard reports whether s matches pattern, where "*" matches any
// sequence and "?" any single character.
func matchWildcard(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	// Iterative glob matching with backtracking on the last star.
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// matchWildcardFold is matchWildcard with case-insensitive comparison.
func matchWildcardFold(pattern, s string) bool {
	return matchWildcard(strings.ToLower(pattern), strings.ToLower(s))
}

// substitute expands ${key} policy variables from the condition context
// ("${*}", "${?}" and "${$}" are the escaped literals).
func substitute(s string, ctx map[string][]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	var sb strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			sb.WriteString(s)
			return sb.String()
		}
		sb.WriteString(s[:start])
		key := s[start+2 : start+end]
		switch key {
		case "*", "?", "$":
			sb.WriteString(key)
		default:
			if vals, ok := lookupKey(ctx, key); ok && len(vals) > 0 {
				sb.WriteString(vals[0])
			} else {
				sb.WriteString(s[start : start+end+1])
			}
		}
		s = s[start+end+1:]
	}
}

// lookupKey finds a condition key case-insensitively.
func lookupKey(ctx map[string][]string, key string) ([]string, bool) {
	if v, ok := ctx[key]; ok {
		return v, true
	}
	lower := strings.ToLower(key)
	for k, v := range ctx {
		if strings.ToLower(k) == lower {
			return v, true
		}
	}
	return nil, false
}

// conditionOperator is a parsed operator name.
type conditionOperator struct {
	base     string // e.g. "StringEquals"
	negated  bool
	ifExists bool
	forAll   bool
	forAny   bool
}

// knownOperators lists the supported base operators.
var knownOperators = map[string]bool{
	"StringEquals": true, "StringNotEquals": true, "StringEqualsIgnoreCase": true, "StringNotEqualsIgnoreCase": true,
	"StringLike": true, "StringNotLike": true,
	"NumericEquals": true, "NumericNotEquals": true, "NumericLessThan": true, "NumericLessThanEquals": true,
	"NumericGreaterThan": true, "NumericGreaterThanEquals": true,
	"DateEquals": true, "DateNotEquals": true, "DateLessThan": true, "DateLessThanEquals": true,
	"DateGreaterThan": true, "DateGreaterThanEquals": true,
	"Bool": true, "IpAddress": true, "NotIpAddress": true, "Null": true,
}

// parseOperator splits set qualifiers and the IfExists suffix off an operator.
func parseOperator(op string) (conditionOperator, bool) {
	var c conditionOperator
	switch {
	case strings.HasPrefix(op, "ForAllValues:"):
		c.forAll = true
		op = strings.TrimPrefix(op, "ForAllValues:")
	case strings.HasPrefix(op, "ForAnyValue:"):
		c.forAny = true
		op = strings.TrimPrefix(op, "ForAnyValue:")
	}
	if strings.HasSuffix(op, "IfExists") && op != "Null" {
		c.ifExists = true
		op = strings.TrimSuffix(op, "IfExists")
	}
	if !knownOperators[op] {
		return c, false
	}
	c.base = op
	c.negated = strings.Contains(op, "Not")
	return c, true
}

// evalCondition evaluates one condition block (operator → key → values).
func evalCondition(cond domain.Condition, ctx map[string][]string) bool {
	for op, keys := range cond {
		parsed, ok := parseOperator(op)
		if !ok {
			return false
		}
		for key, values := range keys {
			actual, present := lookupKey(ctx, key)
			if parsed.base == "Null" {
				wantNull := len(values) > 0 && strings.EqualFold(values[0], "true")
				if (!present || len(actual) == 0) != wantNull {
					return false
				}
				continue
			}
			if !present || len(actual) == 0 {
				if parsed.ifExists || parsed.negated || parsed.forAll {
					continue
				}
				return false
			}
			if !evalValues(parsed, actual, values, ctx) {
				return false
			}
		}
	}
	return true
}

// evalValues applies the operator to the request values and the policy values.
func evalValues(op conditionOperator, actual []string, policyValues []string, ctx map[string][]string) bool {
	expanded := make([]string, 0, len(policyValues))
	for _, v := range policyValues {
		expanded = append(expanded, substitute(v, ctx))
	}
	matchOne := func(a string) bool {
		matched := false
		for _, p := range expanded {
			if compare(op.base, a, p) {
				matched = true
				break
			}
		}
		if op.negated {
			return !matched
		}
		return matched
	}
	switch {
	case op.forAll:
		for _, a := range actual {
			if !matchOne(a) {
				return false
			}
		}
		return true
	case op.forAny:
		for _, a := range actual {
			if matchOne(a) {
				return true
			}
		}
		return false
	default:
		if op.negated {
			// Every request value must avoid every policy value.
			for _, a := range actual {
				if !matchOne(a) {
					return false
				}
			}
			return true
		}
		for _, a := range actual {
			if matchOne(a) {
				return true
			}
		}
		return false
	}
}

// compare evaluates a single (request value, policy value) pair for the base
// operator, ignoring negation (handled by the caller).
func compare(base, actual, policy string) bool {
	switch base {
	case "StringEquals", "StringNotEquals":
		return actual == policy
	case "StringEqualsIgnoreCase", "StringNotEqualsIgnoreCase":
		return strings.EqualFold(actual, policy)
	case "StringLike", "StringNotLike":
		return matchWildcard(policy, actual)
	case "Bool":
		return strings.EqualFold(actual, policy)
	case "IpAddress", "NotIpAddress":
		return ipMatches(policy, actual)
	case "NumericEquals", "NumericNotEquals", "NumericLessThan", "NumericLessThanEquals", "NumericGreaterThan", "NumericGreaterThanEquals":
		a, err1 := strconv.ParseFloat(actual, 64)
		p, err2 := strconv.ParseFloat(policy, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		return ordered(strings.TrimPrefix(base, "Numeric"), a, p)
	case "DateEquals", "DateNotEquals", "DateLessThan", "DateLessThanEquals", "DateGreaterThan", "DateGreaterThanEquals":
		a, err1 := parseDate(actual)
		p, err2 := parseDate(policy)
		if err1 != nil || err2 != nil {
			return false
		}
		return ordered(strings.TrimPrefix(base, "Date"), float64(a.UnixNano()), float64(p.UnixNano()))
	}
	return false
}

func ordered(rel string, a, p float64) bool {
	switch rel {
	case "Equals", "NotEquals":
		return a == p
	case "LessThan":
		return a < p
	case "LessThanEquals":
		return a <= p
	case "GreaterThan":
		return a > p
	case "GreaterThanEquals":
		return a >= p
	}
	return false
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(epoch, 0), nil
	}
	return time.Parse(time.RFC3339, s)
}

// ipMatches reports whether addr is within the policy CIDR (or equals the IP).
func ipMatches(policy, addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			ip = net.ParseIP(host)
		}
	}
	if ip == nil {
		return false
	}
	if strings.Contains(policy, "/") {
		_, network, err := net.ParseCIDR(policy)
		return err == nil && network.Contains(ip)
	}
	p := net.ParseIP(policy)
	return p != nil && p.Equal(ip)
}
