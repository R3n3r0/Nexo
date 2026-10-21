package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// SubtitleTrack describes a subtitle track of a video.
type SubtitleTrack struct {
	// Lang is the BCP 47 language tag; Name the name of the language in
	// that language.
	Lang string `json:"lang"`
	Name string `json:"name"`
	// Auto reports a track the media worker derived (transcribed or
	// translated) from the current content; a manual track was uploaded
	// or edited.
	Auto bool `json:"auto"`
	// From is the language an automatic translation derives from.
	From string `json:"from,omitempty"`
	// Engine names what produced an automatic track.
	Engine    string     `json:"engine,omitempty"`
	Cues      int        `json:"cues"`
	Size      int64      `json:"size"`
	Updated   *time.Time `json:"updated,omitempty"`
	UpdatedBy string     `json:"updatedBy,omitempty"`
}

// SubtitleCue is a cue of a subtitle track, its times in seconds from the
// start of the video.
type SubtitleCue struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	// Settings are the WebVTT cue settings (line:0 align:start...).
	Settings string `json:"settings,omitempty"`
}

// SubtitleCues is a subtitle track and its cues.
type SubtitleCues struct {
	Track SubtitleTrack `json:"track"`
	Cues  []SubtitleCue `json:"cues"`
}

func subtitlesPath(bucket string, lang ...string) string {
	return pathOf(append([]string{"buckets", bucket, "objects", "subtitles"}, lang...)...)
}

// ListSubtitles implements AdminClient.
func (c *client) ListSubtitles(ctx context.Context, bucket, key, versionID string) ([]SubtitleTrack, error) {
	var out struct {
		Tracks []SubtitleTrack `json:"tracks"`
	}
	err := c.do(ctx, request{method: http.MethodGet, path: subtitlesPath(bucket), query: versionQuery(key, versionID)}, &out)
	return out.Tracks, err
}

// GetSubtitles implements AdminClient.
func (c *client) GetSubtitles(ctx context.Context, bucket, key, versionID, lang string) (*SubtitleCues, error) {
	var out SubtitleCues
	return &out, c.do(ctx, request{method: http.MethodGet, path: subtitlesPath(bucket, lang), query: versionQuery(key, versionID)}, &out)
}

// ExportSubtitles implements AdminClient.
func (c *client) ExportSubtitles(ctx context.Context, bucket, key, versionID, lang, format string) ([]byte, error) {
	q := versionQuery(key, versionID)
	q.Set("format", format)
	data, _, err := c.send(ctx, request{method: http.MethodGet, path: subtitlesPath(bucket, lang), query: q})
	return data, err
}

// PutSubtitles implements AdminClient.
func (c *client) PutSubtitles(ctx context.Context, bucket, key, versionID, lang string, cues []SubtitleCue) (*SubtitleTrack, error) {
	var out SubtitleTrack
	if cues == nil {
		cues = []SubtitleCue{}
	}
	body := struct {
		Cues []SubtitleCue `json:"cues"`
	}{cues}
	return &out, c.do(ctx, request{method: http.MethodPut, path: subtitlesPath(bucket, lang), query: versionQuery(key, versionID), body: body}, &out)
}

// ImportSubtitles implements AdminClient.
func (c *client) ImportSubtitles(ctx context.Context, bucket, key, versionID, lang string, doc []byte) (*SubtitleTrack, error) {
	var out SubtitleTrack
	return &out, c.do(ctx, request{method: http.MethodPut, path: subtitlesPath(bucket, lang), query: versionQuery(key, versionID),
		raw: doc, contentType: "text/vtt"}, &out)
}

// DeleteSubtitles implements AdminClient.
func (c *client) DeleteSubtitles(ctx context.Context, bucket, key, versionID, lang string) error {
	return c.do(ctx, request{method: http.MethodDelete, path: subtitlesPath(bucket, lang), query: versionQuery(key, versionID)}, nil)
}
