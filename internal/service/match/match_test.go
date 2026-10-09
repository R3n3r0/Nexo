package match_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// matchGroup is a pattern with the keys it must and must not match.
type matchGroup struct {
	name    string
	pattern match.Pattern
	match   []string
	noMatch []string
}

func prefix(p string) match.Pattern { return match.Pattern{Type: match.TypePrefix, Pattern: p} }
func glob(p string) match.Pattern   { return match.Pattern{Type: match.TypeGlob, Pattern: p} }
func regex(p string) match.Pattern  { return match.Pattern{Type: match.TypeRegex, Pattern: p} }
func ci(p match.Pattern) match.Pattern {
	p.CaseInsensitive = true
	return p
}

// matchingTable is the TAR-01 table: prefix, glob and regex patterns with
// segments, "**", classes, alternatives, case folding, Unicode, keys with
// '%', spaces and '+'.
func matchingTable() []matchGroup {
	return []matchGroup{
		// --- prefix ---
		{name: "prefix folder", pattern: prefix("public/"),
			match:   []string{"public/", "public/a", "public/a/b/c.txt", "public/%2F", "public/ space", "public/+plus"},
			noMatch: []string{"public", "Public/a", "pub/a", "private/public/a", "", "xpublic/a"}},
		{name: "prefix without slash", pattern: prefix("img"),
			match:   []string{"img", "img/", "imgs/a.png", "img2026/a"},
			noMatch: []string{"im", "IMG/a", "a/img"}},
		{name: "prefix case-insensitive", pattern: ci(prefix("Docs/")),
			match:   []string{"docs/a", "DOCS/a", "dOcS/x/y", "Docs/"},
			noMatch: []string{"doc/a", "xdocs/a", "docs"}},
		{name: "prefix unicode", pattern: prefix("città/"),
			match:   []string{"città/roma", "città/"},
			noMatch: []string{"citta/roma", "CITTÀ/roma"}},
		{name: "prefix percent", pattern: prefix("a%20b/"),
			match:   []string{"a%20b/c"},
			noMatch: []string{"a b/c", "a%2520b/c"}},
		{name: "prefix space and plus", pattern: prefix("my docs/c++/"),
			match:   []string{"my docs/c++/main.cpp"},
			noMatch: []string{"my+docs/c++/main.cpp", "my docs/c/main.cpp", "my%20docs/c++/x"}},
		// --- glob: single star ---
		{name: "glob star within a segment", pattern: glob("img/*.png"),
			match:   []string{"img/a.png", "img/.png", "img/long name.png", "img/a+b.png", "img/%41.png"},
			noMatch: []string{"img/a/b.png", "img/a.png.bak", "img/a.PNG", "img.png", "x/img/a.png"}},
		{name: "glob star in the middle", pattern: glob("users/*/avatar.jpg"),
			match:   []string{"users/mario/avatar.jpg", "users/.../avatar.jpg"},
			noMatch: []string{"users/mario/x/avatar.jpg", "users/avatar.jpg", "users//avatar.jpg", "users/mario/avatar.jpeg"}},
		{name: "glob star alone", pattern: glob("*"),
			match:   []string{"a", "file.txt", ""},
			noMatch: []string{"a/b", "dir/"}},
		{name: "glob two stars per segment", pattern: glob("*-*.log"),
			match:   []string{"app-1.log", "-.log", "a-b-c.log"},
			noMatch: []string{"app.log", "dir/app-1.log"}},
		// --- glob: double star ---
		{name: "glob double star at the end", pattern: glob("private/**"),
			match:   []string{"private/", "private/a", "private/a/b/c"},
			noMatch: []string{"private", "privatex/a", "a/private/b"}},
		{name: "glob double star anywhere", pattern: glob("**/private/**"),
			match:   []string{"private/a", "a/private/b", "a/b/c/private/d/e", "private/"},
			noMatch: []string{"private", "aprivate/b", "a/privatex/b", "a/private"}},
		{name: "glob double star zero segments", pattern: glob("docs/**/*.pdf"),
			match:   []string{"docs/a.pdf", "docs/x/a.pdf", "docs/x/y/z/a.pdf"},
			noMatch: []string{"docs/a.pdf.txt", "doc/a.pdf", "docs/x/a.PDF", "a/docs/a.pdf"}},
		{name: "glob leading double star", pattern: glob("**/*.tmp"),
			match:   []string{"a.tmp", "x/a.tmp", "x/y/.tmp"},
			noMatch: []string{"a.tmp/x", "a.tmpx"}},
		{name: "glob double star inside a segment", pattern: glob("a**b"),
			match:   []string{"ab", "a/b", "axx/yyb"},
			noMatch: []string{"a/c", "ba"}},
		{name: "glob triple star", pattern: glob("x/***/y"),
			match:   []string{"x/y", "x/a/y", "x/a/b/y"},
			noMatch: []string{"x/a/z"}},
		// --- glob: question mark ---
		{name: "glob question mark", pattern: glob("log-????.txt"),
			match:   []string{"log-2026.txt", "log-abcd.txt", "log-à1b2.txt"},
			noMatch: []string{"log-202.txt", "log-20266.txt", "log-20/6.txt"}},
		// --- glob: classes ---
		{name: "glob class", pattern: glob("v[123]/app"),
			match:   []string{"v1/app", "v2/app", "v3/app"},
			noMatch: []string{"v4/app", "v12/app", "v/app"}},
		{name: "glob class range", pattern: glob("[a-c][0-9].bin"),
			match:   []string{"a0.bin", "c9.bin", "b5.bin"},
			noMatch: []string{"d0.bin", "a.bin", "A0.bin"}},
		{name: "glob negated class", pattern: glob("x[!0-9]y"),
			match:   []string{"xay", "x-y", "x y"},
			noMatch: []string{"x1y", "x/y", "xy"}},
		{name: "glob caret negated class", pattern: glob("x[^ab]"),
			match:   []string{"xc", "xz"},
			noMatch: []string{"xa", "xb", "x/"}},
		{name: "glob class with bracket", pattern: glob("[]a]z"),
			match:   []string{"]z", "az"},
			noMatch: []string{"bz"}},
		{name: "glob class with escaped dash", pattern: glob(`[a\-z]`),
			match:   []string{"a", "-", "z"},
			noMatch: []string{"b", "y"}},
		// --- glob: alternatives ---
		{name: "glob alternatives", pattern: glob("{img,video}/*"),
			match:   []string{"img/a", "video/b.mp4"},
			noMatch: []string{"audio/a", "img/a/b", "imgvideo/a"}},
		{name: "glob alternatives with globs", pattern: glob("*.{jpg,jpe?g,png}"),
			match:   []string{"a.jpg", "a.jpexg", "a.png"},
			noMatch: []string{"a.gif", "a.jpeg", "a.jpgx", "d/a.jpg"}},
		{name: "glob nested alternatives", pattern: glob("{a,b{c,d}}/x"),
			match:   []string{"a/x", "bc/x", "bd/x"},
			noMatch: []string{"b/x", "c/x", "ab/x"}},
		{name: "glob empty alternative", pattern: glob("file{,.bak}"),
			match:   []string{"file", "file.bak"},
			noMatch: []string{"file.old"}},
		{name: "glob double star in alternatives", pattern: glob("{**/,}secret"),
			match:   []string{"secret", "a/secret", "a/b/secret"},
			noMatch: []string{"asecret", "secret/x"}},
		// --- glob: literal metacharacters ---
		{name: "glob regex metacharacters are literal", pattern: glob("a.b+c(1)|$^"),
			match:   []string{"a.b+c(1)|$^"},
			noMatch: []string{"aXb+c(1)|$^", "a.bbc(1)|$^"}},
		{name: "glob escapes", pattern: glob(`a\*b\?c\[d\{e`),
			match:   []string{"a*b?c[d{e"},
			noMatch: []string{"axbyc[d{e", "a*b?c"}},
		{name: "glob comma and brace outside alternatives", pattern: glob("a,b}c"),
			match:   []string{"a,b}c"},
			noMatch: []string{"a"}},
		{name: "glob percent space plus", pattern: glob("my files/*%20*+.txt"),
			match:   []string{"my files/a%20b+.txt", "my files/%20+.txt"},
			noMatch: []string{"my files/a b+.txt", "my%20files/a%20b+.txt", "my files/a%20b .txt"}},
		{name: "glob unicode", pattern: glob("foto/??/*.jpg"),
			match:   []string{"foto/éè/a.jpg", "foto/日本/写真.jpg"},
			noMatch: []string{"foto/é/a.jpg", "foto/日本人/a.jpg"}},
		{name: "glob case-insensitive", pattern: ci(glob("**/*.PDF")),
			match:   []string{"a.pdf", "x/A.Pdf", "X/Y/b.PDF"},
			noMatch: []string{"a.pdx", "a.pdf/x"}},
		{name: "glob case-insensitive unicode", pattern: ci(glob("ÉTÉ/*")),
			match:   []string{"été/a", "ÉTÉ/a", "Été/b"},
			noMatch: []string{"ete/a"}},
		{name: "glob leading slash paths", pattern: glob("/assets/**"),
			match:   []string{"/assets/app.js", "/assets/css/a.css"},
			noMatch: []string{"assets/app.js", "/asset/app.js"}},
		// --- regex ---
		{name: "regex anchored images", pattern: regex(`^img/[0-9]{4}/.*\.(jpe?g|png|webp)$`),
			match:   []string{"img/2026/a.jpg", "img/2026/x/y.jpeg", "img/1999/a.webp", "img/2026/.png"},
			noMatch: []string{"img/26/a.jpg", "img/2026/a.gif", "x/img/2026/a.jpg", "img/2026/a.jpg.txt", "IMG/2026/a.jpg"}},
		{name: "regex case-insensitive", pattern: ci(regex(`^img/[0-9]{4}/.*\.(jpe?g|png)$`)),
			match:   []string{"IMG/2026/A.JPG", "img/2026/b.Png"},
			noMatch: []string{"img/2026/a.gif"}},
		{name: "regex unanchored", pattern: regex(`private`),
			match:   []string{"private", "a/private/b", "xprivatex"},
			noMatch: []string{"Private", "priv"}},
		{name: "regex prefix anchor only", pattern: regex(`^home/[^/]+/`),
			match:   []string{"home/mario/a", "home/x/"},
			noMatch: []string{"home/mario", "home//a", "a/home/x/"}},
		{name: "regex avatar", pattern: regex(`^users/[a-z0-9_-]+/avatar\.png$`),
			match:   []string{"users/mario_1/avatar.png", "users/a-b/avatar.png"},
			noMatch: []string{"users/Mario/avatar.png", "users/a/b/avatar.png", "users/a/avatar.pngx"}},
		{name: "regex alternation", pattern: regex(`^(docs|img)/`),
			match:   []string{"docs/a", "img/b"},
			noMatch: []string{"video/a", "doc/a"}},
		{name: "regex unicode classes", pattern: regex(`^\p{Greek}+/`),
			match:   []string{"αβγ/a", "Ω/b"},
			noMatch: []string{"abc/a", "/a"}},
		{name: "regex percent and plus", pattern: regex(`^a%2F\+b$`),
			match:   []string{"a%2F+b"},
			noMatch: []string{"a/+b", "a%2Fb"}},
		{name: "regex spaces", pattern: regex(`^my files/.* \(copy\)\.txt$`),
			match:   []string{"my files/a (copy).txt"},
			noMatch: []string{"my files/a(copy).txt", "my%20files/a (copy).txt"}},
		{name: "regex inline flag", pattern: regex(`(?i)^secret/`),
			match:   []string{"SECRET/a", "Secret/b", "secret/c"},
			noMatch: []string{"secrets/a"}},
		{name: "regex dot does not match newline", pattern: regex(`^a.b$`),
			match:   []string{"axb", "a/b"},
			noMatch: []string{"a\nb", "ab"}},
	}
}

