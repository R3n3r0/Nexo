package domain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMediaConfigValidateAndMatch(t *testing.T) {
	c := MediaConfig{Rules: []MediaRule{
		{ID: "courses", Prefix: "courses/", Suffixes: []string{".MP4", ".mov"}, Renditions: []string{"480p", "1080p", "480p"}},
		{ID: "all", Poster: true, Thumbnails: &MediaThumbnails{}},
	}}
	require.NoError(t, c.Validate())
	assert.Equal(t, []string{".mov", ".mp4"}, c.Rules[0].Suffixes)
	assert.Equal(t, []string{"1080p", "480p"}, c.Rules[0].Renditions)
	assert.Equal(t, []string{".m4v", ".mov", ".mp4"}, c.Rules[1].Suffixes)
	assert.Equal(t, "10s", c.Rules[1].Thumbnails.Interval)

	r, ok := c.Match("courses/intro.MP4")
	require.True(t, ok)
	assert.Equal(t, "courses", r.ID)
	r, ok = c.Match("home/clip.m4v")
	require.True(t, ok)
	assert.Equal(t, "all", r.ID)
	_, ok = c.Match("notes.txt")
	assert.False(t, ok)
	_, ok = c.Match(MediaSidecarDir("courses/intro.mp4", "") + "renditions/720p.mp4")
	assert.False(t, ok, "sidecars never match")
	assert.Equal(t, ".nexo-media/a.mp4/null/", MediaSidecarDir("a.mp4", ""))

	for _, bad := range []MediaConfig{
		{Rules: []MediaRule{{ID: "x"}}},
		{Rules: []MediaRule{{ID: "bad id", Poster: true}}},
		{Rules: []MediaRule{{ID: "x", Poster: true}, {ID: "x", Poster: true}}},
		{Rules: []MediaRule{{ID: "x", Renditions: []string{"999p"}}}},
		{Rules: []MediaRule{{ID: "x", Prefix: ".nexo-media/", Poster: true}}},
		{Rules: []MediaRule{{ID: "x", Thumbnails: &MediaThumbnails{Interval: "1h"}}}},
	} {
		assert.ErrorIs(t, bad.Validate(), ErrInvalidArgument)
	}
}

func TestHLSMediaRule(t *testing.T) {
	rule := HLSMediaRule()
	cfg := MediaConfig{Rules: []MediaRule{rule}}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, HLSMediaRuleID, rule.ID)
	assert.Equal(t, MediaRenditionNames(), rule.Renditions, "every rendition")
	assert.True(t, rule.Poster)
	require.NotNil(t, rule.Thumbnails)
	assert.Equal(t, 10*time.Second, rule.Thumbnails.IntervalDuration())
	m, ok := cfg.Match("any/folder/clip.MOV")
	require.True(t, ok, "the whole bucket, the default suffixes")
	assert.Equal(t, HLSMediaRuleID, m.ID)
	// A new rule each call: changing one leaves the next alone.
	rule.Renditions[0] = "x"
	assert.Equal(t, "2160p", HLSMediaRule().Renditions[0])
}
