package topology

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ellipsisRE matches one ellipsis token: an opening brace, a start value, three
// dots, an end value and a closing brace. Braces are not allowed inside a token.
var ellipsisRE = regexp.MustCompile(`\{([^{}]*)\.\.\.([^{}]*)\}`)

// hasEllipsis reports whether arg contains at least one ellipsis token.
func hasEllipsis(arg string) bool { return ellipsisRE.MatchString(arg) }

// expandArg expands every ellipsis token of arg and returns the cartesian
// product of the expansions. The first token varies fastest, so that
// "http://node{1...2}:9000/data{1...2}" yields node1/data1, node2/data1,
// node1/data2, node2/data2: consecutive endpoints live on different nodes.
// An argument without tokens is returned unchanged. Any stray brace is an error.
func expandArg(arg string) ([]string, error) {
	if arg == "" {
		return nil, fmt.Errorf("%w: empty argument", ErrInvalidEndpoint)
	}
	locs := ellipsisRE.FindAllStringSubmatchIndex(arg, -1)

	literals := make([]string, 0, len(locs)+1)
	ranges := make([][]string, 0, len(locs))
	last := 0
	for _, loc := range locs {
		literals = append(literals, arg[last:loc[0]])
		start, end := arg[loc[2]:loc[3]], arg[loc[4]:loc[5]]
		values, err := expandRange(start, end)
		if err != nil {
			if errors.Is(err, ErrTooManyEndpoints) {
				return nil, fmt.Errorf("%w: %q: %w", ErrInvalidEllipsis, arg, err)
			}
			return nil, fmt.Errorf("%w in %q: %v", ErrInvalidEllipsis, arg, err)
		}
		ranges = append(ranges, values)
		last = loc[1]
	}
	literals = append(literals, arg[last:])

	for _, lit := range literals {
		if strings.ContainsAny(lit, "{}") {
			return nil, fmt.Errorf("%w in %q: unbalanced or malformed braces (expected {start...end})", ErrInvalidEllipsis, arg)
		}
	}
	if len(ranges) == 0 {
		return []string{arg}, nil
	}

	total := 1
	for _, r := range ranges {
		total *= len(r)
		if total > maxExpansion {
			return nil, fmt.Errorf("%w: %q expands to more than %d endpoints", ErrTooManyEndpoints, arg, maxExpansion)
		}
	}

	out := make([]string, 0, total)
	idx := make([]int, len(ranges))
	var sb strings.Builder
	for {
		sb.Reset()
		for i, r := range ranges {
			sb.WriteString(literals[i])
			sb.WriteString(r[idx[i]])
		}
		sb.WriteString(literals[len(ranges)])
		out = append(out, sb.String())

		// Odometer increment with the first token varying fastest.
		i := 0
		for ; i < len(idx); i++ {
			idx[i]++
			if idx[i] < len(ranges[i]) {
				break
			}
			idx[i] = 0
		}
		if i == len(idx) {
			return out, nil
		}
	}
}

// expandRange expands the values of a single ellipsis token. Numeric ranges
// ({1...4}) are zero padded to the width of the start value when it has a
// leading zero ({01...16} -> 01..16). Alphabetic ranges are single letters of
// the same case ({a...d}). Start must not exceed end.
func expandRange(start, end string) ([]string, error) {
	switch {
	case start == "" || end == "":
		return nil, fmt.Errorf("empty range bound in {%s...%s}", start, end)
	case isDigits(start) && isDigits(end):
		return expandNumericRange(start, end)
	case isLetter(start) && isLetter(end):
		return expandAlphaRange(start[0], end[0])
	case (isDigits(start) && isLetter(end)) || (isLetter(start) && isDigits(end)):
		return nil, fmt.Errorf("mixed numeric and alphabetic bounds in {%s...%s}", start, end)
	default:
		return nil, fmt.Errorf("unsupported range {%s...%s} (use digits or a single letter per bound)", start, end)
	}
}

func expandNumericRange(start, end string) ([]string, error) {
	lo, err := strconv.ParseUint(start, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid numeric bound %q", start)
	}
	hi, err := strconv.ParseUint(end, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid numeric bound %q", end)
	}
	if lo > hi {
		return nil, fmt.Errorf("range start %s is greater than end %s", start, end)
	}
	if hi-lo+1 > maxExpansion {
		return nil, fmt.Errorf("%w: range {%s...%s} expands to more than %d values", ErrTooManyEndpoints, start, end, maxExpansion)
	}
	width := 0
	if len(start) > 1 && start[0] == '0' {
		width = len(start)
	}
	out := make([]string, 0, hi-lo+1)
	for v := lo; v <= hi; v++ {
		if width > 0 {
			out = append(out, fmt.Sprintf("%0*d", width, v))
		} else {
			out = append(out, strconv.FormatUint(v, 10))
		}
	}
	return out, nil
}

func expandAlphaRange(lo, hi byte) ([]string, error) {
	if isLower(lo) != isLower(hi) {
		return nil, fmt.Errorf("mixed letter case in {%c...%c}", lo, hi)
	}
	if lo > hi {
		return nil, fmt.Errorf("range start %c is greater than end %c", lo, hi)
	}
	out := make([]string, 0, hi-lo+1)
	for c := lo; c <= hi; c++ {
		out = append(out, string(c))
	}
	return out, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isLetter(s string) bool {
	return len(s) == 1 && (isLower(s[0]) || isUpper(s[0]))
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
