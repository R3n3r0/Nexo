package match

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
)

// service is the default matcher service. It is stateless: the compiled
// patterns are cached by their users (the access rules of a bucket, the
// behaviors of a distribution).
type service struct {
	opts options
}

var _ Service = (*service)(nil)

// New returns the matcher service. It has no dependencies: Deps keeps the
// constructor shape of every service.
func New(_ Deps, opts ...Option) Service {
	return &service{opts: newOptions(opts)}
}

// Normalize implements Service.
func (s *service) Normalize(_ context.Context, key string) string {
	return normalizeKey(key, s.opts.nfc)
}

// Decode implements Service.
func (s *service) Decode(_ context.Context, raw string) (string, error) {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return "", fmt.Errorf("%w: malformed escape in %q", domain.ErrInvalidArgument, raw)
	}
	return normalizeKey(decoded, s.opts.nfc), nil
}

// Compile implements Service.
func (s *service) Compile(_ context.Context, p Pattern) (Matcher, error) {
	m, perr := s.compile(p)
	if perr != nil {
		return nil, perr
	}
	return m, nil
}

// CompileSet implements Service.
func (s *service) CompileSet(_ context.Context, patterns []Pattern) (Set, error) {
	ms := make([]*matcher, len(patterns))
	for i, p := range patterns {
		m, perr := s.compile(p)
		if perr != nil {
			perr.Index = i
			return nil, perr
		}
		ms[i] = m
	}
	return newSet(ms), nil
}

// compile validates and compiles one pattern.
func (s *service) compile(p Pattern) (*matcher, *PatternError) {
	if p.Pattern == "" && p.Type != TypePrefix {
		return nil, patternError(p, 0, "the pattern is empty")
	}
	if !utf8.ValidString(p.Pattern) {
		return nil, patternError(p, 0, "the pattern is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(p.Pattern); n > s.opts.maxLength {
		return nil, patternError(p, s.opts.maxLength+1, "the pattern is %d characters long, longer than %d", n, s.opts.maxLength)
	}
	src := p.Pattern
	if s.opts.nfc {
		src = toNFC(src)
	}
	m := &matcher{pattern: p, fold: p.CaseInsensitive}
	switch p.Type {
	case TypePrefix:
		m.prefix = src
		if m.fold {
			m.prefix = foldString(src)
		}
		m.literal, m.literalFold = m.prefix, m.fold
		m.expr = "^" + regexp.QuoteMeta(src)
		if m.fold {
			m.expr = "(?i)" + m.expr
		}
		return m, nil
	case TypeGlob:
		body, gerr := globToRegex(src)
		if gerr != nil {
			return nil, patternError(p, gerr.pos, "%s", gerr.msg)
		}
		m.expr = "^" + body + "$"
	case TypeRegex:
		m.expr = src
	default:
		return nil, patternError(p, 0, "unknown pattern type %q (prefix, glob or regex)", p.Type)
	}
	if m.fold {
		m.expr = "(?i)" + m.expr
	}
	re, err := regexp.Compile(m.expr)
	if err != nil {
		return nil, regexError(p, src, err)
	}
	m.re = re
	for _, name := range re.SubexpNames() {
		if name != "" {
			m.groups = append(m.groups, name)
		}
	}
	m.literal, m.literalFold = anchoredLiteral(m.expr)
	if m.literalFold {
		m.literal = foldString(m.literal)
	}
	m.need = prefilterOf(m.expr)
	return m, nil
}

// regexError locates a compilation error of a regex (or of the
// expression of a glob, which only fails on resource limits).
func regexError(p Pattern, src string, err error) *PatternError {
	var se *syntax.Error
	msg := err.Error()
	pos := 0
	if e, ok := err.(*syntax.Error); ok {
		se = e
		msg = string(se.Code)
		if se.Expr != "" {
			msg += ": " + se.Expr
			if i := strings.Index(src, se.Expr); i >= 0 {
				pos = utf8.RuneCountInString(src[:i]) + 1
			}
		}
	}
	if p.Type == TypeGlob {
		pos = 0
	}
	return patternError(p, pos, "%s", msg)
}

// anchoredLiteral returns the literal text every match of an expression
// anchored at the start of the text begins with, and whether that literal
// is matched with case folding; "" when the expression is not anchored at
// the start or does not begin with a literal. A folded literal is
// returned as written: the caller folds it (foldString).
func anchoredLiteral(expr string) (literal string, fold bool) {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return "", false
	}
	re = re.Simplify()
	subs := []*syntax.Regexp{re}
	if re.Op == syntax.OpConcat {
		subs = re.Sub
	}
	if len(subs) == 0 || subs[0].Op != syntax.OpBeginText {
		return "", false
	}
	var b strings.Builder
	for i, sub := range subs[1:] {
		if sub.Op != syntax.OpLiteral {
			break
		}
		f := sub.Flags&syntax.FoldCase != 0
		if i > 0 && f != fold {
			break
		}
		fold = f
		b.WriteString(string(sub.Rune))
	}
	return b.String(), fold
}

// matcher is a compiled pattern.
type matcher struct {
	pattern Pattern
	// fold matches with simple case folding: keys are folded before the
	// prefix comparisons.
	fold bool
	// prefix is the (folded) literal of a prefix pattern.
	prefix string
	// re is the compiled expression of a glob or a regex.
	re     *regexp.Regexp
	expr   string
	groups []string
	// literal is the text every match begins with, "" when none: the key
	// of the trie of a Set. literalFold reports a folded literal
	// (foldString), compared with the folded key.
	literal     string
	literalFold bool
	// need is the prefilter of the expression: literals one of which
	// every match contains.
	need requirement
}

var _ Matcher = (*matcher)(nil)

// Pattern implements Matcher.
func (m *matcher) Pattern() Pattern { return m.pattern }

// Groups implements Matcher.
func (m *matcher) Groups() []string { return append([]string(nil), m.groups...) }

// Expr implements Matcher.
func (m *matcher) Expr() string { return m.expr }

// Match implements Matcher.
func (m *matcher) Match(key string) Result {
	if m.re == nil {
		k := key
		if m.fold {
			k = foldString(key)
		}
		return Result{Matched: strings.HasPrefix(k, m.prefix)}
	}
	return m.matchExpr(key)
}

// matchExpr evaluates the expression, capturing the named groups.
func (m *matcher) matchExpr(key string) Result {
	if !m.need.possible(key, "") {
		return Result{}
	}
	return m.run(key)
}

// run evaluates the expression without the prefilter.
func (m *matcher) run(key string) Result {
	if len(m.groups) == 0 {
		return Result{Matched: m.re.MatchString(key)}
	}
	sub := m.re.FindStringSubmatch(key)
	if sub == nil {
		return Result{}
	}
	caps := make(map[string]string, len(m.groups))
	for i, name := range m.re.SubexpNames() {
		if name != "" && i < len(sub) {
			caps[name] = sub[i]
		}
	}
	return Result{Matched: true, Captures: caps}
}
