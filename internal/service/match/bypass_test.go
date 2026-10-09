package match_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/match"
)

// TestBypassAttempts is TAR-03 at the level of the matcher: the keys that
// name a protected object through %2F, double encoding, "./", "//", ".."
// or another Unicode normalization form still match the rule protecting
// it (decoded once for raw paths, normalized for every key), and the
// case-insensitive rules cover every letter case.
func TestBypassAttempts(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	rules := []match.Pattern{
		glob("**/private/**"),
		prefix("secret/"),
		regex(`^admin/.*\.cfg$`),
		ci(glob("**/Confidential/**")),
		prefix("città/"),
		glob("café/*"),
	}
	set, err := svc.CompileSet(ctx, rules)
	require.NoError(t, err)
	protected := func(key string) bool {
		hit := false
		set.Match(key, nil, func(int, match.Result) bool { hit = true; return false })
		return hit
	}

	// Raw request paths: decoded once, then normalized.
	raw := []string{
		"pub%2Fprivate%2Fx",
		"pub/%2e/private/x",
		"%2e%2e/private/x",
		"pub/%2e%2e/private/x",
		"secret%2Fkey",
		"%73ecret/key",
		".%2Fsecret/key",
		"admin%2Fsite.cfg",
		"a//private//b",
		"docs/%43onfidential/x",
		"citta%CC%80/roma",
		"cafe%CC%81/menu",
	}
	for _, r := range raw {
		key, err := svc.Decode(ctx, r)
		require.NoError(t, err, r)
		assert.True(t, protected(key), "raw %q decoded to %q escapes the rules", r, key)
	}

	// Decoded keys (as the S3 transport hands them over).
	decoded := []string{
		"./private/x",
		"a/./private/x",
		"a//private/x",
		"x/../private/x",
		"x/y/../../private/x",
		"secret//key",
		"./secret/key",
		"tmp/../secret/key",
		"admin//site.cfg",
		"./admin/site.cfg",
		"a/CONFIDENTIAL/b",
		"a/confidential/b",
		"a/ConFidEntial/b",
		"città/roma",        // NFD of città
		"café/menu",         // NFD of café
		"x/../../secret/key", // ".." above the root
	}
	for _, k := range decoded {
		assert.True(t, protected(svc.Normalize(ctx, k)), "key %q escapes the rules", k)
	}

	// A double-encoded separator is a literal "%2F" after the single
	// decoding: it names another object, which the rules leave alone, and
	// decoding never runs twice.
	key, err := svc.Decode(ctx, "pub%252Fprivate%252Fx")
	require.NoError(t, err)
	assert.Equal(t, "pub%2Fprivate%2Fx", key)
	assert.False(t, protected(key))
	again, err := svc.Decode(ctx, "%252e%252e/secret/key")
	require.NoError(t, err)
	assert.Equal(t, "%2e%2e/secret/key", again)

	// A case-sensitive rule leaves the other letter cases alone: they are
	// other objects (S3 keys are case-sensitive).
	assert.False(t, protected(svc.Normalize(ctx, "SECRET/key")))
	assert.False(t, protected(svc.Normalize(ctx, "a/PRIVATE/x")))
}

// TestNFCDisabled checks the configurable normalization: without NFC the
// NFD spelling is another key.
func TestNFCDisabled(t *testing.T) {
	svc := match.New(match.Deps{}, match.WithNFC(false))
	ctx := context.Background()
	m, err := svc.Compile(ctx, prefix("città/"))
	require.NoError(t, err)
	assert.True(t, m.Match(svc.Normalize(ctx, "città/a")).Matched)
	assert.False(t, m.Match(svc.Normalize(ctx, "città/a")).Matched)
	// The segments are resolved anyway.
	assert.True(t, m.Match(svc.Normalize(ctx, "x/../città/a")).Matched)
}

// TestPathologicalRegexes is TAR-04: expressions that make backtracking
// engines explode run in linear time with RE2 (< 1 ms on 1 KiB).
func TestPathologicalRegexes(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	input := strings.Repeat("a", 1024) + "!"
	patterns := []string{
		`(a+)+$`,
		`^(a+)+$`,
		`(a|a)+$`,
		`(a|aa)+$`,
		`(a*)*b`,
		`^(([a-z])+.)+[A-Z]([a-z])+$`,
		`(.*a){20}`,
		`^(\w+\s?)*$`,
	}
	for _, p := range patterns {
		m, err := svc.Compile(ctx, regex(p))
		require.NoError(t, err, p)
		m.Match(input) // warm up
		const runs = 20
		start := time.Now()
		for i := 0; i < runs; i++ {
			m.Match(input)
		}
		per := time.Since(start) / runs
		limit := time.Millisecond
		if raceDetector {
			limit *= 10
		}
		assert.Less(t, per, limit, "%s took %s on 1 KiB", p, per)
	}
}

// TestRegexTimeIsLinear doubles the input of a pathological expression:
// the time grows linearly, not exponentially.
func TestRegexTimeIsLinear(t *testing.T) {
	svc := match.New(match.Deps{})
	m, err := svc.Compile(context.Background(), regex(`(a+)+$`))
	require.NoError(t, err)
	measure := func(n int) time.Duration {
		in := strings.Repeat("a", n) + "!"
		best := time.Duration(1 << 62)
		for i := 0; i < 5; i++ {
			start := time.Now()
			m.Match(in)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	small, large := measure(4096), measure(4*4096)
	// Linear growth would be ~4x; allow a generous margin for noise.
	assert.Less(t, large, 40*small+time.Millisecond, "4x input: %s -> %s", small, large)
}
