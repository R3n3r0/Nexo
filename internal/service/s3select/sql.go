package s3select

import (
	"strconv"
	"strings"
	"unicode"
)

// The SQL of S3 Select: a lexer and a recursive descent parser building
// the expression tree evaluated per record (eval.go).

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokQuoted // a "quoted" identifier
	tokString
	tokNumber
	tokSymbol
)

type token struct {
	kind tokenKind
	text string
	pos  int
}

// lex splits a query into tokens.
func lex(q string) ([]token, error) {
	var out []token
	rs := []rune(q)
	for i := 0; i < len(rs); {
		c := rs[i]
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '\'':
			var b strings.Builder
			j := i + 1
			for ; ; j++ {
				if j >= len(rs) {
					return nil, parseError("unterminated string at %d", i)
				}
				if rs[j] == '\'' {
					if j+1 < len(rs) && rs[j+1] == '\'' {
						b.WriteRune('\'')
						j++
						continue
					}
					break
				}
				b.WriteRune(rs[j])
			}
			out = append(out, token{kind: tokString, text: b.String(), pos: i})
			i = j + 1
		case c == '"':
			var b strings.Builder
			j := i + 1
			for ; ; j++ {
				if j >= len(rs) {
					return nil, parseError("unterminated identifier at %d", i)
				}
				if rs[j] == '"' {
					if j+1 < len(rs) && rs[j+1] == '"' {
						b.WriteRune('"')
						j++
						continue
					}
					break
				}
				b.WriteRune(rs[j])
			}
			out = append(out, token{kind: tokQuoted, text: b.String(), pos: i})
			i = j + 1
		case unicode.IsDigit(c):
			j := i
			for j < len(rs) && (unicode.IsDigit(rs[j]) || rs[j] == '.' || rs[j] == 'e' || rs[j] == 'E' ||
				((rs[j] == '+' || rs[j] == '-') && (rs[j-1] == 'e' || rs[j-1] == 'E'))) {
				j++
			}
			out = append(out, token{kind: tokNumber, text: string(rs[i:j]), pos: i})
			i = j
		case unicode.IsLetter(c) || c == '_':
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || rs[j] == '_') {
				j++
			}
			out = append(out, token{kind: tokIdent, text: string(rs[i:j]), pos: i})
			i = j
		default:
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "<=", ">=", "<>", "!=", "||":
				out = append(out, token{kind: tokSymbol, text: two, pos: i})
				i += 2
				continue
			}
			if !strings.ContainsRune("(),.*[]+-/%=<>", c) {
				return nil, parseError("unexpected character %q at %d", c, i)
			}
			out = append(out, token{kind: tokSymbol, text: string(c), pos: i})
			i++
		}
	}
	return append(out, token{kind: tokEOF, pos: len(rs)}), nil
}

// query is a parsed SELECT.
type query struct {
	star  bool
	items []selectItem
	// alias names S3Object in the paths (empty: none).
	alias string
	// from is the path applied to every document (S3Object[*].a.b).
	from      []pathStep
	where     expr
	limit     int64
	aggregate bool
	aggs      []*aggExpr
}

type selectItem struct {
	e    expr
	name string
}

type parser struct {
	toks []token
	i    int
	aggs []*aggExpr
	// inAgg counts the aggregates being parsed (no nesting).
	inAgg int
	// depth is the nesting of the expression being parsed.
	depth int
}

// maxDepth bounds the nesting of an expression (parentheses, NOT, unary
// minus), so that a hostile query cannot exhaust the stack.
const maxDepth = 256

