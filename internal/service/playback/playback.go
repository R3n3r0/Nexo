package playback

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// tokenVersion is the version of the payload of the tokens.
const tokenVersion = 1

// tokenKeyLabel separates the key signing the tokens from the other uses
// of the media protection key.
const tokenKeyLabel = "nexo/media/playback-token-v1"

// Limits of the fields of a token.
const (
	maxSubject   = 256
	maxWatermark = 200
)

// Documents of the system store: the revoked sessions, the revoked
// viewers and the streams of the viewers.
const (
	docsRevoked  = "media/playback/revoked"
	docsSubjects = "media/playback/subjects"
	docsViewers  = "media/playback/viewers"
)

// Protections returns the download protection of the videos of a bucket
// (the bucket service): a disabled protection when the bucket has none.
type Protections interface {
	GetMediaProtection(ctx context.Context, bucket string) (domain.MediaProtection, error)
}

// Deps are the dependencies of the service.
type Deps struct {
	// Protections bounds the TTL of the tokens of protected videos
	// (required).
	Protections Protections
	// Store keeps the revocations and the streams of the viewers, shared
	// by the nodes (nil: in memory, forgotten on restart).
	Store sysstore.Store
	// Metrics counts the keys served and refused (optional).
	Metrics metrics.Registry
	Clock   clock.Clock
	Logger  *slog.Logger
}

type service struct {
	deps Deps
	opts options
	key  []byte
	keys metrics.Counter

	mu sync.Mutex
	// revoked are the revoked sessions (expiry) and subjects (revoked
	// before), loaded from the store at most every opts.refresh.
	revoked     map[string]time.Time
	subjects    map[string]time.Time
	revokedLoad time.Time
	// viewers are the streams of the viewers, by document, without a
	// store (with one, they are read from it).
	viewers map[string]viewerDoc
	// pace are the periods each stream fetched, on this node.
	pace map[string]*paceState
}

var _ Service = (*service)(nil)

// viewerDoc are the streams of a viewer of a bucket: the live ones, the
// oldest first, and those a newer one replaced, until they expire.
type viewerDoc struct {
	Sessions []viewerSession `json:"sessions,omitempty"`
	Evicted  []viewerSession `json:"evicted,omitempty"`
}

type viewerSession struct {
	ID        string `json:"sid"`
	Start     int64  `json:"start"`
	ExpiresAt int64  `json:"exp"`
}

// paceState are the key periods a stream fetched since its first key.
type paceState struct {
	first   time.Time
	expires time.Time
	periods map[int]struct{}
}

// revocation is a stored revocation: of a session until ExpiresAt, of a
// subject for the tokens issued up to Before.
type revocation struct {
	Before    int64 `json:"before,omitempty"`
	ExpiresAt int64 `json:"exp"`
}

