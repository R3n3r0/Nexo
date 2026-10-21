package s3select

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// kind is the type of a value.
type kind int

const (
	kNull kind = iota
	kMissing
	kBool
	kInt
	kFloat
	kString
	// kDoc is a JSON object or array (a path ending on one).
	kDoc
)

// value is the result of an expression.
type value struct {
	k   kind
	b   bool
	i   int64
	f   float64
	s   string
	doc any
}

func nullValue() value    { return value{k: kNull} }
func missingValue() value { return value{k: kMissing} }

func boolValue(b bool) value     { return value{k: kBool, b: b} }
func intValue(i int64) value     { return value{k: kInt, i: i} }
func floatValue(f float64) value { return value{k: kFloat, f: f} }
func stringValue(s string) value { return value{k: kString, s: s} }

// absent reports a null or missing value.
func (v value) absent() bool { return v.k == kNull || v.k == kMissing }

// number returns the numeric value of v: a number, or a string that
// parses as one (CSV fields). isInt tells an integer.
func (v value) number() (f float64, i int64, isInt, ok bool) {
	switch v.k {
	case kInt:
		return float64(v.i), v.i, true, true
	case kFloat:
		return v.f, 0, false, true
	case kString:
		s := strings.TrimSpace(v.s)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return float64(n), n, true, true
		}
		if x, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(x, 0) && !math.IsNaN(x) {
			return x, 0, false, true
		}
	}
	return 0, 0, false, false
}

// text returns the string form of v (CSV output, string functions).
func (v value) text() string {
	switch v.k {
	case kBool:
		return strconv.FormatBool(v.b)
	case kInt:
		return strconv.FormatInt(v.i, 10)
	case kFloat:
		return strconv.FormatFloat(v.f, 'f', -1, 64)
	case kString:
		return v.s
	case kDoc:
		b, err := json.Marshal(v.doc)
		if err != nil {
			return ""
		}
		return string(b)
	}
	return ""
}

// truthy reports a true boolean (a WHERE keeps the records it is true for).
func (v value) truthy() bool {
	switch v.k {
	case kBool:
		return v.b
	case kString:
		return strings.EqualFold(v.s, "true")
	}
	return false
}

// fromJSON converts a decoded JSON value.
func fromJSON(x any) value {
	switch t := x.(type) {
	case nil:
		return nullValue()
	case bool:
		return boolValue(t)
	case string:
		return stringValue(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return intValue(i)
		}
		f, err := t.Float64()
		if err != nil {
			return stringValue(t.String())
		}
		return floatValue(f)
	case float64:
		return floatValue(t)
	default:
		return value{k: kDoc, doc: t}
	}
}

// toJSON returns the JSON form of v for the JSON output.
func (v value) toJSON() any {
	switch v.k {
	case kBool:
		return v.b
	case kInt:
		return v.i
	case kFloat:
		return v.f
	case kString:
		return v.s
	case kDoc:
		return v.doc
	}
	return nil
}

// record is an input record: the fields of a CSV line or a JSON value.
type record struct {
	fields []string
	// columns maps the header names to their index (CSV with USE).
	columns map[string]int
	doc     any
	// raw is the JSON text of the record (SELECT * of JSON).
	raw []byte
}

// evalCtx is the context of the evaluation of a record.
type evalCtx struct {
	rec   *record
	alias string
}

type expr interface {
	eval(c *evalCtx) (value, error)
}

type literal struct{ v value }

func (l *literal) eval(*evalCtx) (value, error) { return l.v, nil }

// pathStep is a step of a path: a name or an index.
type pathStep struct {
	name    string
	quoted  bool
	isIndex bool
	index   int
}

type pathExpr struct{ steps []pathStep }

func (p *pathExpr) eval(c *evalCtx) (value, error) {
	steps := p.steps
	if len(steps) > 1 || (c.alias != "" && len(steps) == 1) {
		if first := steps[0]; !first.isIndex && ((c.alias != "" && strings.EqualFold(first.name, c.alias)) || strings.EqualFold(first.name, "S3Object")) {
			steps = steps[1:]
		}
	}
	if len(steps) == 0 {
		if c.rec.doc != nil {
			return fromJSON(c.rec.doc), nil
		}
		return missingValue(), nil
	}
	if c.rec.doc == nil {
		return c.rec.field(steps)
	}
	return walk(c.rec.doc, steps), nil
}