// enter counts a level of nesting; leave undoes it.
func (p *parser) enter() error {
	p.depth++
	if p.depth > maxDepth {
		return unsupported("the expression nests more than %d levels", maxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

func (p *parser) peek() token { return p.toks[p.i] }
func (p *parser) next() token {
	t := p.toks[p.i]
	if t.kind != tokEOF {
		p.i++
	}
	return t
}

// keyword reports whether the next token is the keyword kw.
func (p *parser) keyword(kw string) bool {
	t := p.peek()
	return t.kind == tokIdent && strings.EqualFold(t.text, kw)
}

func (p *parser) acceptKeyword(kw string) bool {
	if p.keyword(kw) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return parseError("expected %s at %d, found %q", kw, p.peek().pos, p.peek().text)
	}
	return nil
}

func (p *parser) symbol(s string) bool {
	t := p.peek()
	return t.kind == tokSymbol && t.text == s
}

func (p *parser) acceptSymbol(s string) bool {
	if p.symbol(s) {
		p.i++
		return true
	}
	return false
}

func (p *parser) expectSymbol(s string) error {
	if !p.acceptSymbol(s) {
		return parseError("expected %q at %d, found %q", s, p.peek().pos, p.peek().text)
	}
	return nil
}

// reserved are the keywords that cannot be a bare alias.
func reserved(word string) bool {
	switch strings.ToUpper(word) {
	case "SELECT", "FROM", "WHERE", "LIMIT", "AS", "AND", "OR", "NOT", "LIKE", "ESCAPE", "BETWEEN", "IN", "IS",
		"NULL", "MISSING", "TRUE", "FALSE", "CAST":
		return true
	}
	return false
}

// parseQuery parses a SELECT statement.
func parseQuery(sql string) (*query, error) {
	toks, err := lex(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	q := &query{limit: -1}
	if err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}
	if p.acceptSymbol("*") {
		q.star = true
	} else {
		for {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			item := selectItem{e: e}
			if p.acceptKeyword("AS") {
				t := p.next()
				if t.kind != tokIdent && t.kind != tokQuoted {
					return nil, parseError("expected an alias at %d", t.pos)
				}
				item.name = t.text
			} else if t := p.peek(); (t.kind == tokIdent && !reserved(t.text)) || t.kind == tokQuoted {
				item.name = p.next().text
			}
			q.items = append(q.items, item)
			if !p.acceptSymbol(",") {
				break
			}
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	if t := p.next(); t.kind != tokIdent || !strings.EqualFold(t.text, "S3Object") {
		return nil, parseError("expected S3Object at %d, found %q", t.pos, t.text)
	}
	if p.acceptSymbol("[") {
		if err := p.expectSymbol("*"); err != nil {
			return nil, err
		}
		if err := p.expectSymbol("]"); err != nil {
			return nil, err
		}
	}
	for p.acceptSymbol(".") {
		t := p.next()
		if t.kind != tokIdent && t.kind != tokQuoted {
			return nil, parseError("expected a name at %d", t.pos)
		}
		q.from = append(q.from, pathStep{name: t.text, quoted: t.kind == tokQuoted})
	}
	if p.acceptKeyword("AS") {
		t := p.next()
		if t.kind != tokIdent && t.kind != tokQuoted {
			return nil, parseError("expected an alias at %d", t.pos)
		}
		q.alias = t.text
	} else if t := p.peek(); t.kind == tokIdent && !reserved(t.text) {
		q.alias = p.next().text
	}
	if p.acceptKeyword("WHERE") {
		before := len(p.aggs)
		if q.where, err = p.parseExpr(); err != nil {
			return nil, err
		}
		if len(p.aggs) != before {
			return nil, unsupported("aggregate functions are not allowed in WHERE")
		}
	}
	if p.acceptKeyword("LIMIT") {
		t := p.next()
		n, err := strconv.ParseInt(t.text, 10, 64)
		if t.kind != tokNumber || err != nil || n < 0 {
			return nil, parseError("LIMIT takes a non-negative integer, found %q", t.text)
		}
		q.limit = n
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, parseError("unexpected %q at %d", t.text, t.pos)
	}
	q.aggs = p.aggs
	q.aggregate = len(p.aggs) > 0
	if q.aggregate {
		for _, it := range q.items {
			if hasBarePath(it.e) {
				return nil, unsupported("a query with aggregate functions can only select aggregates")
			}
		}
	}
	for i := range q.items {
		if q.items[i].name == "" {
			q.items[i].name = defaultName(q.items[i].e, i)
		}
	}
	return q, nil
}

// defaultName is the name of an output column without alias: the last
// name of a path, _N otherwise.
func defaultName(e expr, i int) string {
	if pe, ok := e.(*pathExpr); ok && len(pe.steps) > 0 {
		if last := pe.steps[len(pe.steps)-1]; !last.isIndex {
			return last.name
		}
	}
	return "_" + strconv.Itoa(i+1)
}

// hasBarePath reports whether e reads a field outside an aggregate.
func hasBarePath(e expr) bool {
	switch x := e.(type) {
	case *pathExpr:
		return true
	case *aggExpr:
		return false
	case *binaryExpr:
		return hasBarePath(x.l) || hasBarePath(x.r)
	case *unaryExpr:
		return hasBarePath(x.e)
	case *funcExpr:
		for _, a := range x.args {
			if hasBarePath(a) {
				return true
			}
		}
	case *castExpr:
		return hasBarePath(x.e)
	}
	return false
}

func (p *parser) parseExpr() (expr, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	return p.parseOr()
}

func (p *parser) parseOr() (expr, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &binaryExpr{op: "OR", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (expr, error) {
	l, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		r, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		l = &binaryExpr{op: "AND", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseNot() (expr, error) {
	if p.acceptKeyword("NOT") {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
		e, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &unaryExpr{op: "NOT", e: e}, nil
	}
	return p.parseComparison()
}

func (p *parser) parseComparison() (expr, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"=", "!=", "<>", "<=", ">=", "<", ">"} {
		if p.acceptSymbol(op) {
			r, err := p.parseAdd()
			if err != nil {
				return nil, err
			}
			if op == "<>" {
				op = "!="
			}
			return &binaryExpr{op: op, l: l, r: r}, nil
		}
	}
	if p.acceptKeyword("IS") {
		not := p.acceptKeyword("NOT")
		var e expr
		switch {
		case p.acceptKeyword("NULL"):
			e = &isExpr{e: l, missing: false}
		case p.acceptKeyword("MISSING"):
			e = &isExpr{e: l, missing: true}
		default:
			return nil, parseError("expected NULL or MISSING after IS at %d", p.peek().pos)
		}
		if not {
			e = &unaryExpr{op: "NOT", e: e}
		}
		return e, nil
	}
	not := false
	if p.keyword("NOT") {
		if t := p.toks[p.i+1]; t.kind == tokIdent && (strings.EqualFold(t.text, "LIKE") || strings.EqualFold(t.text, "BETWEEN") || strings.EqualFold(t.text, "IN")) {
			p.i++
			not = true
		}
	}
	var e expr
	switch {
	case p.acceptKeyword("LIKE"):
		pattern, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		var escape expr
		if p.acceptKeyword("ESCAPE") {
			if escape, err = p.parseAdd(); err != nil {
				return nil, err
			}
		}
		e = &likeExpr{e: l, pattern: pattern, escape: escape}
	case p.acceptKeyword("BETWEEN"):
		lo, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		e = &binaryExpr{op: "AND", l: &binaryExpr{op: ">=", l: l, r: lo}, r: &binaryExpr{op: "<=", l: l, r: hi}}
	case p.acceptKeyword("IN"):
		if err := p.expectSymbol("("); err != nil {
			return nil, err
		}
		in := &inExpr{e: l}
		for {
			v, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			in.list = append(in.list, v)
			if !p.acceptSymbol(",") {
				break
			}
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		e = in
	default:
		if not {
			return nil, parseError("unexpected NOT at %d", p.peek().pos)
		}
		return l, nil
	}
	if not {
		e = &unaryExpr{op: "NOT", e: e}
	}
	return e, nil
}

func (p *parser) parseAdd() (expr, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptSymbol("+"):
			op = "+"
		case p.acceptSymbol("-"):
			op = "-"
		case p.acceptSymbol("||"):
			op = "||"
		default:
			return l, nil
		}
		r, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		l = &binaryExpr{op: op, l: l, r: r}
	}
}

func (p *parser) parseMul() (expr, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.acceptSymbol("*"):
			op = "*"
		case p.acceptSymbol("/"):
			op = "/"
		case p.acceptSymbol("%"):
			op = "%"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &binaryExpr{op: op, l: l, r: r}
	}
}

func (p *parser) parseUnary() (expr, error) {
	if p.symbol("-") || p.symbol("+") {
		if err := p.enter(); err != nil {
			return nil, err
		}
		defer p.leave()
	}
	if p.acceptSymbol("-") {
		e, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryExpr{op: "-", e: e}, nil
	}
	if p.acceptSymbol("+") {
		return p.parseUnary()
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (expr, error) {
	t := p.peek()
	switch t.kind {
	case tokString:
		p.next()
		return &literal{v: stringValue(t.text)}, nil
	case tokNumber:
		p.next()
		if i, err := strconv.ParseInt(t.text, 10, 64); err == nil {
			return &literal{v: intValue(i)}, nil
		}
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, parseError("invalid number %q at %d", t.text, t.pos)
		}
		return &literal{v: floatValue(f)}, nil
	case tokSymbol:
		if p.acceptSymbol("(") {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expectSymbol(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
		return nil, parseError("unexpected %q at %d", t.text, t.pos)
	case tokQuoted:
		return p.parsePath()
	case tokIdent:
		switch strings.ToUpper(t.text) {
		case "NULL":
			p.next()
			return &literal{v: nullValue()}, nil
		case "TRUE", "FALSE":
			p.next()
			return &literal{v: boolValue(strings.EqualFold(t.text, "TRUE"))}, nil
		case "CAST":
			return p.parseCast()
		}
		if p.toks[p.i+1].kind == tokSymbol && p.toks[p.i+1].text == "(" {
			return p.parseCall()
		}
		if reserved(t.text) {
			return nil, parseError("unexpected %s at %d", t.text, t.pos)
		}
		return p.parsePath()
	}
	return nil, parseError("unexpected end of the expression")
}

func (p *parser) parsePath() (expr, error) {
	var steps []pathStep
	t := p.next()
	steps = append(steps, pathStep{name: t.text, quoted: t.kind == tokQuoted})
	for {
		switch {
		case p.acceptSymbol("."):
			t := p.next()
			if t.kind != tokIdent && t.kind != tokQuoted {
				return nil, parseError("expected a name after . at %d", t.pos)
			}
			steps = append(steps, pathStep{name: t.text, quoted: t.kind == tokQuoted})
		case p.acceptSymbol("["):
			t := p.next()
			n, err := strconv.Atoi(t.text)
			if t.kind != tokNumber || err != nil || n < 0 {
				if t.kind == tokString {
					steps = append(steps, pathStep{name: t.text, quoted: true})
				} else {
					return nil, parseError("expected an index at %d", t.pos)
				}
			} else {
				steps = append(steps, pathStep{isIndex: true, index: n})
			}
			if err := p.expectSymbol("]"); err != nil {
				return nil, err
			}
		default:
			return &pathExpr{steps: steps}, nil
		}
	}
}

func (p *parser) parseCast() (expr, error) {
	p.next()
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	e, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("AS"); err != nil {
		return nil, err
	}
	t := p.next()
	typ := strings.ToUpper(t.text)
	switch typ {
	case "INT", "INTEGER":
		typ = "INT"
	case "FLOAT", "DECIMAL", "NUMERIC", "DOUBLE":
		typ = "FLOAT"
	case "STRING", "VARCHAR", "CHAR":
		typ = "STRING"
	case "BOOL", "BOOLEAN":
		typ = "BOOL"
	default:
		return nil, unsupported("CAST to %s is not supported", t.text)
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return &castExpr{e: e, typ: typ}, nil
}

func (p *parser) parseCall() (expr, error) {
	name := strings.ToUpper(p.next().text)
	p.next() // (
	switch name {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		if p.inAgg > 0 {
			return nil, unsupported("nested aggregate functions")
		}
		agg := &aggExpr{fn: name}
		if name == "COUNT" && p.acceptSymbol("*") {
			agg.star = true
		} else {
			p.inAgg++
			arg, err := p.parseExpr()
			p.inAgg--
			if err != nil {
				return nil, err
			}
			agg.arg = arg
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		p.aggs = append(p.aggs, agg)
		return agg, nil
	case "SUBSTRING":
		s, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args := []expr{s}
		if p.acceptKeyword("FROM") || p.acceptSymbol(",") {
			from, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, from)
			if p.acceptKeyword("FOR") || p.acceptSymbol(",") {
				n, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				args = append(args, n)
			}
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		if len(args) < 2 {
			return nil, parseError("SUBSTRING needs a start")
		}
		return &funcExpr{fn: name, args: args}, nil
	case "TRIM":
		mode := "BOTH"
		for _, m := range []string{"LEADING", "TRAILING", "BOTH"} {
			if p.acceptKeyword(m) {
				mode = m
				break
			}
		}
		var chars expr
		if !p.acceptKeyword("FROM") {
			e, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if p.acceptKeyword("FROM") {
				chars = e
			} else {
				if err := p.expectSymbol(")"); err != nil {
					return nil, err
				}
				return &funcExpr{fn: name, args: []expr{e}, mode: mode}, nil
			}
		}
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		args := []expr{e}
		if chars != nil {
			args = append(args, chars)
		}
		return &funcExpr{fn: name, args: args, mode: mode}, nil
	}
	arity := map[string][2]int{"LOWER": {1, 1}, "UPPER": {1, 1}, "CHAR_LENGTH": {1, 1}, "CHARACTER_LENGTH": {1, 1}, "COALESCE": {1, 64}, "NULLIF": {2, 2}}
	ar, ok := arity[name]
	if !ok {
		return nil, unsupported("function %s is not supported", name)
	}
	var args []expr
	if !p.symbol(")") {
		for {
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
			if !p.acceptSymbol(",") {
				break
			}
		}
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	if len(args) < ar[0] || len(args) > ar[1] {
		return nil, parseError("%s takes %d to %d arguments", name, ar[0], ar[1])
	}
	if name == "CHARACTER_LENGTH" {
		name = "CHAR_LENGTH"
	}
	return &funcExpr{fn: name, args: args}, nil
}
