package s3

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/playback"
)

// The session channel of the player of Nexo (the advanced playback,
// docs/proposals/video-protection.md §10): a WebSocket on
// /nexo/media/v1/{bucket}/session?key=...&pt=TOKEN, or, behind a proxy
// that cannot carry one, the same messages as POST requests
// (/session to open, /session/key?st=TICKET for the keys).
//
//	player → {"t":"hello","v":1,"pub":"<ECDH P-256 public key>"}
//	server → {"t":"ready","pub":"...","salt":"...","session":"...","ticket":"...","master":"/nexo/media/v1/...&st=...","exp":...,"period":10}
//	player → {"t":"key","id":7,"rev":"<ETag>","r":"720p","i":3}
//	server → {"t":"key","id":7,"key":"<wrapped key>"}  or  {"t":"error","id":7,"code":"too-fast","status":429,...}
//	server → {"t":"closed","code":"denied","status":403,...}  (revoked, replaced, expired, node stopping)
//
// The wrapped keys are AES-128-GCM under the wrapping key both sides
// derive from the ECDH secret (HKDF-SHA256, the salt, the session id),
// bound to sessionKeyAAD of the key: the keys never travel in clear.

// ticketParam is the query parameter of the session tickets.
const ticketParam = "st"

// Bounds of the session channel.
const (
	// sessionHelloTimeout bounds the wait for the hello of the player.
	sessionHelloTimeout = 10 * time.Second
	// sessionPing is the interval of the pings that keep the channel
	// open through the proxies.
	sessionPing = 30 * time.Second
	// sessionMessageLimit bounds a message of the player.
	sessionMessageLimit = 4 << 10
	// sessionVersion is the version of the protocol.
	sessionVersion = 1
)

// sessionHello opens a session.
type sessionHello struct {
	T   string `json:"t"`
	V   int    `json:"v"`
	Pub string `json:"pub"`
}

// sessionReady answers a hello.
type sessionReady struct {
	T      string `json:"t"`
	V      int    `json:"v"`
	Pub    string `json:"pub"`
	Salt   string `json:"salt"`
	ID     string `json:"session"`
	Ticket string `json:"ticket"`
	// Master is the master playlist of the session (a path and a query,
	// on the origin of the channel).
	Master string `json:"master"`
	// ExpiresAt is the end of the session (that of its token), Unix
	// seconds.
	ExpiresAt int64 `json:"exp"`
	// Period is the number of segments of a key period (media.KeyPeriod):
	// segment n is encrypted with the key of period n/Period.
	Period int `json:"period"`
}

// sessionKeyRequest asks for the key of a period of a presentation.
type sessionKeyRequest struct {
	T   string `json:"t"`
	ID  int    `json:"id"`
	Rev string `json:"rev"`
	R   string `json:"r,omitempty"`
	I   int    `json:"i"`
}

