package admin

import (
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// SubtitleCue is a cue of a subtitle track: its times in seconds from
// the start of the video.
type SubtitleCue struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
	// Settings are the WebVTT cue settings (line:0 align:start...).
	Settings string `json:"settings,omitempty"`
}

// SubtitleTrackResponse describes a subtitle track of a video.
type SubtitleTrackResponse struct {
	Lang string `json:"lang"`
	// Name is the name of the language in that language.
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

// SubtitlesResponse lists the subtitle tracks of a video (GET
// /buckets/{bucket}/objects/subtitles).
type SubtitlesResponse struct {
	Tracks []SubtitleTrackResponse `json:"tracks"`
}

// SubtitleCuesResponse is a subtitle track and its cues (GET
// /buckets/{bucket}/objects/subtitles/{lang}).
type SubtitleCuesResponse struct {
	Track SubtitleTrackResponse `json:"track"`
	Cues  []SubtitleCue         `json:"cues"`
}

// SubtitleCuesRequest is the JSON body of PUT
// /buckets/{bucket}/objects/subtitles/{lang}.
type SubtitleCuesRequest struct {
	Cues []SubtitleCue `json:"cues"`
}

func newSubtitleTrackResponse(t domain.MediaSubtitleTrack) SubtitleTrackResponse {
	out := SubtitleTrackResponse{Lang: t.Lang, Name: domain.LanguageName(t.Lang), Auto: t.Auto, From: t.From, Engine: t.Engine,
		Cues: t.Cues, Size: t.Size, UpdatedBy: t.UpdatedBy}
	if !t.Updated.IsZero() {
		u := t.Updated.UTC()
		out.Updated = &u
	}
	return out
}

// seconds converts a duration to seconds, to the millisecond.
func seconds(d time.Duration) float64 { return float64(d.Milliseconds()) / 1000 }

// fromSeconds converts seconds to a duration, to the millisecond.
func fromSeconds(s float64) (time.Duration, error) {
	if math.IsNaN(s) || s < 0 || s > 360000 {
		return 0, fmt.Errorf("%w: a cue time must be between 0 and 100 hours", domain.ErrInvalidArgument)
	}
	return time.Duration(math.Round(s*1000)) * time.Millisecond, nil
}

// subtitleSource returns the video of a subtitle request.
func subtitleSource(r *http.Request) (media.Source, error) {
	src, err := hlsSource(r, false)
	src.Revision, src.Rendition = "", ""
	return src, err
}

// subtitles lists the tracks of a video.
func (h *objectRoutes) subtitles(w http.ResponseWriter, r *http.Request) {
	src, err := h.subtitleRequest(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	tracks, err := h.deps.Media.Subtitles(r.Context(), src)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := SubtitlesResponse{Tracks: make([]SubtitleTrackResponse, 0, len(tracks))}
	for _, t := range tracks {
		out.Tracks = append(out.Tracks, newSubtitleTrackResponse(t))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// subtitleRequest returns the video of a subtitle request; 404 when the
// server has no media service.
func (h *objectRoutes) subtitleRequest(r *http.Request) (media.Source, error) {
	if h.deps.Media == nil {
		return media.Source{}, fmt.Errorf("%w: the media outputs are not enabled", domain.ErrNotFound)
	}
	return subtitleSource(r)
}

// subtitleCues serves a track: JSON cues, or ?format=vtt|srt the
// document to download.
func (h *objectRoutes) subtitleCues(w http.ResponseWriter, r *http.Request) {
	src, err := h.subtitleRequest(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	lang, err := pathParam(r, "lang")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "vtt" && format != "srt" {
		h.fail(w, r, fmt.Errorf("%w: format must be json, vtt or srt", domain.ErrInvalidArgument))
		return
	}
	t, cues, err := h.deps.Media.SubtitleCues(r.Context(), src, lang)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if format == "vtt" || format == "srt" {
		body, contentType := domain.FormatWebVTT(cues), contentTypeVTT
		if format == "srt" {
			body, contentType = domain.FormatSRT(cues), "application/x-subrip; charset=utf-8"
		}
		hdr := w.Header()
		hdr.Set("Content-Type", contentType)
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": t.Lang + "." + format}))
		hdr.Set("Cache-Control", "private, no-cache")
		hdr.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	out := SubtitleCuesResponse{Track: newSubtitleTrackResponse(t), Cues: make([]SubtitleCue, 0, len(cues))}
	for _, c := range cues {
		out.Cues = append(out.Cues, SubtitleCue{Start: seconds(c.Start), End: seconds(c.End), Text: c.Text, Settings: c.Settings})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// putSubtitles stores a manual track: a JSON list of cues, or a WebVTT or
// SubRip document (any other content type).
func (h *objectRoutes) putSubtitles(w http.ResponseWriter, r *http.Request) {
	src, err := h.subtitleRequest(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	lang, err := pathParam(r, "lang")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var cues []domain.SubtitleCue
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct == "application/json" {
		var in SubtitleCuesRequest
		if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
			h.fail(w, r, err)
			return
		}
		for _, c := range in.Cues {
			start, err := fromSeconds(c.Start)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			end, err := fromSeconds(c.End)
			if err != nil {
				h.fail(w, r, err)
				return
			}
			cues = append(cues, domain.SubtitleCue{Start: start, End: end, Text: c.Text, Settings: c.Settings})
		}
	} else {
		data, err := readBody(r, domain.MaxSubtitleBytes)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if cues, err = domain.ParseSubtitles(data); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	t, err := h.deps.Media.PutSubtitles(r.Context(), src, lang, cues, caller(r).EffectiveUser())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newSubtitleTrackResponse(t))
}

// deleteSubtitles removes a track.
func (h *objectRoutes) deleteSubtitles(w http.ResponseWriter, r *http.Request) {
	src, err := h.subtitleRequest(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	lang, err := pathParam(r, "lang")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Media.DeleteSubtitles(r.Context(), src, lang); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
