package match

import (
	"regexp"
	"strings"
)

// globParser translates a glob into an RE2 expression. pos is the index
// of the next rune of src; positions reported in errors are 1-based.
type globParser struct {
	src []rune
	pos int
	out strings.Builder
}

// globError is a translation error at a 1-based position.
type globError struct {
	pos int
	msg string
}

// globToRegex translates a glob into the body of an anchored RE2
// expression (without the ^ and $ anchors).
func globToRegex(glob string) (string, *globError) {
	p := &globParser{src: []rune(glob)}
	if err := p.sequence(true, false); err != nil {
		return "", err
	}
	if p.pos < len(p.src) {
		// Only an unbalanced '}' or ',' could stop the top level, and
		// both are literal there.
		return "", &globError{pos: p.pos + 1, msg: "unexpected character"}
	}
	return p.out.String(), nil
}

// sequence translates runes up to the end of the source or, inside
// braces, up to the ',' or '}' that ends the alternative. segStart tells
// whether the sequence starts a path segment (for "**/").
func (p *globParser) sequence(segStart, inBraces bool) *globError {
	for p.pos < len(p.src) {
		r := p.src[p.pos]
		switch {
		case inBraces && (r == ',' || r == '}'):
			return nil
		case r == '\\':
			if p.pos+1 >= len(p.src) {
				return &globError{pos: p.pos + 1, msg: "trailing backslash"}
			}
			p.out.WriteString(regexp.QuoteMeta(string(p.src[p.pos+1])))
			p.pos += 2
			segStart = false
		case r == '*':
			p.star(segStart)
			segStart = false
		case r == '?':
			p.out.WriteString("[^/]")
			p.pos++
			segStart = false
		case r == '[':
			if err := p.class(); err != nil {
				return err
			}
			segStart = false
		case r == '{':
			if err := p.braces(segStart); err != nil {
				return err
			}
			segStart = false
		default:
			p.out.WriteString(regexp.QuoteMeta(string(r)))
			p.pos++
			segStart = r == '/'
		}
	}
	return nil
}

// star translates a run of '*': one star stays within a segment, two or
// more cross segments; "**/" at the start of a segment also matches no
// segment.
func (p *globParser) star(segStart bool) {
	n := 0
	for p.pos < len(p.src) && p.src[p.pos] == '*' {
		n++
		p.pos++
	}
	if n == 1 {
		p.out.WriteString("[^/]*")
		return
	}
	if segStart && p.pos < len(p.src) && p.src[p.pos] == '/' {
		p.out.WriteString("(?:.*/)?")
		p.pos++
		return
	}
	p.out.WriteString(".*")
}

// class translates a character class "[...]": "!" or "^" first negates
// it (a negated class never matches '/'), "]" first is literal, "a-z" is
// a range, "\" escapes the next character.
func (p *globParser) class() *globError {
	start := p.pos
	p.pos++ // '['
	negated := false
	if p.pos < len(p.src) && (p.src[p.pos] == '!' || p.src[p.pos] == '^') {
		negated = true
		p.pos++
	}
	var body strings.Builder
	first := true
	for {
		if p.pos >= len(p.src) {
			return &globError{pos: start + 1, msg: "unterminated character class"}
		}
		r := p.src[p.pos]
		if r == ']' && !first {
			p.pos++
			break
		}
		first = false
		lo, err := p.classChar()
		if err != nil {
			return err
		}
		if p.pos+1 < len(p.src) && p.src[p.pos] == '-' && p.src[p.pos+1] != ']' {
			rangePos := p.pos
			p.pos++
			hi, err := p.classChar()
			if err != nil {
				return err
			}
			if hi < lo {
				return &globError{pos: rangePos + 1, msg: "invalid character class range " + string(lo) + "-" + string(hi)}
			}
			body.WriteString(classEscape(lo) + "-" + classEscape(hi))
			continue
		}
		body.WriteString(classEscape(lo))
	}
	p.out.WriteByte('[')
	if negated {
		p.out.WriteString("^/")
	}
	p.out.WriteString(body.String())
	p.out.WriteByte(']')
	return nil
}

// classChar reads one (possibly escaped) character of a class.
func (p *globParser) classChar() (rune, *globError) {
	r := p.src[p.pos]
	if r == '\\' {
		if p.pos+1 >= len(p.src) {
			return 0, &globError{pos: p.pos + 1, msg: "trailing backslash"}
		}
		p.pos += 2
		return p.src[p.pos-1], nil
	}
	p.pos++
	return r, nil
}

// classEscape escapes a character for the inside of an RE2 class.
func classEscape(r rune) string {
	switch r {
	case '\\', ']', '[', '^', '-':
		return `\` + string(r)
	}
	return string(r)
}

// braces translates an alternation "{a,b,...}" whose alternatives are
// globs.
func (p *globParser) braces(segStart bool) *globError {
	start := p.pos
	p.pos++ // '{'
	p.out.WriteString("(?:")
	for {
		if err := p.sequence(segStart, true); err != nil {
			return err
		}
		if p.pos >= len(p.src) {
			return &globError{pos: start + 1, msg: "unterminated alternation"}
		}
		if p.src[p.pos] == '}' {
			p.pos++
			p.out.WriteByte(')')
			return nil
		}
		// ','
		p.pos++
		p.out.WriteByte('|')
	}
}
