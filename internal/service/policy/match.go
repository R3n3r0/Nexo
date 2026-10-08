package policy

import (
	"strings"
	"unicode"
)

// pchar is one element of a compiled wildcard pattern.
type pchar struct {
	r    rune
	kind uint8
}

// Kinds of pattern elements.
const (
	pLit  uint8 = iota // a literal rune
	pStar              // "*": any sequence, empty included
	pAny               // "?": any single rune
)

// pattern is a compiled wildcard pattern whose literal runes may come from
// policy variables (and therefore contain '*' or '?' that are not
// wildcards).
type pattern []pchar

// compileWildcard compiles s, where "*" and "?" are wildcards and there are
// no policy variables (actions, principals).
func compileWildcard(s string) pattern {
	p := make(pattern, 0, len(s))
	return appendWildcard(p, s)
}

func appendWildcard(p pattern, s string) pattern {
	for _, r := range s {
		switch r {
		case '*':
			p = append(p, pchar{kind: pStar})
		case '?':
			p = append(p, pchar{kind: pAny})
		default:
			p = append(p, pchar{r: r})
		}
	}
	return p
}

func appendLiteral(p pattern, s string) pattern {
	for _, r := range s {
		p = append(p, pchar{r: r})
	}
	return p
}

// compilePattern compiles a policy string with wildcards and policy
// variables, expanded from ctx as literal text. ok is false when a variable
// cannot be resolved.
func compilePattern(s string, ctx *evalContext) (pattern, bool) {
	p := make(pattern, 0, len(s))
	ok := expandTemplate(s, ctx, func(text string, fromVar bool) {
		if fromVar {
			p = appendLiteral(p, text)
		} else {
			p = appendWildcard(p, text)
		}
	})
	return p, ok
}

// expandLiteral expands the policy variables of s into a plain string (the
// operators without wildcards). ok is false when a variable cannot be
// resolved.
func expandLiteral(s string, ctx *evalContext) (string, bool) {
	if !strings.Contains(s, "${") {
		return s, true
	}
	var sb strings.Builder
	ok := expandTemplate(s, ctx, func(text string, _ bool) { sb.WriteString(text) })
	return sb.String(), ok
}

// match reports whether s matches the pattern; fold compares runes
// case-insensitively.
func (p pattern) match(s string, fold bool) bool {
	return matchRunes(p, []rune(s), fold)
}

func matchRunes(p pattern, s []rune, fold bool) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi].kind == pStar:
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi].kind == pAny || runeEqual(p[pi].r, s[si], fold)):
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi].kind == pStar {
		pi++
	}
	return pi == len(p)
}

func runeEqual(a, b rune, fold bool) bool {
	if a == b {
		return true
	}
	return fold && unicode.ToLower(a) == unicode.ToLower(b)
}

// matchWildcard reports whether s matches the wildcard pattern (no
// variables).
func matchWildcard(pat, s string, fold bool) bool {
	if pat == "*" {
		return true
	}
	if !strings.ContainsAny(pat, "*?") {
		if fold {
			return strings.EqualFold(pat, s)
		}
		return pat == s
	}
	return compileWildcard(pat).match(s, fold)
}

// splitARN splits a compiled ARN pattern into its six colon-separated
// components; the last one keeps any further colon. ok is false when the
// pattern has fewer than six components.
func (p pattern) splitARN() ([]pattern, bool) {
	parts := make([]pattern, 0, 6)
	start := 0
	for i, c := range p {
		if len(parts) == 5 {
			break
		}
		if c.kind == pLit && c.r == ':' {
			parts = append(parts, p[start:i])
			start = i + 1
		}
	}
	if len(parts) != 5 {
		return nil, false
	}
	return append(parts, p[start:]), true
}

// matchARN implements ArnLike/ArnEquals: each of the six components of the
// ARN is matched separately, with wildcards. Patterns that are not ARNs
// fall back to a whole-string match.
func matchARN(p pattern, value string) bool {
	parts, ok := p.splitARN()
	if !ok {
		return p.match(value, false)
	}
	values := strings.SplitN(value, ":", 6)
	if len(values) != 6 {
		return false
	}
	for i := range parts {
		if !parts[i].match(values[i], false) {
			return false
		}
	}
	return true
}

// expandTemplate walks a policy string, emitting its literal text and the
// values of its ${...} variables in order. It returns false when a
// variable cannot be resolved. An unterminated "${" is literal text
// (Validate rejects it in stored documents).
func expandTemplate(s string, ctx *evalContext, emit func(text string, fromVar bool)) bool {
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			emit(s, false)
			return true
		}
		j := strings.IndexByte(s[i+2:], '}')
		if j < 0 {
			emit(s, false)
			return true
		}
		if i > 0 {
			emit(s[:i], false)
		}
		v, ok := resolveVariable(s[i+2:i+2+j], ctx)
		if !ok {
			return false
		}
		emit(v, true)
		s = s[i+3+j:]
	}
}

// resolveVariable resolves the inside of a ${...} reference: the escapes
// "*", "?" and "$", a condition key, or "key, 'default'".
func resolveVariable(inner string, ctx *evalContext) (string, bool) {
	switch inner {
	case "*", "?", "$":
		return inner, true
	}
	key, def, hasDef, valid := parseVariable(inner)
	if !valid {
		return "", false
	}
	if ctx != nil {
		if v, ok := ctx.variable(key); ok {
			return v, true
		}
	}
	return def, hasDef
}

// parseVariable splits "key" or "key, 'default'".
func parseVariable(inner string) (key, def string, hasDef, valid bool) {
	k, d, found := strings.Cut(inner, ",")
	key = strings.TrimSpace(k)
	if !found {
		return key, "", false, key != "" && !strings.ContainsAny(key, " '{}$")
	}
	d = strings.TrimSpace(d)
	if len(d) < 2 || d[0] != '\'' || d[len(d)-1] != '\'' {
		return key, "", false, false
	}
	return key, d[1 : len(d)-1], true, key != "" && !strings.ContainsAny(key, " '{}$")
}

// checkVariables reports the first malformed ${...} reference of s.
func checkVariables(s string) (string, bool) {
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			return "", true
		}
		j := strings.IndexByte(s[i+2:], '}')
		if j < 0 {
			return s[i:], false
		}
		inner := s[i+2 : i+2+j]
		switch inner {
		case "*", "?", "$":
		default:
			if _, _, _, valid := parseVariable(inner); !valid {
				return s[i : i+3+j], false
			}
		}
		s = s[i+3+j:]
	}
}

// normalizeKey makes condition keys case-insensitive: the part before the
// first "/" (the key name) is lowercased, the rest (a tag key such as in
// s3:ExistingObjectTag/<key>) is kept as is.
func normalizeKey(k string) string {
	if i := strings.IndexByte(k, '/'); i >= 0 {
		return strings.ToLower(k[:i]) + k[i:]
	}
	return strings.ToLower(k)
}
