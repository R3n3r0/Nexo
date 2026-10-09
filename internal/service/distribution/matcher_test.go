package distribution_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

func TestMatcher(t *testing.T) {
	ctx := context.Background()
	m := distribution.NewMatcher()
	tests := []struct {
		pattern distribution.PathPattern
		path    string
		want    bool
	}{
		{distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"}, "/internal/a", true},
		{distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/"}, "/Internal/a", false},
		{distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/internal/", CaseInsensitive: true}, "/Internal/a", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"}, "/assets/a/b/c.js", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"}, "/assets/", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"}, "/assetsX/a", false},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/*.js"}, "/assets/app.js", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/*.js"}, "/assets/a/app.js", false},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/**/private/**"}, "/a/b/private/x", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/**/private/**"}, "/private/x", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/{img,video}/*"}, "/video/a.mp4", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/{img,video}/*"}, "/audio/a.mp3", false},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/file?.txt"}, "/file1.txt", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/file?.txt"}, "/file/.txt", false},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/[ab]*.txt"}, "/b-x.txt", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/[!ab]*.txt"}, "/b-x.txt", false},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/città/*.html"}, "/città/a.html", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/a+b(c).txt"}, "/a+b(c).txt", true},
		{distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/A/*", CaseInsensitive: true}, "/a/x", true},
		{distribution.PathPattern{Type: distribution.PatternRegex, Pattern: `^/downloads/.*\.(zip|iso)$`}, "/downloads/v1/setup.iso", true},
		{distribution.PathPattern{Type: distribution.PatternRegex, Pattern: `^/downloads/.*\.(zip|iso)$`}, "/downloads/v1/setup.exe", false},
	}
	for _, tc := range tests {
		t.Run(string(tc.pattern.Type)+" "+tc.pattern.Pattern+" "+tc.path, func(t *testing.T) {
			p, err := m.Compile(ctx, tc.pattern)
			require.NoError(t, err)
			assert.Equal(t, tc.want, p.Match(tc.path))
		})
	}
	for _, bad := range []distribution.PathPattern{
		{Type: distribution.PatternGlob, Pattern: "/[abc"},
		{Type: distribution.PatternGlob, Pattern: "/{a,b"},
		{Type: distribution.PatternRegex, Pattern: "(a"},
		{Type: distribution.PatternRegex, Pattern: strings.Repeat("a", distribution.MaxPatternLength+1)},
		{Type: "wildcard", Pattern: "/x"},
		{Type: distribution.PatternPrefix},
	} {
		_, err := m.Compile(ctx, bad)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", bad)
	}
}
