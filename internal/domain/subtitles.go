package domain

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// The subtitle tracks of a video live in the sidecar directory of its
// version: one WebVTT file per language and a manifest describing them.
const (
	MediaSubtitlesDir      = "subtitles/"
	MediaSubtitlesManifest = MediaSubtitlesDir + "manifest.json"
)

// Bounds of a subtitle track.
const (
	// MaxSubtitleBytes bounds a subtitle file (WebVTT or SRT).
	MaxSubtitleBytes = 2 << 20
	// MaxSubtitleCueText bounds the text of a cue, in bytes.
	MaxSubtitleCueText = 2048
	// MaxSubtitleCues bounds the cues of a track.
	MaxSubtitleCues = 20_000
	// maxCueText bounds the text of a cue, in bytes.
	maxCueText = MaxSubtitleCueText
	// maxSubtitleLanguages bounds the languages of a rule.
	maxSubtitleLanguages = 20
)

// MediaSubtitleFile returns the sidecar file of the manual subtitle track
// of a language ("subtitles/it.vtt").
func MediaSubtitleFile(lang string) string { return MediaSubtitlesDir + lang + ".vtt" }

// MediaAutoSubtitlesDir holds the automatic tracks the media worker
// writes: never the files of the manual tracks, so that the worker can
// never overwrite a track a user uploaded meanwhile.
const MediaAutoSubtitlesDir = MediaSubtitlesDir + "auto/"

// MediaAutoSubtitleFile returns the sidecar file of the automatic subtitle
// track of a language ("subtitles/auto/it.vtt").
func MediaAutoSubtitleFile(lang string) string { return MediaAutoSubtitlesDir + lang + ".vtt" }

// MediaSubtitles configures the automatic subtitles of a video rule: the
// media worker transcribes the speech of the video in its language and
// translates the transcription into Languages.
type MediaSubtitles struct {
	// Source is the spoken language (a BCP-47 tag); empty lets the
	// transcription detect it.
	Source string `json:"source,omitempty"`
	// Languages are the languages to translate the transcription into
	// (BCP-47 tags), besides the spoken one.
	Languages []string `json:"languages,omitempty"`
}

// validate canonicalizes the language tags.
func (s *MediaSubtitles) validate() error {
	if s.Source != "" {
		tag, err := CanonicalLanguage(s.Source)
		if err != nil {
			return err
		}
		s.Source = tag
	}
	if len(s.Languages) > maxSubtitleLanguages {
		return fmt.Errorf("at most %d subtitle languages", maxSubtitleLanguages)
	}
	langs := make([]string, 0, len(s.Languages))
	for _, l := range s.Languages {
		tag, err := CanonicalLanguage(l)
		if err != nil {
			return err
		}
		if !slices.Contains(langs, tag) {
			langs = append(langs, tag)
		}
	}
	s.Languages = langs
	return nil
}

// CanonicalLanguage validates a BCP-47 language tag and returns its
// canonical form ("pt-br" is "pt-BR"): a language subtag and, optionally,
// a script and a region.
func CanonicalLanguage(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" || len(tag) > 35 {
		return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidArgument, tag)
	}
	t, err := language.Parse(tag)
	if err != nil {
		return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidArgument, tag)
	}
	base, conf := t.Base()
	if conf != language.Exact || base.String() == "und" {
		return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidArgument, tag)
	}
	out := t.String()
	for _, r := range out {
		if r != '-' && (r >= utf8.RuneSelf || !unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			return "", fmt.Errorf("%w: invalid language tag %q", ErrInvalidArgument, tag)
		}
	}
	return out, nil
}

// LanguageName returns the name of a language in that language
// ("Italiano" for it, "English" for en), the tag itself when unknown.
func LanguageName(tag string) string {
	t, err := language.Parse(tag)
	if err != nil {
		return tag
	}
	name := display.Self.Name(t)
	if name == "" {
		return tag
	}
	r, size := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r)) + name[size:]
}