func TestMatchingTable(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	total := 0
	for _, g := range matchingTable() {
		m, err := svc.Compile(ctx, g.pattern)
		require.NoError(t, err, g.name)
		set, err := svc.CompileSet(ctx, []match.Pattern{g.pattern})
		require.NoError(t, err, g.name)
		check := func(key string, want bool) {
			total++
			norm := svc.Normalize(ctx, key)
			assert.Equal(t, want, m.Match(norm).Matched, "%s: %q on %q", g.name, g.pattern.Pattern, key)
			got := false
			set.Match(norm, nil, func(int, match.Result) bool { got = true; return true })
			assert.Equal(t, want, got, "%s (set): %q on %q", g.name, g.pattern.Pattern, key)
		}
		for _, k := range g.match {
			check(k, true)
		}
		for _, k := range g.noMatch {
			check(k, false)
		}
	}
	assert.Greater(t, total, 200, "TAR-01 asks for more than 200 cases")
}

func TestCaptures(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	m, err := svc.Compile(ctx, regex(`^home/(?P<user>[^/]+)/(?<kind>docs|img)?`))
	require.NoError(t, err)
	assert.Equal(t, []string{"user", "kind"}, m.Groups())
	r := m.Match("home/mario/docs/a.txt")
	require.True(t, r.Matched)
	assert.Equal(t, map[string]string{"user": "mario", "kind": "docs"}, r.Captures)
	r = m.Match("home/luigi/other")
	require.True(t, r.Matched)
	assert.Equal(t, map[string]string{"user": "luigi", "kind": ""}, r.Captures)
	assert.False(t, m.Match("other/mario/").Matched)

	plain, err := svc.Compile(ctx, glob("home/*/x"))
	require.NoError(t, err)
	assert.Nil(t, plain.Match("home/a/x").Captures)
	assert.Empty(t, plain.Groups())
}

