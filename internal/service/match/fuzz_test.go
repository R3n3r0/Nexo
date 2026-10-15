package match_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// within runs fn and fails when it does not return in time: a hang (an
// infinite loop of the Unicode normalization on invalid input) is a
// denial of service, which the fuzzer would otherwise only report with
// the deadline of the whole test.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return within 5s", what)
	}
}

// FuzzNormalize checks the canonical form of the keys shared by the access
// rules, the behaviors and the cache (TAR-04): it terminates, is
// idempotent, resolves every "." and ".." segment, and keeps valid UTF-8
// valid.
func FuzzNormalize(f *testing.F) {
	for _, seed := range []string{
		"a//b", "./a/b", "x/../a/b", "/a/b/", "../../etc/passwd", "a/./b/..", "café", "\xff\xfe", "%2e%2e/x",
		"a/Å/b", ".", "..", "/", "", "\xcc\x81\xcc\x81", "ﬁ/./ﬀ",
	} {
		f.Add(seed)
	}
	ctx := context.Background()
	svc := match.New(match.Deps{})
	raw := match.New(match.Deps{}, match.WithNFC(false))
	f.Fuzz(func(t *testing.T, key string) {
		for _, s := range []match.Service{svc, raw} {
			var once, twice string
			within(t, "Normalize", func() {
				once = s.Normalize(ctx, key)
				twice = s.Normalize(ctx, once)
			})
			if once != twice {
				t.Fatalf("Normalize(%q) = %q, then %q: not idempotent", key, once, twice)
			}
			for _, seg := range strings.Split(once, "/") {
				if seg == "." || seg == ".." {
					t.Fatalf("Normalize(%q) = %q keeps a %q segment", key, once, seg)
				}
			}
			if utf8.ValidString(key) && !utf8.ValidString(once) {
				t.Fatalf("Normalize(%q) = %q: invalid UTF-8", key, once)
			}
			within(t, "Decode", func() { _, _ = s.Decode(ctx, key) })
		}
	})
}

// FuzzCompileAndMatch compiles fuzzed prefix, glob and regex patterns
// (RE2: linear time) and matches fuzzed keys: no panic, invalid patterns
// are domain.ErrInvalidArgument with a *PatternError, a set agrees with its
// matchers, and a prefix pattern matches exactly the keys it prefixes.
func FuzzCompileAndMatch(f *testing.F) {
	seeds := []struct {
		kind    string
		pattern string
		key     string
		ci      bool
	}{
		{"prefix", "public/", "public/a.txt", false},
		{"glob", "**/*.jpg", "a/b/c.jpg", false},
		{"glob", "img/{a,b}/[0-9]?.png", "img/a/1x.png", true},
		{"glob", "[", "x", false},
		{"glob", "a\\", "a", false},
		{"regex", `^private/(?P<user>[^/]+)/.*$`, "private/bob/x", false},
		{"regex", `(a+)+$`, strings.Repeat("a", 64) + "!", false},
		{"regex", `(?i)ÄÖ`, "äö", true},
		{"regex", `(`, "x", false},
		{"other", "x", "x", false},
	}
	for _, s := range seeds {
		f.Add(s.kind, s.pattern, s.key, s.ci)
	}
	ctx := context.Background()
	svc := match.New(match.Deps{})
	f.Fuzz(func(t *testing.T, kind, pattern, key string, ci bool) {
		p := match.Pattern{Type: match.Type(kind), Pattern: pattern, CaseInsensitive: ci}
		var m match.Matcher
		var err error
		within(t, "Compile", func() { m, err = svc.Compile(ctx, p) })
		if err != nil {
			var pe *match.PatternError
			if !errors.Is(err, domain.ErrInvalidArgument) || !errors.As(err, &pe) {
				t.Fatalf("Compile(%+v): error %v is not an invalid argument with a PatternError", p, err)
			}
			return
		}
		norm := svc.Normalize(ctx, key)
		var r match.Result
		within(t, "Match", func() { r = m.Match(norm) })
		if p.Type == match.TypePrefix && !ci && r.Matched != strings.HasPrefix(norm, pattern) {
			t.Fatalf("prefix %q on %q: matched %v", pattern, norm, r.Matched)
		}
		set, err := svc.CompileSet(ctx, []match.Pattern{p, {Type: match.TypePrefix, Pattern: ""}})
		if err != nil {
			t.Fatalf("CompileSet of a valid pattern: %v", err)
		}
		var hits []int
		set.Match(norm, nil, func(i int, _ match.Result) bool {
			hits = append(hits, i)
			return true
		})
		inSet := len(hits) > 0 && hits[0] == 0
		if inSet != r.Matched {
			t.Fatalf("pattern %+v on %q: matcher %v, set %v", p, norm, r.Matched, hits)
		}
	})
}
