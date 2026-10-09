package match

import (
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// maxAlternatives bounds the literals of a requirement.
const maxAlternatives = 16

// requirement is a prefilter of an expression: every match contains one
// of the literals (folded with foldString when fold is set). An empty
// requirement constrains nothing. It never excludes a key the expression
// would match: it only spares the evaluation of the expression on the
// keys that cannot match.
type requirement struct {
	lits []string
	fold bool
}

// possible reports whether key may match; folded is the folded key when
// the caller already computed it ("" to compute it here).
func (q requirement) possible(key, folded string) bool {
	if len(q.lits) == 0 {
		return true
	}
	if q.fold {
		if folded == "" {
			folded = foldString(key)
		}
		key = folded
	}
	for _, l := range q.lits {
		if strings.Contains(key, l) {
			return true
		}
	}
	return false
}

// minLen is the length of the shortest literal (the selectivity).
func (q requirement) minLen() int {
	n := -1
	for _, l := range q.lits {
		if n < 0 || len(l) < n {
			n = len(l)
		}
	}
	return n
}

// prefilterOf computes the requirement of an expression.
func prefilterOf(expr string) requirement {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return requirement{}
	}
	q, ok := required(re.Simplify())
	if !ok {
		return requirement{}
	}
	if q.fold {
		for i, l := range q.lits {
			q.lits[i] = foldString(l)
		}
	}
	return q
}

// required returns a requirement every match of re satisfies; ok is false
// when there is none.
func required(re *syntax.Regexp) (requirement, bool) {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return requirement{}, false
		}
		lit := string(re.Rune)
		if !utf8.ValidString(lit) {
			return requirement{}, false
		}
		return requirement{lits: []string{lit}, fold: re.Flags&syntax.FoldCase != 0}, true
	case syntax.OpCapture, syntax.OpPlus:
		return required(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return required(re.Sub[0])
		}
		return requirement{}, false
	case syntax.OpConcat:
		var best requirement
		found := false
		for _, sub := range re.Sub {
			q, ok := required(sub)
			if !ok {
				continue
			}
			if !found || q.minLen() > best.minLen() || (q.minLen() == best.minLen() && len(q.lits) < len(best.lits)) {
				best, found = q, true
			}
		}
		return best, found
	case syntax.OpAlternate:
		var out requirement
		for _, sub := range re.Sub {
			q, ok := required(sub)
			if !ok {
				return requirement{}, false
			}
			out.lits = append(out.lits, q.lits...)
			out.fold = out.fold || q.fold
			if len(out.lits) > maxAlternatives {
				return requirement{}, false
			}
		}
		return out, len(out.lits) > 0
	}
	return requirement{}, false
}
