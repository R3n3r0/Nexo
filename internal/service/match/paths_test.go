package match_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// TestRequestPaths covers the patterns of the distribution behaviors and
// of the cache settings (docs/10 §10.2, docs/11 §11.1): decoded request
// paths with their leading "/", matched in normalized form.
func TestRequestPaths(t *testing.T) {
	ctx := context.Background()
	svc := match.New(match.Deps{})
	tests := []struct {
		pattern match.Pattern
		path    string
		want    bool
	}{
		{prefix("/internal/"), "/internal/a", true},
		{prefix("/internal/"), "/Internal/a", false},
		{ci(prefix("/internal/")), "/Internal/a", true},
		{prefix("/internal/"), "/assets/../internal/a", true},
		{prefix("/internal/"), "//internal/a", true},
		{glob("/assets/**"), "/assets/a/b/c.js", true},
		{glob("/assets/**"), "/assets/", true},
		{glob("/assets/**"), "/assetsX/a", false},
		{glob("/assets/*.js"), "/assets/app.js", true},
		{glob("/assets/*.js"), "/assets/a/app.js", false},
		{glob("/**/private/**"), "/a/b/private/x", true},
		{glob("/**/private/**"), "/private/x", true},
		{glob("/{img,video}/*"), "/video/a.mp4", true},
		{glob("/{img,video}/*"), "/audio/a.mp3", false},
		{glob("/file?.txt"), "/file1.txt", true},
		{glob("/file?.txt"), "/file/.txt", false},
		{glob("/[ab]*.txt"), "/b-x.txt", true},
		{glob("/[!ab]*.txt"), "/b-x.txt", false},
		{glob("/città/*.html"), "/città/a.html", true},
		{glob("/città/*.html"), "/città/a.html", true},
		{glob("/a+b(c).txt"), "/a+b(c).txt", true},
		{ci(glob("/A/*")), "/a/x", true},
		{regex(`^/downloads/.*\.(zip|iso)$`), "/downloads/v1/setup.iso", true},
		{regex(`^/downloads/.*\.(zip|iso)$`), "/downloads/v1/setup.exe", false},
	}
	for _, tc := range tests {
		t.Run(string(tc.pattern.Type)+" "+tc.pattern.Pattern+" "+tc.path, func(t *testing.T) {
			m, err := svc.Compile(ctx, tc.pattern)
			require.NoError(t, err)
			assert.Equal(t, tc.want, m.Match(svc.Normalize(ctx, tc.path)).Matched)
		})
	}
	for _, bad := range []match.Pattern{
		glob("/[abc"),
		glob("/{a,b"),
		regex("(a"),
		{Type: "wildcard", Pattern: "/x"},
	} {
		_, err := svc.Compile(ctx, bad)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", bad)
	}
}
