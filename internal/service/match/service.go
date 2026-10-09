// Package match compiles and evaluates the key patterns of the access
// rules (docs/10 §10.1) and of the distribution behaviors (§10.2): literal
// prefixes, globs and RE2 regular expressions, matched against keys
// normalized so that an encoding never bypasses a rule.
//
// # Patterns
//
//   - prefix: a literal prefix ("public/"; "" matches every key). The
//     prefixes of a Set are looked up in a trie.
//   - glob: "*" matches any run of characters within a path segment (never
//     '/'), "**" any run of characters across segments ("**/" at the start
//     of a segment also matches no segment at all: "docs/**/*.pdf" matches
//     "docs/a.pdf"), "?" one character but '/', "[abc]", "[a-z]" and
//     "[!abc]" (or "[^abc]") one character of the class (a negated class
//     never matches '/'), "{a,b}" one of the alternatives (globs
//     themselves, nesting allowed), "\" escapes the next character. A glob
//     compiles into an anchored RE2 expression.
//   - regex: an RE2 expression (the Go regexp syntax: no backreference, no
//     lookaround), unanchored unless it uses ^ and $, at most
//     MaxPatternLength characters. Its named groups ((?P<name>...) or
//     (?<name>...)) are captured. RE2 runs in time linear in the key: a
//     pathological expression such as (a+)+$ cannot cause a ReDoS.
//
// CaseInsensitive matches with Unicode simple case folding (the (?i) of
// RE2) for every type. A pattern that does not compile is
// domain.ErrInvalidArgument with a *PatternError locating the problem.
//
// # Keys
//
// The keys are matched in their normalized form (Normalize): the Unicode
// NFC form (configurable with WithNFC) with the empty, "." and ".."
// segments resolved, so that a pattern written for "a/b" also covers
// "a//b", "./a/b", "x/../a/b" and the NFD spelling of the same name. The
// patterns themselves are NFC-normalized when compiled. Normalize never
// decodes: the S3 keys reach the policy engine decoded once, by the URL
// parser of the transport, so "%2F" or "%252F" left in a key are literal
// characters of another object. Decode is the single URL decoding of a raw
// path (the distributions serve raw URL paths), followed by Normalize.
package match

import (
	"context"
)

// Type is the type of a pattern.
type Type string

// Pattern types.
const (
	TypePrefix Type = "prefix"
	TypeGlob   Type = "glob"
	TypeRegex  Type = "regex"
)

// MaxPatternLength is the default maximum length of a pattern, in
// characters (WithMaxPatternLength).
const MaxPatternLength = 1024

// Pattern is a key pattern, as configured.
type Pattern struct {
	Type    Type   `json:"type" yaml:"type"`
	Pattern string `json:"pattern" yaml:"pattern"`
	// CaseInsensitive matches with Unicode simple case folding.
	CaseInsensitive bool `json:"caseInsensitive,omitempty" yaml:"caseInsensitive,omitempty"`
}

// Deps are the dependencies of the matcher service (none: the struct
// keeps the constructor shape of every service).
type Deps struct{}

// Service compiles patterns and normalizes keys.
type Service interface {
	// Compile validates and compiles a pattern. An invalid pattern (an
	// empty glob or regex, too long, unknown type, malformed glob or regex) is
	// domain.ErrInvalidArgument with a *PatternError.
	Compile(ctx context.Context, p Pattern) (Matcher, error)
	// CompileSet compiles an ordered list of patterns evaluated together:
	// the literal prefixes of the patterns (prefix patterns, and the
	// anchored literal start of globs and regexes) are indexed in a trie
	// so that a key is matched only against the patterns it may match.
	// The error of an invalid pattern names its index (*PatternError).
	CompileSet(ctx context.Context, patterns []Pattern) (Set, error)
	// Normalize returns the canonical form of a decoded key: NFC (unless
	// disabled) with the empty, "." and ".." segments resolved (a leading
	// and a trailing '/' are kept).
	Normalize(ctx context.Context, key string) string
	// Decode URL-decodes a raw path once ("+" is a literal plus) and
	// normalizes it. A malformed escape is domain.ErrInvalidArgument.
	Decode(ctx context.Context, raw string) (string, error)
}

// Matcher is a compiled pattern. Its methods are pure (no I/O, they never
// block): the documented exception to the context rule.
type Matcher interface {
	// Pattern returns the pattern as configured.
	Pattern() Pattern
	// Match matches a normalized key (Service.Normalize).
	Match(key string) Result
	// Groups lists the named groups of a regex, in order of appearance.
	Groups() []string
	// Expr returns the RE2 expression the pattern is evaluated with (the
	// equivalent anchored expression of a prefix).
	Expr() string
}

// Set is an ordered list of compiled patterns (CompileSet). Its methods
// are pure, like those of Matcher.
type Set interface {
	// Len returns the number of patterns.
	Len() int
	// Matcher returns the compiled pattern of index i.
	Matcher(i int) Matcher
	// Match calls yield for every pattern matching the normalized key, in
	// ascending order of index, until yield returns false. keep, when not
	// nil, is asked first for every pattern the trie leaves as a
	// candidate: false skips it without evaluating it (the cheaper checks
	// of the caller).
	Match(key string, keep func(i int) bool, yield func(i int, r Result) bool)
}

// Result is the outcome of a match.
type Result struct {
	Matched bool
	// Captures are the named groups of a regex that matched (nil when the
	// pattern has none). A group that did not participate is "".
	Captures map[string]string
}

// PatternError locates the problem of an invalid pattern. It unwraps to
// domain.ErrInvalidArgument.
type PatternError struct {
	// Index is the 0-based position of the pattern in a Set, -1 for a
	// single pattern.
	Index int
	Type  Type
	// Pattern is the pattern as configured.
	Pattern string
	// Position is the 1-based position, in characters, of the problem in
	// the pattern; 0 when it concerns the whole pattern.
	Position int
	// Msg describes the problem.
	Msg string
}