// field resolves a path in a CSV record: _N or a column name.
func (r *record) field(steps []pathStep) (value, error) {
	if len(steps) != 1 || steps[0].isIndex {
		return missingValue(), nil
	}
	name := steps[0].name
	if strings.HasPrefix(name, "_") {
		if n, err := strconv.Atoi(name[1:]); err == nil && n >= 1 {
			if n > len(r.fields) {
				return missingValue(), nil
			}
			return stringValue(r.fields[n-1]), nil
		}
	}
	if i, ok := r.columns[name]; ok {
		return r.fieldAt(i), nil
	}
	if !steps[0].quoted {
		for col, i := range r.columns {
			if strings.EqualFold(col, name) {
				return r.fieldAt(i), nil
			}
		}
	}
	return missingValue(), nil
}

func (r *record) fieldAt(i int) value {
	if i >= len(r.fields) {
		return missingValue()
	}
	return stringValue(r.fields[i])
}

// walk resolves a path in a JSON value.
func walk(doc any, steps []pathStep) value {
	cur := doc
	for _, s := range steps {
		switch t := cur.(type) {
		case map[string]any:
			if s.isIndex {
				return missingValue()
			}
			v, ok := t[s.name]
			if !ok && !s.quoted {
				for k, x := range t {
					if strings.EqualFold(k, s.name) {
						v, ok = x, true
						break
					}
				}
			}
			if !ok {
				return missingValue()
			}
			cur = v
		case []any:
			if !s.isIndex || s.index >= len(t) {
				return missingValue()
			}
			cur = t[s.index]
		default:
			return missingValue()
		}
	}
	return fromJSON(cur)
}

type unaryExpr struct {
	op string
	e  expr
}

func (u *unaryExpr) eval(c *evalCtx) (value, error) {
	v, err := u.e.eval(c)
	if err != nil {
		return value{}, err
	}
	switch u.op {
	case "NOT":
		if v.absent() {
			return nullValue(), nil
		}
		if v.k != kBool && (v.k != kString || (!strings.EqualFold(v.s, "true") && !strings.EqualFold(v.s, "false"))) {
			return value{}, evalError("EvaluatorInvalidArguments", "NOT applies to a boolean")
		}
		return boolValue(!v.truthy()), nil
	default: // "-"
		if v.absent() {
			return nullValue(), nil
		}
		f, i, isInt, ok := v.number()
		if !ok {
			return value{}, evalError("EvaluatorInvalidArguments", "- applies to a number")
		}
		if isInt {
			return intValue(-i), nil
		}
		return floatValue(-f), nil
	}
}

type binaryExpr struct {
	op   string
	l, r expr
}

func (b *binaryExpr) eval(c *evalCtx) (value, error) {
	l, err := b.l.eval(c)
	if err != nil {
		return value{}, err
	}
	switch b.op {
	case "AND":
		if l.k == kBool && !l.b {
			return boolValue(false), nil
		}
		r, err := b.r.eval(c)
		if err != nil {
			return value{}, err
		}
		if r.k == kBool && !r.b {
			return boolValue(false), nil
		}
		if l.absent() || r.absent() {
			return nullValue(), nil
		}
		return boolValue(l.truthy() && r.truthy()), nil
	case "OR":
		if l.truthy() {
			return boolValue(true), nil
		}
		r, err := b.r.eval(c)
		if err != nil {
			return value{}, err
		}
		if r.truthy() {
			return boolValue(true), nil
		}
		if l.absent() || r.absent() {
			return nullValue(), nil
		}
		return boolValue(false), nil
	}
	r, err := b.r.eval(c)
	if err != nil {
		return value{}, err
	}
	switch b.op {
	case "||":
		if l.absent() || r.absent() {
			return nullValue(), nil
		}
		return stringValue(l.text() + r.text()), nil
	case "+", "-", "*", "/", "%":
		return arith(b.op, l, r)
	}
	return compare(b.op, l, r)
}

