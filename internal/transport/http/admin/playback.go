package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/playback"
)

// PlaybackTokenRequest is the body of POST
// /buckets/{bucket}/objects/playback-tokens (the video in ?key= and
// ?versionId=).
type PlaybackTokenRequest struct {
	// Subject identifies the viewer for the application (required).
	Subject string `json:"subject"`
	// Watermark is the text the player draws over the video (default:
	// the subject).
	Watermark string `json:"watermark,omitempty"`
	// TTLSeconds is the validity of the token (default 2 h, at most the
	// token TTL of the protection of the bucket).
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

// PlaybackTokenResponse is a playback token and where it plays.
type PlaybackTokenResponse struct {
	Token     string    `json:"token"`
	SessionID string    `json:"sessionId"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Playlist is the master playlist of the video with the token, for any
	// HLS player; Player the page of the Nexo player. They are absolute
	// when the server knows its public URL, else paths of the S3 listener.
	Playlist string `json:"playlist"`
	Player   string `json:"player"`
}

func (h *mediaRoutes) playback(w http.ResponseWriter, r *http.Request) (playback.Service, bool) {
	if h.deps.Playback == nil {
		h.fail(w, r, fmt.Errorf("%w: playback tokens are not enabled", domain.ErrNotFound))
		return nil, false
	}
	return h.deps.Playback, true
}

func (h *mediaRoutes) issueToken(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.playback(w, r)
	if !ok {
		return
	}
	var in PlaybackTokenRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if in.TTLSeconds < 0 {
		h.fail(w, r, fmt.Errorf("%w: ttlSeconds must be positive", domain.ErrInvalidArgument))
		return
	}
	q := r.URL.Query()
	bucket := bucketName(r)
	tok, err := svc.Issue(r.Context(), playback.IssueRequest{Bucket: bucket, Key: q.Get("key"), VersionID: q.Get("versionId"),
		Subject: in.Subject, Watermark: in.Watermark, TTL: time.Duration(in.TTLSeconds) * time.Second})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	master := url.Values{"key": {tok.Claims.Key}, "pt": {tok.Token}}
	if tok.Claims.VersionID != "" {
		master.Set("versionId", tok.Claims.VersionID)
	}
	playlist := domain.MediaMasterPath(bucket) + "?" + master.Encode()
	base := strings.TrimSuffix(h.deps.ServerURL, "/")
	h.deps.Enc.WriteJSON(w, http.StatusCreated, PlaybackTokenResponse{
		Token: tok.Token, SessionID: tok.Claims.SessionID, ExpiresAt: time.Unix(tok.Claims.ExpiresAt, 0).UTC(),
		Playlist: base + playlist, Player: base + domain.MediaPlayerPath + "#" + playlist,
	})
}

func (h *mediaRoutes) revokeSession(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.playback(w, r)
	if !ok {
		return
	}
	session, err := pathParam(r, "session")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := svc.Revoke(r.Context(), session); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *mediaRoutes) revokeSubject(w http.ResponseWriter, r *http.Request) {
	svc, ok := h.playback(w, r)
	if !ok {
		return
	}
	subject, err := pathParam(r, "subject")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := svc.RevokeSubject(r.Context(), bucketName(r), subject); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