// New returns the playback token service. The key (WithKey) is required:
// domain.ErrInvalidArgument without it or without Protections.
func New(deps Deps, opts ...Option) (Service, error) {
	o := newOptions(opts)
	if deps.Protections == nil || len(o.key) < 16 {
		return nil, fmt.Errorf("%w: playback tokens require the bucket protections and a key", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	mac := hmac.New(sha256.New, o.key)
	mac.Write([]byte(tokenKeyLabel))
	s := &service{deps: deps, opts: o, key: mac.Sum(nil),
		revoked: map[string]time.Time{}, subjects: map[string]time.Time{},
		viewers: map[string]viewerDoc{}, pace: map[string]*paceState{}}
	if deps.Metrics != nil {
		var err error
		if s.keys, err = deps.Metrics.Counter(context.Background(), metrics.Definition{
			Subsystem: "media", Name: "keys_total",
			Help:   "Keys of protected videos asked with a playback token, by result (ok, sessions, pace).",
			Labels: []string{"result"},
		}); err != nil {
			return nil, fmt.Errorf("playback: metrics: %w", err)
		}
	}
	return s, nil
}

// Issue implements Service.
func (s *service) Issue(ctx context.Context, req IssueRequest) (*Token, error) {
	if err := domain.ValidateObjectKey(req.Key); err != nil {
		return nil, err
	}
	if req.Bucket == "" {
		return nil, fmt.Errorf("%w: playback token: the bucket is required", domain.ErrInvalidArgument)
	}
	if err := checkText("subject", req.Subject, maxSubject, true); err != nil {
		return nil, err
	}
	if err := checkText("watermark", req.Watermark, maxWatermark, false); err != nil {
		return nil, err
	}
	p, err := s.deps.Protections.GetMediaProtection(ctx, req.Bucket)
	if err != nil {
		return nil, err
	}
	limit := domain.MaxPlaybackTokenTTL
	if p.Protects(req.Key) {
		limit = p.TokenTTL()
	}
	ttl := req.TTL
	switch {
	case ttl == 0:
		ttl = min(domain.DefaultPlaybackTokenTTL, limit)
	case ttl < time.Minute || ttl > limit:
		return nil, fmt.Errorf("%w: playback token: the TTL must be 1m to %s", domain.ErrInvalidArgument, limit)
	}
	sid := make([]byte, 16)
	if _, err := rand.Read(sid); err != nil {
		return nil, fmt.Errorf("playback: session id: %w", err)
	}
	now := s.deps.Clock.Now()
	c := Claims{Version: tokenVersion, Bucket: req.Bucket, Key: req.Key, VersionID: req.VersionID,
		Subject: req.Subject, Watermark: req.Watermark, SessionID: hex.EncodeToString(sid),
		IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()}
	payload, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("playback: encode token: %w", err)
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	return &Token{Token: enc + "." + base64.RawURLEncoding.EncodeToString(s.sign(enc)), Claims: c}, nil
}

// checkText checks a printable field of a token.
func checkText(name, v string, limit int, required bool) error {
	switch {
	case required && strings.TrimSpace(v) == "":
		return fmt.Errorf("%w: playback token: the %s is required", domain.ErrInvalidArgument, name)
	case len(v) > limit:
		return fmt.Errorf("%w: playback token: the %s is longer than %d bytes", domain.ErrInvalidArgument, name, limit)
	case !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0:
		return fmt.Errorf("%w: playback token: the %s holds control characters", domain.ErrInvalidArgument, name)
	}
	return nil
}

func (s *service) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

// Verify implements Service.
func (s *service) Verify(ctx context.Context, token, bucket, key, versionID string) (*Claims, error) {
	c, err := s.parse(token)
	if err != nil {
		return nil, err
	}
	now := s.deps.Clock.Now()
	switch {
	case now.Unix() >= c.ExpiresAt:
		return nil, fmt.Errorf("%w: the playback token has expired", domain.ErrAccessDenied)
	case c.Bucket != bucket || c.Key != key || (c.VersionID != "" && c.VersionID != versionID):
		return nil, fmt.Errorf("%w: the playback token is for another video", domain.ErrAccessDenied)
	}
	revoked, err := s.isRevoked(ctx, c, now)
	if err != nil {
		return nil, err
	}
	if revoked {
		return nil, fmt.Errorf("%w: the playback token was revoked", domain.ErrAccessDenied)
	}
	return c, nil
}

// parse checks the signature of a token and decodes its claims.
func (s *service) parse(token string) (*Claims, error) {
	invalid := fmt.Errorf("%w: invalid playback token", domain.ErrAccessDenied)
	enc, sig, ok := strings.Cut(token, ".")
	if !ok || len(token) > 4096 {
		return nil, invalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(enc)) {
		return nil, invalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return nil, invalid
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.Version != tokenVersion || c.SessionID == "" {
		return nil, invalid
	}
	return &c, nil
}

// subjectID names a viewer in the documents (the subject may hold any
// character).
func subjectID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// isRevoked reports whether the token was revoked, alone or with its
// viewer. A store failure fails closed.
func (s *service) isRevoked(ctx context.Context, c *Claims, now time.Time) (bool, error) {
	if err := s.loadRevocations(ctx, now); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.revoked[c.SessionID]; ok {
		return true, nil
	}
	before, ok := s.subjects[subjectID(c.Bucket, c.Subject)]
	return ok && c.IssuedAt <= before.Unix(), nil
}

// loadRevocations reads the revocations of the store when the cache is
// older than the refresh interval.
func (s *service) loadRevocations(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	fresh := s.deps.Store == nil || now.Sub(s.revokedLoad) < s.opts.refresh
	s.mu.Unlock()
	if fresh {
		return nil
	}
	sessions, err := s.list(ctx, docsRevoked)
	if err != nil {
		return err
	}
	subjects, err := s.list(ctx, docsSubjects)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked, s.subjects = map[string]time.Time{}, map[string]time.Time{}
	for id, r := range sessions {
		s.revoked[id] = time.Unix(r.ExpiresAt, 0)
	}
	for id, r := range subjects {
		s.subjects[id] = time.Unix(r.Before, 0)
	}
	s.revokedLoad = now
	return nil
}

// list reads the revocations of a directory of the store, skipping the
// expired ones and the damaged ones.
func (s *service) list(ctx context.Context, dir string) (map[string]revocation, error) {
	docs, err := s.deps.Store.List(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("playback: read revocations: %w", err)
	}
	now := s.deps.Clock.Now().Unix()
	out := make(map[string]revocation, len(docs))
	for id, raw := range docs {
		var r revocation
		if json.Unmarshal(raw, &r) != nil || r.ExpiresAt <= now {
			continue
		}
		out[id] = r
	}
	return out, nil
}

// Revoke implements Service.
func (s *service) Revoke(ctx context.Context, sessionID string) error {
	if sessionID == "" || len(sessionID) > 64 || strings.Trim(sessionID, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: invalid session id %q", domain.ErrInvalidArgument, sessionID)
	}
	exp := s.deps.Clock.Now().Add(domain.MaxPlaybackTokenTTL)
	if err := s.store(ctx, docsRevoked+"/"+sessionID, revocation{ExpiresAt: exp.Unix()}); err != nil {
		return err
	}
	s.mu.Lock()
	s.revoked[sessionID] = exp
	s.mu.Unlock()
	s.purge(ctx)
	return nil
}

// RevokeSubject implements Service.
func (s *service) RevokeSubject(ctx context.Context, bucket, subject string) error {
	if err := checkText("subject", subject, maxSubject, true); err != nil {
		return err
	}
	now := s.deps.Clock.Now()
	id := subjectID(bucket, subject)
	if err := s.store(ctx, docsSubjects+"/"+id, revocation{Before: now.Unix(), ExpiresAt: now.Add(domain.MaxPlaybackTokenTTL).Unix()}); err != nil {
		return err
	}
	s.mu.Lock()
	s.subjects[id] = now
	s.mu.Unlock()
	s.purge(ctx)
	return nil
}

// store writes a document of the store (nothing without a store).
func (s *service) store(ctx context.Context, path string, v any) error {
	if s.deps.Store == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("playback: encode %s: %w", path, err)
	}
	if err := s.deps.Store.Put(ctx, path, data); err != nil {
		return fmt.Errorf("playback: store %s: %w", path, err)
	}
	return nil
}

// purge deletes the expired revocations of the store; a failure is
// logged (retried at the next revocation).
func (s *service) purge(ctx context.Context) {
	if s.deps.Store == nil {
		return
	}
	now := s.deps.Clock.Now().Unix()
	for _, dir := range []string{docsRevoked, docsSubjects} {
		docs, err := s.deps.Store.List(ctx, dir)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "playback: list revocations", "err", err)
			return
		}
		for id, raw := range docs {
			var r revocation
			if json.Unmarshal(raw, &r) == nil && r.ExpiresAt > now {
				continue
			}
			if err := s.deps.Store.Delete(ctx, dir+"/"+id); err != nil {
				s.deps.Logger.WarnContext(ctx, "playback: purge revocation", "id", id, "err", err)
			}
		}
	}
}

