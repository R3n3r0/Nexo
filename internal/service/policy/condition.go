package policy

import (
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// conditionOperator is a parsed operator name such as
// "ForAllValues:StringLikeIfExists".
type conditionOperator struct {
	base     string // e.g. "StringLike"
	negated  bool
	ifExists bool
	forAll   bool
	forAny   bool
}

// operatorType groups the base operators by the type of their values.
func operatorType(base string) string {
	switch base {
	case "StringEquals", "StringNotEquals", "StringEqualsIgnoreCase", "StringNotEqualsIgnoreCase",
		"StringLike", "StringNotLike":
		return "string"
	case "NumericEquals", "NumericNotEquals", "NumericLessThan", "NumericLessThanEquals",
		"NumericGreaterThan", "NumericGreaterThanEquals":
		return "numeric"
	case "DateEquals", "DateNotEquals", "DateLessThan", "DateLessThanEquals",
		"DateGreaterThan", "DateGreaterThanEquals":
		return "date"
	case "Bool":
		return "bool"
	case "IpAddress", "NotIpAddress":
		return "ip"
	case "ArnEquals", "ArnLike", "ArnNotEquals", "ArnNotLike":
		return "arn"
	case "Null":
		return "null"
	}
	return ""
}

// isNegated reports whether a base operator matches when the value is
// absent from the policy values.
func isNegated(base string) bool {
	switch base {
	case "StringNotEquals", "StringNotEqualsIgnoreCase", "StringNotLike", "NumericNotEquals",
		"DateNotEquals", "NotIpAddress", "ArnNotEquals", "ArnNotLike":
		return true
	}
	return false
}

// parseOperator splits the set qualifier and the IfExists suffix off an
// operator name. ok is false for unknown operators and for the invalid
// combinations NullIfExists and qualified Null.
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
	if base, found := strings.CutSuffix(op, "IfExists"); found {
		c.ifExists = true
		op = base
	}
	if operatorType(op) == "" {
		return c, false
	}
	if op == "Null" && (c.ifExists || c.forAll || c.forAny) {
		return c, false
	}
	c.base = op
	c.negated = isNegated(op)
	return c, true
}

// checkConditionValue validates one policy value of an operator. Values
// with policy variables are checked at evaluation time.
func checkConditionValue(base, v string) bool {
	if strings.Contains(v, "${") {
		return true
	}
	switch operatorType(base) {
	case "numeric":
		_, ok := parseNumber(v)
		return ok
	case "date":
		_, ok := parseDate(v)
		return ok
	case "bool", "null":
		return strings.EqualFold(v, "true") || strings.EqualFold(v, "false")
	case "ip":
		_, ok := parseNetwork(v)
		return ok
	}
	return true
}

// evalCondition evaluates a condition block: every operator, and every
// key of every operator, must hold. known is false when the block uses an
// unknown operator (possible only in documents that skipped validation).
func evalCondition(cond domain.Condition, ctx *evalContext) (holds, known bool) {
	for op := range cond {
		if _, ok := parseOperator(op); !ok {
			return false, false
		}
	}
	for op, keys := range cond {
		parsed, _ := parseOperator(op)
		for key, values := range keys {
			if !evalKey(parsed, ctx, key, values) {
				return false, true
			}
		}
	}
	return true, true
}

// evalKey evaluates one operator on one condition key.
func evalKey(op conditionOperator, ctx *evalContext, key string, values domain.StringSet) bool {
	actual, present := ctx.get(key)
	absent := !present || len(actual) == 0
	if op.base == "Null" {
		for _, v := range values {
			if strings.EqualFold(v, "true") == absent {
				return true
			}
		}
		return false
	}
	if absent {
		switch {
		case op.ifExists, op.forAll:
			return true // IfExists skips absent keys; ForAllValues of an empty set holds
		case op.forAny:
			return false
		default:
			return op.negated
		}
	}
	policy := resolveValues(op, values, ctx)
	switch {
	case op.forAll:
		for _, a := range actual {
			if !valueHolds(op, a, policy) {
				return false
			}
		}
		return true
	case op.forAny:
		for _, a := range actual {
			if valueHolds(op, a, policy) {
				return true
			}
		}
		return false
	case op.negated:
		// Every request value must avoid every policy value.
		for _, a := range actual {
			if !valueHolds(op, a, policy) {
				return false
			}
		}
		return true
	default:
		for _, a := range actual {
			if valueHolds(op, a, policy) {
				return true
			}
		}
		return false
	}
}

