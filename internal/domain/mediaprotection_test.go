package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMediaProtectionValidate: the defaults of a protection, its bounds
// and the normalized origins.
//
// covers: S-OBJ-17
func TestMediaProtectionValidate(t *testing.T) {
	p := MediaProtection{Enabled: true, AllowedOrigins: []string{" HTTPS://Learn.Example.com/ "}}
	require.NoError(t, p.Validate())
	assert.Equal(t, DefaultMediaSuffixes(), p.Suffixes)
	assert.Equal(t, DefaultPlaybackTokenTTL, p.TokenTTL())
	assert.Equal(t, DefaultMediaSessions, p.MaxSessions)
	assert.Equal(t, DefaultMediaPaceFactor, p.PaceFactor)
	assert.Equal(t, []string{"https://learn.example.com"}, p.AllowedOrigins)
	assert.Equal(t, MediaPlaybackStandard, p.Mode)
	assert.Equal(t, MediaLegacyRefuse, p.LegacyDevices)
	assert.True(t, p.AllowsStandardPlayback())

	for name, bad := range map[string]MediaProtection{
		"short TTL":       {TokenTTLSeconds: 30},
		"long TTL":        {TokenTTLSeconds: int(25 * time.Hour / time.Second)},
		"sessions":        {MaxSessions: -2},
		"pace":            {PaceFactor: 21},
		"origin path":     {AllowedOrigins: []string{"https://a.example/x"}},
		"origin scheme":   {AllowedOrigins: []string{"ftp://a.example"}},
		"reserved prefix": {Prefix: MediaSidecarPrefix},
		"empty suffix":    {Suffixes: []string{" "}},
		"mode":            {Mode: "drm"},
		"legacy devices":  {LegacyDevices: "maybe"},
	} {
		require.ErrorIs(t, bad.Validate(), ErrInvalidArgument, name)
	}
	unlimited := MediaProtection{MaxSessions: -1}
	require.NoError(t, unlimited.Validate())
	assert.Equal(t, -1, unlimited.MaxSessions)
}

// TestMediaProtectionAdvanced: the advanced mode refuses the standard
// playback unless the legacy devices may take it.
//
// covers: S-OBJ-18
func TestMediaProtectionAdvanced(t *testing.T) {
	p := MediaProtection{Enabled: true, Mode: MediaPlaybackAdvanced}
	require.NoError(t, p.Validate())
	assert.True(t, p.Advanced())
	assert.False(t, p.AllowsStandardPlayback())
	p.LegacyDevices = MediaLegacyStandard
	assert.True(t, p.AllowsStandardPlayback())
	var none *MediaProtection
	assert.False(t, none.Advanced())
}

// TestMediaProtectionProtects: the videos under the prefix and their
// renditions are protected; posters, thumbnails, other files and a
// disabled protection are not.
//
// covers: S-OBJ-17
func TestMediaProtectionProtects(t *testing.T) {
	p := &MediaProtection{Enabled: true, Prefix: "courses/"}
	require.NoError(t, p.Validate())
	assert.True(t, p.Protects("courses/intro.MP4"))
	assert.True(t, p.Protects("courses/a/b.mov"))
	assert.False(t, p.Protects("courses/notes.pdf"))
	assert.False(t, p.Protects("public/intro.mp4"))
	dir := MediaSidecarDir("courses/intro.mp4", "v1")
	assert.True(t, p.Protects(dir+MediaRenditionsDir+"720p.mp4"), "a rendition")
	assert.False(t, p.Protects(dir+MediaPosterFile), "the poster")
	assert.False(t, p.Protects(MediaSidecarDir("public/x.mp4", "v1")+MediaRenditionsDir+"720p.mp4"))
	p.Enabled = false
	assert.False(t, p.Protects("courses/intro.mp4"))
	var none *MediaProtection
	assert.False(t, none.Protects("courses/intro.mp4"))

	assert.True(t, (&MediaProtection{}).AllowsOrigin("https://any.example"))
	limited := &MediaProtection{AllowedOrigins: []string{"https://learn.example.com"}}
	assert.True(t, limited.AllowsOrigin(""), "a native player sends no origin")
	assert.True(t, limited.AllowsOrigin("https://LEARN.example.com"))
	assert.False(t, limited.AllowsOrigin("https://evil.example"))
}