// MediaSubtitleTrack describes a subtitle track of a video in its
// manifest.
type MediaSubtitleTrack struct {
	// Lang is the BCP-47 tag of the language (the file is File()).
	Lang string `json:"lang"`
	// Auto marks a track the media worker made (transcribed or
	// translated). A manual track (uploaded, or an automatic one edited)
	// is never overwritten by the worker.
	Auto bool `json:"auto"`
	// ETag is the ETag of the content of the video an automatic track was
	// made from: it is served only while the video keeps that content.
	ETag string `json:"etag,omitempty"`
	// From is the language an automatic track was translated from (empty
	// for the transcription).
	From string `json:"from,omitempty"`
	// Engine names the transcription or translation backend and model.
	Engine  string    `json:"engine,omitempty"`
	Cues    int       `json:"cues"`
	Size    int64     `json:"size"`
	Updated time.Time `json:"updated"`
	// UpdatedBy is the user who uploaded or edited a manual track.
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// File is the sidecar file of the track: MediaAutoSubtitleFile for an
// automatic track, MediaSubtitleFile for a manual one.
func (t MediaSubtitleTrack) File() string {
	if t.Auto {
		return MediaAutoSubtitleFile(t.Lang)
	}
	return MediaSubtitleFile(t.Lang)
}

// Current reports whether the track applies to a video whose content has
// etag: a manual track always does, an automatic one while the video
// keeps the content it was made from.
func (t MediaSubtitleTrack) Current(etag string) bool {
	return !t.Auto || strings.Trim(t.ETag, `"`) == strings.Trim(etag, `"`)
}

// MediaSubtitlesManifestDoc is the manifest of the subtitle tracks of a
// video (MediaSubtitlesManifest).
type MediaSubtitlesManifestDoc struct {
	Tracks []MediaSubtitleTrack `json:"tracks"`
}

// Track returns the track of a language.
func (m MediaSubtitlesManifestDoc) Track(lang string) (MediaSubtitleTrack, bool) {
	for _, t := range m.Tracks {
		if t.Lang == lang {
			return t, true
		}
	}
	return MediaSubtitleTrack{}, false
}

// Put adds or replaces the track of its language, keeping the tracks
// sorted by language.
func (m *MediaSubtitlesManifestDoc) Put(t MediaSubtitleTrack) {
	m.Remove(t.Lang)
	m.Tracks = append(m.Tracks, t)
	slices.SortFunc(m.Tracks, func(a, b MediaSubtitleTrack) int { return strings.Compare(a.Lang, b.Lang) })
}

// Remove removes the track of a language; it reports whether there was
// one.
func (m *MediaSubtitlesManifestDoc) Remove(lang string) bool {
	n := len(m.Tracks)
	m.Tracks = slices.DeleteFunc(m.Tracks, func(t MediaSubtitleTrack) bool { return t.Lang == lang })
	return len(m.Tracks) != n
}

// SubtitleCue is a cue of a subtitle track.
type SubtitleCue struct {
	Start time.Duration `json:"start"`
	End   time.Duration `json:"end"`
	// Text is the payload, lines separated by "\n" (WebVTT markup such
	// as <i> is kept).
	Text string `json:"text"`
	// Settings are the WebVTT cue settings ("line:0 align:start").
	Settings string `json:"settings,omitempty"`
}

// ParseSubtitles reads a WebVTT or SRT document: the cues sorted by start
// time, their text trimmed and sanitized (no "-->", no control
// characters, only the WebVTT cue tags b, i, u, c, v, lang, ruby, rt and
// timestamps). Comments, styles and regions of a WebVTT document are
// dropped. A document over the bounds, a malformed timestamp or a cue
// ending before it starts fails with ErrInvalidArgument.
func ParseSubtitles(data []byte) ([]SubtitleCue, error) {
	if len(data) > MaxSubtitleBytes {
		return nil, fmt.Errorf("%w: subtitles: more than %d bytes", ErrInvalidArgument, MaxSubtitleBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: subtitles: not UTF-8 text", ErrInvalidArgument)
	}
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	blocks := splitBlocks(text)
	vtt := len(blocks) > 0 && (blocks[0] == "WEBVTT" || strings.HasPrefix(blocks[0], "WEBVTT ") ||
		strings.HasPrefix(blocks[0], "WEBVTT\t") || strings.HasPrefix(blocks[0], "WEBVTT\n"))
	if vtt {
		blocks = blocks[1:]
	}
	var cues []SubtitleCue
	for _, b := range blocks {
		if vtt && (strings.HasPrefix(b, "NOTE") || strings.HasPrefix(b, "STYLE") || strings.HasPrefix(b, "REGION")) {
			continue
		}
		lines := strings.Split(b, "\n")
		// An identifier (WebVTT) or an index (SRT) before the timing line.
		if !strings.Contains(lines[0], "-->") {
			if len(lines) < 2 || !strings.Contains(lines[1], "-->") {
				return nil, fmt.Errorf("%w: subtitles: a cue without timing: %q", ErrInvalidArgument, truncateText(lines[0], 40))
			}
			lines = lines[1:]
		}
		cue, err := parseTiming(lines[0])
		if err != nil {
			return nil, err
		}
		cue.Text = sanitizeCueText(strings.Join(lines[1:], "\n"))
		if cue.Text == "" {
			continue
		}
		if len(cue.Text) > maxCueText {
			return nil, fmt.Errorf("%w: subtitles: a cue longer than %d bytes", ErrInvalidArgument, maxCueText)
		}
		cues = append(cues, cue)
		if len(cues) > MaxSubtitleCues {
			return nil, fmt.Errorf("%w: subtitles: more than %d cues", ErrInvalidArgument, MaxSubtitleCues)
		}
	}
	slices.SortStableFunc(cues, func(a, b SubtitleCue) int { return int(a.Start - b.Start) })
	return cues, nil
}

// splitBlocks splits a document at its blank lines.
func splitBlocks(text string) []string {
	var blocks []string
	var cur []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), MaxSubtitleBytes)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		if line == "" {
			if len(cur) > 0 {
				blocks = append(blocks, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, strings.Join(cur, "\n"))
	}
	return blocks
}

// parseTiming reads "start --> end [settings]".
func parseTiming(line string) (SubtitleCue, error) {
	left, right, _ := strings.Cut(line, "-->")
	fields := strings.Fields(right)
	if len(fields) == 0 {
		return SubtitleCue{}, fmt.Errorf("%w: subtitles: invalid timing %q", ErrInvalidArgument, truncateText(line, 60))
	}
	start, err := parseTimestamp(strings.TrimSpace(left))
	if err != nil {
		return SubtitleCue{}, err
	}
	end, err := parseTimestamp(fields[0])
	if err != nil {
		return SubtitleCue{}, err
	}
	if end <= start {
		return SubtitleCue{}, fmt.Errorf("%w: subtitles: a cue ends before it starts (%s)", ErrInvalidArgument, truncateText(line, 60))
	}
	return SubtitleCue{Start: start, End: end, Settings: sanitizeSettings(strings.Join(fields[1:], " "))}, nil
}

// parseTimestamp reads [hh:]mm:ss.ttt (WebVTT) or hh:mm:ss,ttt (SRT).
func parseTimestamp(s string) (time.Duration, error) {
	bad := fmt.Errorf("%w: subtitles: invalid timestamp %q", ErrInvalidArgument, truncateText(s, 20))
	s = strings.Replace(s, ",", ".", 1)
	main, frac, ok := strings.Cut(s, ".")
	if !ok || len(frac) == 0 || len(frac) > 3 {
		return 0, bad
	}
	parts := strings.Split(main, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	var total int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 || len(p) == 0 || (i > 0 && (n > 59 || len(p) != 2)) || n > 1_000_000 {
			return 0, bad
		}
		total = total*60 + n
	}
	ms, err := strconv.ParseInt(frac+strings.Repeat("0", 3-len(frac)), 10, 64)
	if err != nil || ms < 0 {
		return 0, bad
	}
	return time.Duration(total)*time.Second + time.Duration(ms)*time.Millisecond, nil
}

// isCueTag reports the WebVTT cue tags kept in the text of a cue.
func isCueTag(name string) bool {
	switch name {
	case "b", "i", "u", "c", "v", "lang", "ruby", "rt":
		return true
	}
	return false
}

// sanitizeCueTags keeps the WebVTT tags of a cue text (cueTags and the
// timestamps <00:01.000>) and drops the others, their content kept; a "<"
// opening no tag is escaped.
func sanitizeCueTags(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i:]
		j := strings.IndexAny(s[1:], "<>\n")
		if j < 0 || s[1+j] != '>' {
			b.WriteString("&lt;")
			s = s[1:]
			continue
		}
		tag := s[:j+2]
		name := strings.TrimPrefix(tag[1:len(tag)-1], "/")
		if k := strings.IndexAny(name, ". \t"); k >= 0 {
			name = name[:k]
		}
		if isCueTag(name) || (name != "" && name[0] >= '0' && name[0] <= '9' && !strings.ContainsAny(name, "\"'=")) {
			b.WriteString(tag)
		}
		s = s[j+2:]
	}
}

