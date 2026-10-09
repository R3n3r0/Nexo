package distribution

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// MaxPatternLength bounds a path pattern (docs/10 §10.1).
const MaxPatternLength = 1024

// matcher is the built-in Matcher: literal prefixes, globs compiled into
// anchored RE2 expressions, RE2 expressions.
type matcher struct{}

var _ Matcher = matcher{}

// NewMatcher returns the built-in Matcher.
func NewMatcher() Matcher { return matcher{} }

// Compile implements Matcher.
func (matcher) Compile(_ context.Context, p PathPattern) (CompiledPattern, error) {
	if p.Pattern == "" {
		return nil, fmt.Errorf("%w: empty path pattern", ErrInvalid)
	}
	if len(p.Pattern) > MaxPatternLength {
		return nil, fmt.Errorf("%w: path pattern longer than %d characters", ErrInvalid, MaxPatternLength)
	}
	switch p.Type {
	case PatternPrefix:
		if p.CaseInsensitive {
			return prefixPattern{prefix: strings.ToLower(p.Pattern), fold: true}, nil
		}
		return prefixPattern{prefix: p.Pattern}, nil
	case PatternGlob:
		expr, err := globToRegex(p.Pattern)
		if err != nil {
			return nil, err
		}
		return compileRegex(expr, p.CaseInsensitive)
	case PatternRegex:
		return compileRegex(p.Pattern, p.CaseInsensitive)
	default:
		return nil, fmt.Errorf("%w: path pattern type %q must be prefix, glob or regex", ErrInvalid, p.Type)
	}
}

type prefixPattern struct {
	prefix string
	fold   bool
}

// Match implements CompiledPattern.
func (p prefixPattern) Match(path string) bool {
	if p.fold {
		return strings.HasPrefix(strings.ToLower(path), p.prefix)
	}
	return strings.HasPrefix(path, p.prefix)
}

type regexPattern struct{ re *regexp.Regexp }

// Match implements CompiledPattern.
func (p regexPattern) Match(path string) bool { return p.re.MatchString(path) }

// compileRegex compiles an RE2 expression; the error locates the problem.
func compileRegex(expr string, fold bool) (CompiledPattern, error) {
	if fold {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid regular expression: %v", ErrInvalid, err)
	}
	return regexPattern{re: re}, nil
}

// globToRegex translates a glob into an anchored RE2 expression: "**"
// spans segments ("**/" also matches no segment, a final "/**" also
// matches the directory itself), "*" and "?" stay within a segment,
// "[...]" ("[!...]" negated) is a class and "{a,b}" an alternative.
func globToRegex(glob string) (string, error) {
	body, rest, err := translateGlob(glob, false)
	if err != nil {
		return "", err
	}
	if rest != "" {
		return "", fmt.Errorf("%w: glob %q: unexpected %q", ErrInvalid, glob, rest)
	}
	return "^" + body + "$", nil
}

// translateGlob translates glob until its end or, inside braces, until the
// closing brace or the comma of the alternative (returned in rest).
func translateGlob(glob string, inBraces bool) (string, string, error) {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				// "**/" may match no directory at all; "/**" at the end
				// matches the directory itself too.
				switch {
				case i+2 < len(glob) && glob[i+2] == '/':
					b.WriteString("(?:.*/)?")
					i += 2
				default:
					b.WriteString(".*")
					i++
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(glob[i+1:], ']')
			if end < 0 {
				return "", "", fmt.Errorf("%w: glob %q: unterminated character class", ErrInvalid, glob)
			}
			class := glob[i+1 : i+1+end]
			if class == "" {
				return "", "", fmt.Errorf("%w: glob %q: empty character class", ErrInvalid, glob)
			}
			b.WriteByte('[')
			if class[0] == '!' || class[0] == '^' {
				b.WriteByte('^')
				class = class[1:]
			}
			for _, r := range class {
				if r == '\\' || r == '[' || r == ']' {
					b.WriteByte('\\')
				}
				b.WriteRune(r)
			}
			b.WriteByte(']')
			i += end + 1
		case '{':
			var alts []string
			rest := glob[i+1:]
			for {
				alt, after, err := translateGlob(rest, true)
				if err != nil {
					return "", "", err
				}
				alts = append(alts, alt)
				if after == "" {
					return "", "", fmt.Errorf("%w: glob %q: unterminated alternative", ErrInvalid, glob)
				}
				if after[0] == '}' {
					rest = after[1:]
					break
				}
				rest = after[1:] // ','
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			sub, after, err := translateGlob(rest, inBraces)
			if err != nil {
				return "", "", err
			}
			b.WriteString(sub)
			return b.String(), after, nil
		case '}', ',':
			if inBraces {
				return b.String(), glob[i:], nil
			}
			writeLiteral(&b, c)
		case '\\':
			if i+1 < len(glob) {
				i++
				writeLiteral(&b, glob[i])
				continue
			}
			b.WriteString(`\\`)
		default:
			writeLiteral(&b, c)
		}
	}
	return b.String(), "", nil
}

// writeLiteral writes a byte of a glob as a literal of the expression: the
// ASCII metacharacters are escaped, the bytes of a multi-byte UTF-8
// sequence are copied as they are.
func writeLiteral(b *strings.Builder, c byte) {
	if c < 0x80 && strings.IndexByte(`\.+*?()|[]{}^$`, c) >= 0 {
		b.WriteByte('\\')
	}
	b.WriteByte(c)
}