// policyValue is a condition value after variable expansion.
type policyValue struct {
	text    string
	pattern pattern
	// unresolved marks a value whose variables cannot be resolved: it
	// never matches a positive operator and always matches a negated one,
	// so that it never widens the statement.
	unresolved bool
}

func resolveValues(op conditionOperator, values domain.StringSet, ctx *evalContext) []policyValue {
	out := make([]policyValue, 0, len(values))
	for _, v := range values {
		var pv policyValue
		switch op.base {
		case "StringLike", "StringNotLike", "ArnLike", "ArnNotLike", "ArnEquals", "ArnNotEquals":
			p, ok := compilePattern(v, ctx)
			pv = policyValue{pattern: p, unresolved: !ok}
		default:
			s, ok := expandLiteral(v, ctx)
			pv = policyValue{text: s, unresolved: !ok}
		}
		out = append(out, pv)
	}
	return out
}

// valueHolds reports whether one request value satisfies the operator:
// for positive operators it matches some policy value, for negated ones it
// matches none. A request value of the wrong type (a non-numeric value for
// a Numeric operator, ...) never satisfies the operator.
func valueHolds(op conditionOperator, actual string, policy []policyValue) bool {
	matched := false
	for _, p := range policy {
		if p.unresolved {
			if op.negated {
				matched = true
			}
			continue
		}
		m, ok := compare(op.base, actual, p)
		if !ok {
			return false
		}
		if m {
			matched = true
		}
	}
	if op.negated {
		return !matched
	}
	return matched
}

// compare evaluates a (request value, policy value) pair for the base
// operator, ignoring negation. ok is false when a value has the wrong type.
func compare(base, actual string, p policyValue) (matched, ok bool) {
	switch base {
	case "StringEquals", "StringNotEquals":
		return actual == p.text, true
	case "StringEqualsIgnoreCase", "StringNotEqualsIgnoreCase":
		return strings.EqualFold(actual, p.text), true
	case "StringLike", "StringNotLike":
		return p.pattern.match(actual, false), true
	case "ArnEquals", "ArnNotEquals", "ArnLike", "ArnNotLike":
		return matchARN(p.pattern, actual), true
	case "Bool":
		a, err1 := strconv.ParseBool(strings.ToLower(actual))
		b, err2 := strconv.ParseBool(strings.ToLower(p.text))
		if err1 != nil || err2 != nil {
			return false, false
		}
		return a == b, true
	case "IpAddress", "NotIpAddress":
		network, ok1 := parseNetwork(p.text)
		addr, ok2 := parseAddr(actual)
		if !ok1 || !ok2 {
			return false, false
		}
		return network.Contains(addr), true
	}
	switch operatorType(base) {
	case "numeric":
		a, ok1 := parseNumber(actual)
		b, ok2 := parseNumber(p.text)
		if !ok1 || !ok2 {
			return false, false
		}
		return ordered(strings.TrimPrefix(base, "Numeric"), a, b), true
	case "date":
		a, ok1 := parseDate(actual)
		b, ok2 := parseDate(p.text)
		if !ok1 || !ok2 {
			return false, false
		}
		return ordered(strings.TrimPrefix(base, "Date"), float64(a.UnixNano()), float64(b.UnixNano())), true
	}
	return false, false
}

// ordered applies a relation (Equals, NotEquals, LessThan, ...) ignoring
// negation: NotEquals compares for equality, the caller negates.
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

// parseNumber parses a finite number.
func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// parseDate accepts the ISO 8601 forms used in policies and epoch seconds.
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05", "2006-01-02T15:04Z07:00", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(epoch, 0), true
	}
	return time.Time{}, false
}

// parseNetwork parses a CIDR block or a single address (IPv4 or IPv6).
// IPv4-mapped IPv6 networks are unmapped.
func parseNetwork(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, false
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p.Masked(), true
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, false
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), true
}

// parseAddr parses a request address, tolerating a port, brackets and an
// IPv6 zone; IPv4-mapped IPv6 addresses are unmapped.
func parseAddr(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(strings.Trim(s, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}