// arith evaluates an arithmetic operation.
func arith(op string, l, r value) (value, error) {
	if l.absent() || r.absent() {
		return nullValue(), nil
	}
	lf, li, lInt, lok := l.number()
	rf, ri, rInt, rok := r.number()
	if !lok || !rok {
		return value{}, evalError("EvaluatorInvalidArguments", "%s applies to numbers", op)
	}
	if lInt && rInt {
		switch op {
		case "+":
			return intValue(li + ri), nil
		case "-":
			return intValue(li - ri), nil
		case "*":
			return intValue(li * ri), nil
		case "/":
			if ri == 0 {
				return value{}, evalError("DivisionByZero", "division by zero")
			}
			if li%ri == 0 {
				return intValue(li / ri), nil
			}
			return floatValue(float64(li) / float64(ri)), nil
		case "%":
			if ri == 0 {
				return value{}, evalError("DivisionByZero", "division by zero")
			}
			return intValue(li % ri), nil
		}
	}
	switch op {
	case "+":
		return floatValue(lf + rf), nil
	case "-":
		return floatValue(lf - rf), nil
	case "*":
		return floatValue(lf * rf), nil
	case "/":
		if rf == 0 {
			return value{}, evalError("DivisionByZero", "division by zero")
		}
		return floatValue(lf / rf), nil
	default:
		if rf == 0 {
			return value{}, evalError("DivisionByZero", "division by zero")
		}
		return floatValue(math.Mod(lf, rf)), nil
	}
}

// order compares two values: -1, 0 or 1, ok false when they do not
// compare. A number compares with a string holding a number.
func order(l, r value) (int, bool) {
	if l.k == kBool || r.k == kBool {
		if l.k == kBool && r.k == kBool {
			switch {
			case l.b == r.b:
				return 0, true
			case !l.b:
				return -1, true
			default:
				return 1, true
			}
		}
		lb, rb := l, r
		if lb.k == kString {
			lb = boolValue(strings.EqualFold(l.s, "true"))
		}
		if rb.k == kString {
			rb = boolValue(strings.EqualFold(r.s, "true"))
		}
		if lb.k == kBool && rb.k == kBool {
			return order(lb, rb)
		}
		return 0, false
	}
	numeric := l.k == kInt || l.k == kFloat || r.k == kInt || r.k == kFloat
	if numeric {
		lf, li, lInt, lok := l.number()
		rf, ri, rInt, rok := r.number()
		if !lok || !rok {
			return 0, false
		}
		if lInt && rInt {
			switch {
			case li < ri:
				return -1, true
			case li > ri:
				return 1, true
			}
			return 0, true
		}
		switch {
		case lf < rf:
			return -1, true
		case lf > rf:
			return 1, true
		}
		return 0, true
	}
	if l.k == kString && r.k == kString {
		return strings.Compare(l.s, r.s), true
	}
	if l.k == kDoc || r.k == kDoc {
		if l.text() == r.text() {
			return 0, true
		}
	}
	return 0, false
}

// compare evaluates a comparison.
func compare(op string, l, r value) (value, error) {
	if l.absent() || r.absent() {
		return nullValue(), nil
	}
	c, ok := order(l, r)
	if !ok {
		return boolValue(op == "!="), nil
	}
	switch op {
	case "=":
		return boolValue(c == 0), nil
	case "!=":
		return boolValue(c != 0), nil
	case "<":
		return boolValue(c < 0), nil
	case "<=":
		return boolValue(c <= 0), nil
	case ">":
		return boolValue(c > 0), nil
	default:
		return boolValue(c >= 0), nil
	}
}

type isExpr struct {
	e       expr
	missing bool
}

func (x *isExpr) eval(c *evalCtx) (value, error) {
	v, err := x.e.eval(c)
	if err != nil {
		return value{}, err
	}
	if x.missing {
		return boolValue(v.k == kMissing), nil
	}
	return boolValue(v.absent()), nil
}

type inExpr struct {
	e    expr
	list []expr
}