func TestExpr(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	tests := []struct {
		p    match.Pattern
		want string
	}{
		{prefix("a.b/"), `^a\.b/`},
		{ci(prefix("a")), `(?i)^a`},
		{glob("*.txt"), `^[^/]*\.txt$`},
		{glob("a/**/b"), `^a/(?:.*/)?b$`},
		{glob("{x,y}[!a]?"), `^(?:x|y)[^/a][^/]$`},
		{ci(glob("a")), `(?i)^a$`},
		{regex("^x(y)$"), `^x(y)$`},
	}
	for _, tt := range tests {
		m, err := svc.Compile(ctx, tt.p)
		require.NoError(t, err)
		assert.Equal(t, tt.want, m.Expr(), tt.p.Pattern)
		assert.Equal(t, tt.p, m.Pattern())
	}
}

// TestInvalidPatterns is TAR-02: invalid regexes and globs, patterns too
// long: domain.ErrInvalidArgument with the position of the problem.
func TestInvalidPatterns(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	long := strings.Repeat("a", match.MaxPatternLength+1)
	tests := []struct {
		name    string
		p       match.Pattern
		wantPos int
		wantMsg string
	}{
		{"empty regex", regex(""), 0, "empty"},
		{"empty glob", glob(""), 0, "empty"},
		{"unknown type", match.Pattern{Type: "wildcard", Pattern: "x"}, 0, "unknown pattern type"},
		{"missing type", match.Pattern{Pattern: "x"}, 0, "unknown pattern type"},
		{"regex too long", regex(long), match.MaxPatternLength + 1, "longer than 1024"},
		{"glob too long", glob(long), match.MaxPatternLength + 1, "longer than 1024"},
		{"prefix too long", prefix(long), match.MaxPatternLength + 1, "longer than 1024"},
		{"regex missing paren", regex(`^img/(a|b`), 1, "missing closing )"},
		{"regex unexpected paren", regex(`ab)c`), 0, "unexpected )"},
		{"regex invalid repeat", regex(`img/**`), 5, "invalid nested repetition operator"},
		{"regex backreference", regex(`^(a)\1$`), 5, "invalid escape sequence"},
		{"regex lookahead", regex(`^a(?=b)`), 3, "invalid or unsupported Perl syntax"},
		{"regex bad class", regex(`x[z-a]`), 3, "invalid character class range"},
		{"regex repeat count", regex(`a{1001}`), 2, "invalid repeat count"},
		{"glob unterminated class", glob("img/[abc"), 5, "unterminated character class"},
		{"glob bad range", glob("x[z-a]"), 4, "invalid character class range"},
		{"glob unterminated alternation", glob("a/{b,c"), 3, "unterminated alternation"},
		{"glob trailing backslash", glob(`a\`), 2, "trailing backslash"},
		{"invalid utf-8", glob("a\xffb"), 0, "UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Compile(ctx, tt.p)
			require.Error(t, err)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			var pe *match.PatternError
			require.ErrorAs(t, err, &pe)
			assert.Equal(t, -1, pe.Index)
			if tt.wantPos > 0 {
				assert.Equal(t, tt.wantPos, pe.Position, pe.Error())
				assert.Contains(t, pe.Error(), fmt.Sprintf("position %d", tt.wantPos))
			} else if tt.name != "regex unexpected paren" {
				assert.Zero(t, pe.Position, pe.Error())
			}
			assert.Contains(t, pe.Error(), tt.wantMsg)
		})
	}
}

func TestCompileSetErrorNamesThePattern(t *testing.T) {
	svc := match.New(match.Deps{})
	_, err := svc.CompileSet(context.Background(), []match.Pattern{prefix("a/"), glob("b/*"), regex("(c")})
	var pe *match.PatternError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 2, pe.Index)
	assert.Contains(t, pe.Error(), "pattern 3 (regex)")
}

func TestMaxPatternLengthOption(t *testing.T) {
	svc := match.New(match.Deps{}, match.WithMaxPatternLength(4))
	_, err := svc.Compile(context.Background(), glob("abcde"))
	var pe *match.PatternError
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, 5, pe.Position)
	_, err = svc.Compile(context.Background(), glob("abcd"))
	require.NoError(t, err)
}

// TestSetOrderAndFilters checks that a Set yields every match in index
// order, skips what keep refuses, stops when yield returns false and never
// loses a pattern to the trie prefilter (literal starts, folded literals,
// patterns without a literal).
func TestSetOrderAndFilters(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	patterns := []match.Pattern{
		prefix("img/"),                      // 0
		glob("img/*.png"),                   // 1
		regex(`^img/[0-9]+/`),               // 2
		regex(`png$`),                       // 3 (no literal start)
		ci(prefix("IMG/")),                  // 4
		ci(regex(`^img/2026`)),              // 5
		regex(`(?i)^IMG/`),                  // 6 (folded literal without CaseInsensitive)
		glob("**/*.png"),                    // 7 (no literal start)
		prefix("video/"),                    // 8
		regex(`^im`),                        // 9 (shorter literal)
		ci(glob("ſecret/*")),                // 10 (LONG S folds with s and S)
		regex(`^img/(?P<year>[0-9]{4})/.*`), // 11
	}
	set, err := svc.CompileSet(ctx, patterns)
	require.NoError(t, err)
	require.Equal(t, len(patterns), set.Len())
	assert.Equal(t, patterns[3], set.Matcher(3).Pattern())

	all, err := svc.CompileSet(ctx, []match.Pattern{prefix(""), glob("x")})
	require.NoError(t, err)
	var hits []int
	all.Match("anything", nil, func(i int, _ match.Result) bool { hits = append(hits, i); return true })
	assert.Equal(t, []int{0}, hits, "the empty prefix matches every key")

	collect := func(key string, keep func(int) bool) []int {
		var got []int
		set.Match(key, keep, func(i int, _ match.Result) bool { got = append(got, i); return true })
		return got
	}
	assert.Equal(t, []int{0, 2, 3, 4, 5, 6, 7, 9, 11}, collect("img/2026/a.png", nil))
	assert.Equal(t, []int{0, 3, 4, 6, 7, 9}, collect("img/x/a.png", nil))
	assert.Equal(t, []int{0, 1, 3, 4, 6, 7, 9}, collect("img/a.png", nil))
	assert.Equal(t, []int{3, 4, 5, 6, 7}, collect("IMG/2026/a.png", nil))
	assert.Equal(t, []int{8}, collect("video/a.mp4", nil))
	assert.Equal(t, []int{10}, collect("SECRET/x", nil))
	assert.Equal(t, []int{10}, collect("secret/x", nil))
	assert.Empty(t, collect("other", nil))

	odd := func(i int) bool { return i%2 == 1 }
	assert.Equal(t, []int{3, 5, 7, 9, 11}, collect("img/2026/a.png", odd))

	var first []int
	set.Match("img/2026/a.png", nil, func(i int, _ match.Result) bool { first = append(first, i); return len(first) < 2 })
	assert.Equal(t, []int{0, 2}, first)

	var caps map[string]string
	set.Match("img/2026/a.png", func(i int) bool { return i == 11 }, func(_ int, r match.Result) bool { caps = r.Captures; return false })
	assert.Equal(t, map[string]string{"year": "2026"}, caps)
}

func TestNormalize(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	tests := []struct{ in, want string }{
		{"a/b/c", "a/b/c"},
		{"a//b", "a/b"},
		{"a///b//c", "a/b/c"},
		{"./a/b", "a/b"},
		{"a/./b", "a/b"},
		{"a/b/.", "a/b/"},
		{"x/../a/b", "a/b"},
		{"../../a", "a"},
		{"a/b/..", "a/"},
		{"a/b/", "a/b/"},
		{"/a/b", "/a/b"},
		{"//a", "/a"},
		{".", ""},
		{"..", ""},
		{"", ""},
		{"a/..b/c", "a/..b/c"},
		{"a/.b", "a/.b"},
		{"café/x", "café/x"},
		{"%2e%2e/a", "%2e%2e/a"},
		{"a%2Fb", "a%2Fb"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, svc.Normalize(ctx, tt.in), tt.in)
	}
	noNFC := match.New(match.Deps{}, match.WithNFC(false))
	assert.Equal(t, "café/x", noNFC.Normalize(ctx, "café/x"))
}

func TestDecode(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	tests := []struct{ in, want string }{
		{"a/b", "a/b"},
		{"a%2Fb", "a/b"},
		{"a%252Fb", "a%2Fb"},
		{"%2e%2e/private/x", "private/x"},
		{"pub/%2e/private", "pub/private"},
		{"a+b", "a+b"},
		{"a%20b", "a b"},
		{"cafe%CC%81", "café"},
	}
	for _, tt := range tests {
		got, err := svc.Decode(ctx, tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.want, got, tt.in)
	}
	_, err := svc.Decode(ctx, "a%zzb")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestPrefilterNeverExcludesAMatch checks the trie and the literal
// prefilters against the bare RE2 expression of every pattern, on the
// keys of the table and on keys mixing their fragments.
func TestPrefilterNeverExcludesAMatch(t *testing.T) {
	svc := match.New(match.Deps{})
	ctx := context.Background()
	groups := matchingTable()
	var patterns []match.Pattern
	keys := map[string]bool{}
	for _, g := range groups {
		patterns = append(patterns, g.pattern)
		for _, k := range append(append([]string{}, g.match...), g.noMatch...) {
			keys[svc.Normalize(ctx, k)] = true
		}
	}
	extra := []match.Pattern{
		regex(`(^|/)secret-[0-9]+/`), regex(`(?i)\.(css|js|woff2?)$|^static/`), ci(regex(`(a|b)c+de`)),
		regex(`x{2,}y`), regex(`(?:ab)+`), ci(glob("**/ſecret/**")), regex(`K`), ci(regex(`k`)),
	}
	patterns = append(patterns, extra...)
	for _, k := range []string{"a/secret-12/x", "SECRET-1/", "x.CSS", "static/a", "xxACCDE", "xxy", "abab", "a/SECRET/b", "K", "K"} {
		keys[k] = true
	}
	set, err := svc.CompileSet(ctx, patterns)
	require.NoError(t, err)
	checked := 0
	for key := range keys {
		hits := map[int]bool{}
		set.Match(key, nil, func(i int, _ match.Result) bool { hits[i] = true; return true })
		for i, p := range patterns {
			m, err := svc.Compile(ctx, p)
			require.NoError(t, err)
			want := regexp.MustCompile(m.Expr()).MatchString(key)
			assert.Equal(t, want, m.Match(key).Matched, "%q on %q (matcher)", p.Pattern, key)
			assert.Equal(t, want, hits[i], "%q on %q (set)", p.Pattern, key)
			checked++
		}
	}
	assert.Greater(t, checked, 1000)
}

// BenchmarkSet200Regexes matches keys against 200 regexes of a Set.
func BenchmarkSet200Regexes(b *testing.B) {
	svc := match.New(match.Deps{})
	var patterns []match.Pattern
	for i := 0; i < 200; i++ {
		switch i % 3 {
		case 0:
			patterns = append(patterns, regex(fmt.Sprintf(`^tenants/t%03d/[a-z0-9_-]+/.*\.(jpe?g|png|pdf)$`, i)))
		case 1:
			patterns = append(patterns, regex(fmt.Sprintf(`(^|/)secret-%03d/`, i)))
		default:
			patterns = append(patterns, glob(fmt.Sprintf("**/dir%03d/**/*.{txt,md}", i)))
		}
	}
	set, err := svc.CompileSet(context.Background(), patterns)
	require.NoError(b, err)
	keys := []string{"tenants/t042/docs/report.pdf", "a/b/secret-100/x", "x/dir050/y/z.md", "nothing/here.bin"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		set.Match(keys[i%len(keys)], nil, func(int, match.Result) bool { return true })
	}
}