// sanitizeCueText trims the lines of a cue, drops the control characters,
// the tags that are not WebVTT cue tags and the "-->" a payload must not
// hold.
func sanitizeCueText(s string) string {
	s = sanitizeCueTags(strings.ReplaceAll(s, "-->", "->"))
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// sanitizeSettings keeps the WebVTT cue settings (name:value pairs).
func sanitizeSettings(s string) string {
	var out []string
	for _, f := range strings.Fields(s) {
		name, value, ok := strings.Cut(f, ":")
		if !ok || name == "" || value == "" || len(f) > 64 {
			continue
		}
		valid := true
		for _, r := range f {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(":%.,-_", r) {
				valid = false
			}
		}
		if valid {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

// FormatWebVTT writes cues as a WebVTT document.
func FormatWebVTT(cues []SubtitleCue) []byte {
	var b bytes.Buffer
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		b.WriteString("\n")
		b.WriteString(FormatSubtitleTime(c.Start))
		b.WriteString(" --> ")
		b.WriteString(FormatSubtitleTime(c.End))
		if c.Settings != "" {
			b.WriteString(" ")
			b.WriteString(c.Settings)
		}
		b.WriteString("\n")
		b.WriteString(sanitizeCueText(c.Text))
		b.WriteString("\n")
	}
	return b.Bytes()
}

// FormatSRT writes cues as an SRT document.
func FormatSRT(cues []SubtitleCue) []byte {
	var b bytes.Buffer
	for i, c := range cues {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n", i+1, strings.Replace(FormatSubtitleTime(c.Start), ".", ",", 1),
			strings.Replace(FormatSubtitleTime(c.End), ".", ",", 1), sanitizeCueText(c.Text))
	}
	return b.Bytes()
}

// FormatSubtitleTime writes a WebVTT timestamp, hh:mm:ss.ttt.
func FormatSubtitleTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}

// truncateText shortens s for an error message.
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