// sessionMessage is an answer of the server: a key, an error or the end
// of the session.
type sessionMessage struct {
	T       string `json:"t"`
	ID      int    `json:"id,omitempty"`
	Key     string `json:"key,omitempty"`
	Code    string `json:"code,omitempty"`
	Status  int    `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// sessionKeyAAD is the additional data a wrapped key is bound to: the
// presentation (revision and rendition) and the period.
func sessionKeyAAD(rev, rendition string, period int) string {
	return "nexo/media/key\x00" + rev + "\x00" + rendition + "\x00" + strconv.Itoa(period)
}

// sessionError is the message of an error, with the code the player shows.
func sessionError(id int, err error) sessionMessage {
	status, code := http.StatusInternalServerError, "failed"
	switch {
	case errors.Is(err, domain.ErrAccessDenied):
		status, code = http.StatusForbidden, "denied"
	case errors.Is(err, domain.ErrTooManyRequests):
		status, code = http.StatusTooManyRequests, "too-fast"
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrObjectNotFound):
		status, code = http.StatusNotFound, "not-found"
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrPreconditionFailed):
		status, code = http.StatusBadRequest, "invalid"
	}
	return sessionMessage{T: "error", ID: id, Code: code, Status: status, Message: err.Error()}
}

// sessionSource is the video a session request names (?key=, ?versionId=).
func sessionSource(r *http.Request) (media.Source, error) {
	q := r.URL.Query()
	src := media.Source{Bucket: chi.URLParam(r, "bucket"), Key: q.Get("key"), VersionID: q.Get("versionId")}
	return src, domain.ValidateObjectKey(src.Key)
}

// sessionProtection returns the protection of the video of a session
// request, refusing a video that is not protected and an origin the
// protection does not allow.
func (h *mediaRoutes) sessionProtection(r *http.Request, src media.Source) (*domain.MediaProtection, error) {
	if h.deps.Playback == nil {
		return nil, fmt.Errorf("%w: playback tokens are not enabled", domain.ErrAccessDenied)
	}
	prot, err := h.deps.Media.Protection(r.Context(), src)
	if err != nil {
		return nil, err
	}
	if prot == nil {
		return nil, fmt.Errorf("%w: %s is not a protected video: it plays without a session", domain.ErrNotFound, src.Key)
	}
	if !prot.AllowsOrigin(r.Header.Get("Origin")) {
		return nil, fmt.Errorf("%w: origin not allowed", domain.ErrAccessDenied)
	}
	return prot, nil
}

// openSession answers the hello of a player with the claims of its token.
func (h *mediaRoutes) openSession(ctx context.Context, src media.Source, claims *playback.Claims, hello sessionHello) (*sessionReady, *playback.Claims, error) {
	if hello.T != "hello" || hello.V != sessionVersion {
		return nil, nil, fmt.Errorf("%w: expected a hello of version %d", domain.ErrInvalidArgument, sessionVersion)
	}
	pub, err := base64.RawURLEncoding.DecodeString(hello.Pub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: malformed public key", domain.ErrInvalidArgument)
	}
	s, err := h.deps.Playback.OpenSession(ctx, claims, pub)
	if err != nil {
		return nil, nil, err
	}
	// The claims of the ticket, as every request of the session verifies
	// them (they hold the wrapping key).
	sc, err := h.deps.Playback.VerifyTicket(ctx, s.Ticket, src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return nil, nil, err
	}
	q := url.Values{"key": {src.Key}, ticketParam: {s.Ticket}}
	if src.VersionID != "" {
		q.Set("versionId", src.VersionID)
	}
	return &sessionReady{
		T: "ready", V: sessionVersion, Pub: base64.RawURLEncoding.EncodeToString(s.ServerKey),
		Salt: base64.RawURLEncoding.EncodeToString(s.Salt), ID: s.Claims.Session, Ticket: s.Ticket,
		Master:    MediaPrefix + url.PathEscape(src.Bucket) + "/master.m3u8?" + q.Encode(),
		ExpiresAt: s.Claims.ExpiresAt, Period: media.KeyPeriod,
	}, sc, nil
}

// sessionKey serves a key of a session, wrapped: the limits of the
// protection apply as to the keys of a token.
func (h *mediaRoutes) sessionKey(ctx context.Context, src media.Source, claims *playback.Claims, prot *domain.MediaProtection, req sessionKeyRequest) sessionMessage {
	if req.Rev == "" || req.I < 0 {
		return sessionError(req.ID, fmt.Errorf("%w: rev and a non-negative i are required", domain.ErrInvalidArgument))
	}
	src.Revision, src.Rendition, src.Session, src.Viewer = req.Rev, req.R, claims.Session, claims.SessionID
	p, err := h.deps.Media.Describe(ctx, src)
	if err != nil {
		return sessionError(req.ID, err)
	}
	if err := h.deps.Playback.AuthorizeKey(ctx, claims, prot, req.I, media.PeriodLength(p, 0)); err != nil {
		return sessionError(req.ID, err)
	}
	key, err := h.deps.Media.Key(ctx, src, req.I)
	if err != nil {
		return sessionError(req.ID, err)
	}
	wrapped, err := h.deps.Playback.WrapKey(claims, key, sessionKeyAAD(req.Rev, req.R, req.I))
	if err != nil {
		return sessionError(req.ID, err)
	}
	return sessionMessage{T: "key", ID: req.ID, Key: base64.RawURLEncoding.EncodeToString(wrapped)}
}

// session serves the WebSocket of a session; a request that is no
// WebSocket handshake is refused.
func (h *mediaRoutes) session(w http.ResponseWriter, r *http.Request) {
	src, err := sessionSource(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	prot, err := h.sessionProtection(r, src)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	claims, err := h.deps.Playback.Verify(r.Context(), r.URL.Query().Get(tokenParam), src.Bucket, src.Key, src.VersionID)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: a WebSocket handshake is expected (or POST the hello)", domain.ErrInvalidArgument))
		return
	}
	// The origin was checked against the protection: the token is the
	// capability, any origin may hold one when the protection lists none.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.deps.Logger.DebugContext(r.Context(), "s3: media session: handshake", "err", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(sessionMessageLimit)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	h.runSession(ctx, conn, src, claims, prot)
}

// runSession runs the channel of a session: the hello, then the keys
// until the player leaves, the stream may no longer play, or the node
// stops.
func (h *mediaRoutes) runSession(ctx context.Context, conn *websocket.Conn, src media.Source, claims *playback.Claims, prot *domain.MediaProtection) {
	write := func(m any) error {
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, sessionHelloTimeout)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, data)
	}
	end := func(err error) {
		m := sessionError(0, err)
		m.T = "closed"
		_ = write(m)
		_ = conn.Close(websocket.StatusPolicyViolation, m.Code)
	}

	hctx, cancel := context.WithTimeout(ctx, sessionHelloTimeout)
	_, data, err := conn.Read(hctx)
	cancel()
	if err != nil {
		return
	}
	var hello sessionHello
	if err := json.Unmarshal(data, &hello); err != nil {
		end(fmt.Errorf("%w: malformed hello", domain.ErrInvalidArgument))
		return
	}
	ready, sc, err := h.openSession(ctx, src, claims, hello)
	if err != nil {
		end(err)
		return
	}
	if err := write(ready); err != nil {
		return
	}

	// The player's messages, read on their own goroutine.
	type incoming struct {
		data []byte
		err  error
	}
	in := make(chan incoming)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			select {
			case in <- incoming{data, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	every := h.deps.SessionAlive
	if every <= 0 {
		every = playback.DefaultRefresh
	}
	alive := time.NewTicker(every)
	defer alive.Stop()
	ping := time.NewTicker(sessionPing)
	defer ping.Stop()
	expiry := time.NewTimer(time.Until(time.Unix(sc.ExpiresAt, 0)))
	defer expiry.Stop()
	for {
		select {
		case m := <-in:
			if m.err != nil {
				return
			}
			var req sessionKeyRequest
			if err := json.Unmarshal(m.data, &req); err != nil || req.T != "key" {
				end(fmt.Errorf("%w: unexpected message", domain.ErrInvalidArgument))
				return
			}
			if err := write(h.sessionKey(ctx, src, sc, prot, req)); err != nil {
				return
			}
		case <-alive.C:
			current, err := h.deps.Media.Protection(ctx, src)
			if err == nil && current == nil {
				err = fmt.Errorf("%w: the video is no longer protected", domain.ErrAccessDenied)
			}
			if err == nil {
				prot = current
				err = h.deps.Playback.Alive(ctx, sc, prot)
			}
			if errors.Is(err, domain.ErrAccessDenied) {
				end(err)
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, sessionHelloTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-expiry.C:
			end(fmt.Errorf("%w: the playback token has expired", domain.ErrAccessDenied))
			return
		case <-h.deps.Stopping:
			_ = conn.Close(websocket.StatusGoingAway, "the server is stopping")
			return
		}
	}
}

// sessionPost opens a session with a POST of the hello (the channel
// without WebSocket).
func (h *mediaRoutes) sessionPost(w http.ResponseWriter, r *http.Request) {
	src, err := sessionSource(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if _, err := h.sessionProtection(r, src); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	claims, err := h.deps.Playback.Verify(r.Context(), r.URL.Query().Get(tokenParam), src.Bucket, src.Key, src.VersionID)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	var hello sessionHello
	if err := readSessionJSON(r, &hello); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	ready, _, err := h.openSession(r.Context(), src, claims, hello)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	writeSessionJSON(w, http.StatusOK, ready)
}

// sessionKeyPost serves a key of a session to a POST (?st= the ticket).
func (h *mediaRoutes) sessionKeyPost(w http.ResponseWriter, r *http.Request) {
	src, err := sessionSource(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	prot, err := h.sessionProtection(r, src)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	claims, err := h.deps.Playback.VerifyTicket(r.Context(), r.URL.Query().Get(ticketParam), src.Bucket, src.Key, src.VersionID)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	var req sessionKeyRequest
	if err := readSessionJSON(r, &req); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	req.T = "key"
	m := h.sessionKey(r.Context(), src, claims, prot, req)
	status := http.StatusOK
	if m.Status != 0 {
		status = m.Status
	}
	writeSessionJSON(w, status, m)
}

// readSessionJSON decodes the JSON body of a session request.
func readSessionJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, sessionMessageLimit+1))
	if err != nil {
		return err
	}
	if len(data) > sessionMessageLimit {
		return fmt.Errorf("%w: the message is larger than %d bytes", domain.ErrInvalidArgument, sessionMessageLimit)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%w: malformed message: %v", domain.ErrInvalidArgument, err)
	}
	return nil
}

func writeSessionJSON(w http.ResponseWriter, status int, v any) {
	data, _ := json.Marshal(v)
	hdr := w.Header()
	hdr.Set("Content-Type", "application/json")
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