// AuthorizeKey implements Service.
func (s *service) AuthorizeKey(ctx context.Context, c *Claims, p *domain.MediaProtection, period int, periodLength time.Duration) error {
	if err := s.authorizeStream(ctx, c, p); err != nil {
		s.count(ctx, "sessions")
		return err
	}
	if err := s.authorizePace(c, p, period, periodLength); err != nil {
		s.count(ctx, "pace")
		return err
	}
	s.count(ctx, "ok")
	return nil
}

func (s *service) count(ctx context.Context, result string) {
	if s.keys != nil {
		s.keys.Add(ctx, 1, result)
	}
}

// authorizeStream counts the stream of the token among those of its
// viewer: a new stream beyond MaxSessions replaces the oldest one, which
// gets no key afterwards.
func (s *service) authorizeStream(ctx context.Context, c *Claims, p *domain.MediaProtection) error {
	if p.MaxSessions < 0 {
		return nil
	}
	limit := p.MaxSessions
	if limit == 0 {
		limit = domain.DefaultMediaSessions
	}
	now := s.deps.Clock.Now()
	id := subjectID(c.Bucket, c.Subject)
	doc, err := s.viewer(ctx, id)
	if err != nil {
		return err
	}
	if doc.evicted(c.SessionID) {
		return fmt.Errorf("%w: a newer stream of this viewer replaced this one (at most %d at a time)", domain.ErrAccessDenied, limit)
	}
	if doc.live(c.SessionID) {
		return nil
	}
	doc.expire(now.Unix())
	doc.Sessions = append(doc.Sessions, viewerSession{ID: c.SessionID, Start: now.Unix(), ExpiresAt: c.ExpiresAt})
	for len(doc.Sessions) > limit {
		doc.Evicted = append(doc.Evicted, doc.Sessions[0])
		doc.Sessions = doc.Sessions[1:]
	}
	if s.deps.Store == nil {
		s.mu.Lock()
		s.viewers[id] = doc
		s.mu.Unlock()
		return nil
	}
	return s.store(ctx, docsViewers+"/"+id, doc)
}