func (x *inExpr) eval(c *evalCtx) (value, error) {
	v, err := x.e.eval(c)
	if err != nil {
		return value{}, err
	}
	if v.absent() {
		return nullValue(), nil
	}
	for _, e := range x.list {
		w, err := e.eval(c)
		if err != nil {
			return value{}, err
		}
		if eq, err := compare("=", v, w); err == nil && eq.k == kBool && eq.b {
			return boolValue(true), nil
		}
	}
	return boolValue(false), nil
}

type likeExpr struct {
	e, pattern, escape expr
}

func (x *likeExpr) eval(c *evalCtx) (value, error) {
	v, err := x.e.eval(c)
	if err != nil {
		return value{}, err
	}
	p, err := x.pattern.eval(c)
	if err != nil {
		return value{}, err
	}
	if v.absent() || p.absent() {
		return nullValue(), nil
	}
	esc := rune(0)
	if x.escape != nil {
		e, err := x.escape.eval(c)
		if err != nil {
			return value{}, err
		}
		if utf8.RuneCountInString(e.text()) != 1 {
			return value{}, evalError("InvalidArgument", "the ESCAPE of LIKE is one character")
		}
		esc, _ = utf8.DecodeRuneInString(e.text())
	}
	return boolValue(like([]rune(v.text()), []rune(p.text()), esc)), nil
}

// likeElem is an element of a compiled LIKE pattern: a literal rune, _
// (one rune) or % (any run).
type likeElem struct {
	r          rune
	one, multi bool
}

// like matches s against a LIKE pattern (% any run, _ any character, esc
// quoting the next one). The greedy two-pointer match backtracks only to
// the last %: linear in practice, O(len(s)·len(p)) at worst.
func like(s, p []rune, esc rune) bool {
	elems := make([]likeElem, 0, len(p))
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case esc != 0 && c == esc && i+1 < len(p):
			i++
			elems = append(elems, likeElem{r: p[i]})
		case c == '%':
			if n := len(elems); n == 0 || !elems[n-1].multi {
				elems = append(elems, likeElem{multi: true})
			}
		case c == '_':
			elems = append(elems, likeElem{one: true})
		default:
			elems = append(elems, likeElem{r: c})
		}
	}
	si, pi := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(elems) && !elems[pi].multi && (elems[pi].one || elems[pi].r == s[si]):
			si++
			pi++
		case pi < len(elems) && elems[pi].multi:
			star, mark = pi, si
			pi++
		case star >= 0:
			mark++
			si, pi = mark, star+1
		default:
			return false
		}
	}
	for pi < len(elems) && elems[pi].multi {
		pi++
	}
	return pi == len(elems)
}

type castExpr struct {
	e   expr
	typ string
}

func (x *castExpr) eval(c *evalCtx) (value, error) {
	v, err := x.e.eval(c)
	if err != nil || v.absent() {
		return v, err
	}
	if x.typ != "STRING" && v.k == kString && strings.TrimSpace(v.s) == "" {
		// An empty CSV field has no value.
		return nullValue(), nil
	}
	switch x.typ {
	case "INT":
		f, i, isInt, ok := v.number()
		if !ok {
			return value{}, evalError("CastFailed", "cannot cast %q to INT", v.text())
		}
		if isInt {
			return intValue(i), nil
		}
		return intValue(int64(f)), nil
	case "FLOAT":
		f, _, _, ok := v.number()
		if !ok {
			return value{}, evalError("CastFailed", "cannot cast %q to FLOAT", v.text())
		}
		return floatValue(f), nil
	case "BOOL":
		switch {
		case v.k == kBool:
			return v, nil
		case strings.EqualFold(v.text(), "true"):
			return boolValue(true), nil
		case strings.EqualFold(v.text(), "false"):
			return boolValue(false), nil
		}
		return value{}, evalError("CastFailed", "cannot cast %q to BOOL", v.text())
	default:
		return stringValue(v.text()), nil
	}
}

type funcExpr struct {
	fn   string
	args []expr
	mode string
}

