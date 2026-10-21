package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSubtitlesWebVTT(t *testing.T) {
	doc := "\ufeffWEBVTT - a title\r\n\r\nNOTE a comment\r\nover two lines\r\n\r\nSTYLE\n::cue { color: red }\n\n" +
		"intro\n00:01.000 --> 00:02.500 line:0 align:start bogus <script>\n<i>Hello</i>\n  world  \n\n" +
		"00:00:00.200 --> 00:00:00.900\nFirst --> second\x07\n\n" +
		"01:00:00.000 --> 01:00:01.000\n\n"
	cues, err := ParseSubtitles([]byte(doc))
	require.NoError(t, err)
	require.Len(t, cues, 2, "sorted by start, comments, styles and empty cues dropped")
	assert.Equal(t, SubtitleCue{Start: 200 * time.Millisecond, End: 900 * time.Millisecond, Text: "First -> second"}, cues[0])
	assert.Equal(t, SubtitleCue{Start: time.Second, End: 2500 * time.Millisecond, Text: "<i>Hello</i>\nworld",
		Settings: "line:0 align:start"}, cues[1])

	out := string(FormatWebVTT(cues))
	assert.Equal(t, "WEBVTT\n\n00:00:00.200 --> 00:00:00.900\nFirst -> second\n\n00:00:01.000 --> 00:00:02.500 line:0 align:start\n<i>Hello</i>\nworld\n", out)
	again, err := ParseSubtitles([]byte(out))
	require.NoError(t, err)
	assert.Equal(t, cues, again, "round trip")
}

func TestSubtitleCueTags(t *testing.T) {
	doc := "WEBVTT\n\n00:01.000 --> 00:02.000\n<script>alert(1)</script><b>a</b> <c.yellow.bg>b</c> <v Bob>c</v> <img src=x onerror=y>" +
		"<00:01.500>d < e <lang en>f</lang> <ruby>g<rt>h</rt></ruby> <a href='x'>i</a>\n"
	cues, err := ParseSubtitles([]byte(doc))
	require.NoError(t, err)
	require.Len(t, cues, 1)
	assert.Equal(t, "alert(1)<b>a</b> <c.yellow.bg>b</c> <v Bob>c</v> <00:01.500>d &lt; e <lang en>f</lang> <ruby>g<rt>h</rt></ruby> i", cues[0].Text)
}

func TestParseSubtitlesSRT(t *testing.T) {
	doc := "1\r\n00:00:01,000 --> 00:00:03,040\r\nCiao a tutti\r\n\r\n2\r\n00:00:03,500 --> 00:00:05,000\r\nSeconda riga\r\ne terza\r\n"
	cues, err := ParseSubtitles([]byte(doc))
	require.NoError(t, err)
	require.Len(t, cues, 2)
	assert.Equal(t, 1040*time.Millisecond+2*time.Second, cues[0].End)
	assert.Equal(t, "Seconda riga\ne terza", cues[1].Text)
	assert.Equal(t, "1\n00:00:01,000 --> 00:00:03,040\nCiao a tutti\n\n2\n00:00:03,500 --> 00:00:05,000\nSeconda riga\ne terza\n",
		string(FormatSRT(cues)))
}

func TestParseSubtitlesInvalid(t *testing.T) {
	for name, doc := range map[string]string{
		"no timing":       "WEBVTT\n\nhello\nworld\n",
		"bad timestamp":   "WEBVTT\n\n00:01 --> 00:02.000\nx\n",
		"bad minutes":     "WEBVTT\n\n00:61.000 --> 01:02.000\nx\n",
		"end before":      "WEBVTT\n\n00:02.000 --> 00:01.000\nx\n",
		"no end":          "WEBVTT\n\n00:02.000 -->\nx\n",
		"not utf-8":       "WEBVTT\n\n00:01.000 --> 00:02.000\n\xff\xfe\n",
		"too long a text": "WEBVTT\n\n00:01.000 --> 00:02.000\n" + strings.Repeat("a", 3000) + "\n",
	} {
		_, err := ParseSubtitles([]byte(doc))
		assert.ErrorIs(t, err, ErrInvalidArgument, name)
	}
	_, err := ParseSubtitles(make([]byte, MaxSubtitleBytes+1))
	assert.ErrorIs(t, err, ErrInvalidArgument)
}

func TestLanguages(t *testing.T) {
	for in, want := range map[string]string{"it": "it", "pt-br": "pt-BR", " EN ": "en", "zh-Hant-TW": "zh-Hant-TW", "es-419": "es-419"} {
		got, err := CanonicalLanguage(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "und", "x", "it_IT/..", "en-<b>", strings.Repeat("a", 40)} {
		_, err := CanonicalLanguage(bad)
		assert.ErrorIs(t, err, ErrInvalidArgument, bad)
	}
	assert.Equal(t, "Italiano", LanguageName("it"))
	assert.Equal(t, "English", LanguageName("en"))
	assert.Equal(t, "Deutsch", LanguageName("de"))
}

func TestSubtitlesManifest(t *testing.T) {
	var m MediaSubtitlesManifestDoc
	m.Put(MediaSubtitleTrack{Lang: "it", Auto: true, ETag: `"e1"`})
	m.Put(MediaSubtitleTrack{Lang: "en"})
	m.Put(MediaSubtitleTrack{Lang: "it", Auto: true, ETag: "e2"})
	require.Len(t, m.Tracks, 2)
	assert.Equal(t, "en", m.Tracks[0].Lang)
	it, ok := m.Track("it")
	require.True(t, ok)
	assert.True(t, it.Current(`"e2"`))
	assert.False(t, it.Current("e1"), "an automatic track follows the content")
	en, _ := m.Track("en")
	assert.True(t, en.Current("anything"), "a manual track always applies")
	assert.True(t, m.Remove("en"))
	assert.False(t, m.Remove("en"))
}

func TestMediaRuleSubtitles(t *testing.T) {
	c := MediaConfig{Rules: []MediaRule{{ID: "talks", Subtitles: &MediaSubtitles{Source: "IT", Languages: []string{"en", "pt-br", "en"}}}}}
	require.NoError(t, c.Validate(), "a rule may produce subtitles only")
	assert.Equal(t, "it", c.Rules[0].Subtitles.Source)
	assert.Equal(t, []string{"en", "pt-BR"}, c.Rules[0].Subtitles.Languages)
	clone := c.Clone()
	clone.Rules[0].Subtitles.Languages[0] = "fr"
	assert.Equal(t, "en", c.Rules[0].Subtitles.Languages[0])

	bad := MediaConfig{Rules: []MediaRule{{ID: "x", Subtitles: &MediaSubtitles{Languages: []string{"!!"}}}}}
	assert.ErrorIs(t, bad.Validate(), ErrInvalidArgument)
	img := MediaConfig{Rules: []MediaRule{{ID: "img", Images: &MediaImages{}, Subtitles: &MediaSubtitles{}}}}
	assert.ErrorContains(t, img.Validate(), "subtitles")

	src, file, ok := MediaSidecarSource(MediaSidecarDir("talks/a.mp4", "v1") + MediaSubtitleFile("it"))
	require.True(t, ok)
	assert.Equal(t, "talks/a.mp4", src)
	assert.Equal(t, "subtitles/it.vtt", file)
}