// viewer returns the streams of a viewer: always the stored document,
// since another node may have replaced the stream since (a key is asked
// once every KeyPeriod segments: the read is cheap), or the in-memory one
// without a store.
func (s *service) viewer(ctx context.Context, id string) (viewerDoc, error) {
	if s.deps.Store == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.viewers[id].clone(), nil
	}
	var doc viewerDoc
	raw, err := s.deps.Store.Get(ctx, docsViewers+"/"+id)
	switch {
	case errors.Is(err, sysstore.ErrNotFound) || errors.Is(err, domain.ErrNotFound):
	case err != nil:
		return viewerDoc{}, fmt.Errorf("playback: read viewer streams: %w", err)
	default:
		if err := json.Unmarshal(raw, &doc); err != nil {
			s.deps.Logger.WarnContext(ctx, "playback: damaged viewer streams, reset", "id", id, "err", err)
			doc = viewerDoc{}
		}
	}
	return doc, nil
}

func (d viewerDoc) live(sid string) bool {
	for _, v := range d.Sessions {
		if v.ID == sid {
			return true
		}
	}
	return false
}

func (d viewerDoc) evicted(sid string) bool {
	for _, v := range d.Evicted {
		if v.ID == sid {
			return true
		}
	}
	return false
}

// expire drops the expired streams.
func (d *viewerDoc) expire(now int64) {
	keep := func(in []viewerSession) []viewerSession {
		out := in[:0]
		for _, v := range in {
			if v.ExpiresAt > now {
				out = append(out, v)
			}
		}
		return out
	}
	d.Sessions, d.Evicted = keep(d.Sessions), keep(d.Evicted)
}

func (d viewerDoc) clone() viewerDoc {
	return viewerDoc{Sessions: append([]viewerSession(nil), d.Sessions...), Evicted: append([]viewerSession(nil), d.Evicted...)}
}

// authorizePace bounds the distinct key periods a stream fetched to the
// burst plus PaceFactor times the periods played since its first key: a
// player seeking around stays under it, a downloader asking for every key
// stops after the burst.
func (s *service) authorizePace(c *Claims, p *domain.MediaProtection, period int, periodLength time.Duration) error {
	if periodLength <= 0 {
		periodLength = time.Minute
	}
	factor := p.PaceFactor
	if factor <= 0 {
		factor = domain.DefaultMediaPaceFactor
	}
	now := s.deps.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.pace[c.SessionID]
	if !ok {
		if len(s.pace) >= s.opts.maxEntries {
			for id, v := range s.pace {
				if !now.Before(v.expires) {
					delete(s.pace, id)
				}
			}
		}
		st = &paceState{first: now, expires: time.Unix(c.ExpiresAt, 0), periods: map[int]struct{}{}}
		s.pace[c.SessionID] = st
	}
	if _, seen := st.periods[period]; seen {
		return nil
	}
	allowed := s.opts.burst + int(int64(factor)*int64(now.Sub(st.first))/int64(periodLength))
	if len(st.periods) >= allowed {
		return fmt.Errorf("%w: the keys of this video are asked faster than it plays", domain.ErrTooManyRequests)
	}
	st.periods[period] = struct{}{}
	return nil
}