func (x *funcExpr) eval(c *evalCtx) (value, error) {
	vals := make([]value, len(x.args))
	for i, a := range x.args {
		v, err := a.eval(c)
		if err != nil {
			return value{}, err
		}
		vals[i] = v
	}
	switch x.fn {
	case "COALESCE":
		for _, v := range vals {
			if !v.absent() {
				return v, nil
			}
		}
		return nullValue(), nil
	case "NULLIF":
		if eq, _ := compare("=", vals[0], vals[1]); eq.k == kBool && eq.b {
			return nullValue(), nil
		}
		return vals[0], nil
	}
	if vals[0].absent() {
		return nullValue(), nil
	}
	s := vals[0].text()
	switch x.fn {
	case "LOWER":
		return stringValue(strings.ToLower(s)), nil
	case "UPPER":
		return stringValue(strings.ToUpper(s)), nil
	case "CHAR_LENGTH":
		return intValue(int64(utf8.RuneCountInString(s))), nil
	case "TRIM":
		cut := " "
		if len(vals) > 1 && !vals[1].absent() {
			cut = vals[1].text()
		}
		switch x.mode {
		case "LEADING":
			return stringValue(strings.TrimLeft(s, cut)), nil
		case "TRAILING":
			return stringValue(strings.TrimRight(s, cut)), nil
		}
		return stringValue(strings.Trim(s, cut)), nil
	default: // SUBSTRING, 1-based
		rs := []rune(s)
		_, from, isInt, ok := vals[1].number()
		if !ok || !isInt {
			return value{}, evalError("InvalidArgument", "the start of SUBSTRING is an integer")
		}
		end := int64(len(rs)) + 1
		if len(vals) > 2 {
			_, n, isInt, ok := vals[2].number()
			if !ok || !isInt || n < 0 {
				return value{}, evalError("InvalidArgument", "the length of SUBSTRING is a non-negative integer")
			}
			end = from + n
		}
		from = max(from, 1)
		end = min(end, int64(len(rs))+1)
		if from >= end {
			return stringValue(""), nil
		}
		return stringValue(string(rs[from-1 : end-1])), nil
	}
}

// aggExpr is an aggregate function; its value is the result accumulated
// over the records (accumulate), read once they are all seen.
type aggExpr struct {
	fn    string
	arg   expr
	star  bool
	count int64
	sumF  float64
	sumI  int64
	isInt bool
	best  value
	seen  bool
}

func (a *aggExpr) accumulate(c *evalCtx) error {
	if a.star {
		a.count++
		return nil
	}
	v, err := a.arg.eval(c)
	if err != nil {
		return err
	}
	if v.absent() || (a.fn != "COUNT" && v.k == kString && strings.TrimSpace(v.s) == "") {
		// The empty CSV fields have no value to sum or compare.
		return nil
	}
	switch a.fn {
	case "COUNT":
		a.count++
	case "SUM", "AVG":
		f, i, isInt, ok := v.number()
		if !ok {
			return evalError("EvaluatorInvalidArguments", "%s applies to numbers, found %q", a.fn, v.text())
		}
		if !a.seen {
			a.isInt = true
		}
		a.seen = true
		a.count++
		a.isInt = a.isInt && isInt
		a.sumF += f
		a.sumI += i
	default: // MIN, MAX
		if v.k == kString {
			// Numeric CSV fields compare as numbers.
			if f, i, isInt, ok := v.number(); ok {
				v = floatValue(f)
				if isInt {
					v = intValue(i)
				}
			}
		}
		if !a.seen {
			a.best, a.seen = v, true
			return nil
		}
		if c, ok := order(v, a.best); ok && ((a.fn == "MIN" && c < 0) || (a.fn == "MAX" && c > 0)) {
			a.best = v
		}
	}
	return nil
}

func (a *aggExpr) eval(*evalCtx) (value, error) {
	switch a.fn {
	case "COUNT":
		return intValue(a.count), nil
	case "SUM":
		if !a.seen {
			return nullValue(), nil
		}
		if a.isInt {
			return intValue(a.sumI), nil
		}
		return floatValue(a.sumF), nil
	case "AVG":
		if a.count == 0 {
			return nullValue(), nil
		}
		return floatValue(a.sumF / float64(a.count)), nil
	default:
		if !a.seen {
			return nullValue(), nil
		}
		return a.best, nil
	}
}
