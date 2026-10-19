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

func TestImageMediaRules(t *testing.T) {
	cfg := MediaConfig{Rules: []MediaRule{
		{ID: "pics", Prefix: "pics/", Images: &MediaImages{Formats: []string{"jpeg", "webp", "jpeg"}, Widths: []int{640, 160, 640}}},
		ImagesMediaRule(),
		HLSMediaRule(),
	}}
	require.NoError(t, cfg.Validate())
	pics := cfg.Rules[0]
	assert.Equal(t, []string{"webp", "jpeg"}, pics.Images.Formats, "the most compact first, deduplicated")
	assert.Equal(t, []int{160, 640}, pics.Images.Widths)
	assert.Equal(t, DefaultImageSuffixes(), pics.Suffixes, "an image rule selects images")
	assert.Equal(t, MediaImageFormats(), cfg.Rules[1].Images.Formats)

	r, ok := cfg.Match("pics/beach.JPG")
	require.True(t, ok)
	assert.Equal(t, "pics", r.ID)
	r, ok = cfg.Match("holidays/IMG_0001.heic")
	require.True(t, ok)
	assert.Equal(t, ImagesMediaRuleID, r.ID)
	r, ok = cfg.Match("holidays/clip.mp4")
	require.True(t, ok)
	assert.Equal(t, HLSMediaRuleID, r.ID, "videos go to the video rule")
	_, ok = cfg.Match(".nexo-media/pics/beach.jpg/null/images/640.webp")
	assert.False(t, ok, "the variants are never selected")

	for name, rule := range map[string]MediaRule{
		"format":       {ID: "x", Images: &MediaImages{Formats: []string{"bmp"}}},
		"small width":  {ID: "x", Images: &MediaImages{Widths: []int{8}}},
		"large width":  {ID: "x", Images: &MediaImages{Widths: []int{9000}}},
		"video output": {ID: "x", Poster: true, Images: &MediaImages{}},
	} {
		c := MediaConfig{Rules: []MediaRule{rule}}
		assert.ErrorIs(t, c.Validate(), ErrInvalidArgument, name)
	}
	many := MediaConfig{Rules: []MediaRule{{ID: "x", Images: &MediaImages{Widths: []int{16, 32, 48, 64, 80, 96, 112, 128, 144, 160, 176, 192, 208, 224, 240, 256, 272}}}}}
	assert.ErrorIs(t, many.Validate(), ErrInvalidArgument)

	clone := cfg.Clone()
	clone.Rules[0].Images.Widths[0] = 1
	assert.Equal(t, 160, cfg.Rules[0].Images.Widths[0], "Clone copies the variants")
	assert.Equal(t, "images/640.webp", MediaImageFile(640, "webp"))
	assert.Equal(t, "images/full.jpg", MediaImageFile(0, "jpeg"))
	assert.Equal(t, "image/avif", MediaImageContentType("avif"))
}
