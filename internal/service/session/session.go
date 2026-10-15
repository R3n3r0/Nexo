package session

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// Deps are the dependencies of the session service.
type Deps struct {
	// IAM verifies the credentials and resolves the identities (required).
	IAM iam.Service
	// Store persists the revoked sessions in iam.NamespaceSessions
	// (default: iam.NewMemoryStore(), which forgets them on restart).
	Store  iam.Store
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*service)

// WithSigningKey sets the HMAC key used to sign tokens. A random key is
// generated when none is given (sessions then do not survive restarts).
func WithSigningKey(key []byte) Option {
	return func(s *service) {
		if len(key) > 0 {
			s.key = append([]byte(nil), key...)
		}
	}
}

// sessionKeyInfo is the HKDF context of the session signing keys.
const sessionKeyInfo = "nexo-console-session-v2"

// DeriveKey derives the signing key of the sessions from the root
// credentials and the deployment ID: HKDF-SHA256 with the deployment ID as
// salt. Every node of a deployment accepts the same session cookies, and
// no other deployment does, even with the same root credentials (a cookie
// of staging is not valid in production).
func DeriveKey(accessKey, secretKey, deploymentID string) []byte {
	// The access key is length-prefixed: no two credential pairs share
	// the input key material.
	ikm := binary.BigEndian.AppendUint32(nil, uint32(len(accessKey))) // #nosec G115 -- the length of an access key, bounded by the request limits
	ikm = append(append(ikm, accessKey...), secretKey...)
	k, err := hkdf.Key(sha256.New, ikm, []byte(deploymentID), sessionKeyInfo, 32)
	if err != nil {
		// Only an invalid length fails: 32 bytes of SHA-256 never do.
		panic(fmt.Sprintf("session: derive key: %v", err))
	}
	return k
}

// WithTTL sets the session lifetime (default 12h).
func WithTTL(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.ttl = d
		}
	}
}

// WithIssuer sets the JWT issuer claim (default "nexo").
func WithIssuer(iss string) Option {
	return func(s *service) {
		if iss != "" {
			s.issuer = iss
		}
	}
}

type claims struct {
	jwt.RegisteredClaims
	CSRF   string `json:"csrf"`
	IsRoot bool   `json:"root,omitempty"`
	Parent string `json:"parent,omitempty"`
	// Cred binds the session to the secret key it was opened with
	// (credentialTag): a rotated secret invalidates the session.
	Cred string `json:"cred"`
}

// revocation is the stored record of a revoked session.
type revocation struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// service issues HS256 JWT sessions; the revoked ones are kept in the
// store (and cached in memory) until they expire.
type service struct {
	deps   Deps
	key    []byte
	ttl    time.Duration
	issuer string

	mu sync.Mutex
	// revoked caches the revocations known to this node.
	revoked map[string]time.Time

	// limits configure the throttle, built by New.
	limits throttleLimits
	// throttle locks out the clients that fail too many logins.
	throttle *throttle
}

var _ Service = (*service)(nil)

// New returns a session service.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.IAM == nil {
		return nil, errors.New("session: IAM dependency is required")
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Store == nil {
		deps.Store = iam.NewMemoryStore()
	}
	s := &service{
		deps: deps, ttl: 12 * time.Hour, issuer: "nexo", revoked: make(map[string]time.Time),
		limits: defaultThrottleLimits(),
	}
	for _, o := range opts {
		o(s)
	}
	s.throttle = newThrottle(s.limits)
	if len(s.key) == 0 {
		key, err := randomBytes(32)
		if err != nil {
			return nil, fmt.Errorf("session: generate signing key: %w", err)
		}
		s.key = key
	}
	return s, nil
}

// Login implements Service. Failed attempts count against the (client,
// access key) pair and against the client; a locked-out pair or client is
// refused before its credentials are checked, so that guessing stays
// throttled.
func (s *service) Login(ctx context.Context, in LoginInput) (*Session, error) {
	now := s.deps.Clock.Now()
	if wait := s.throttle.check(in.Client, in.AccessKey, now); wait > 0 {
		return nil, fmt.Errorf("session: %w", &ThrottledError{RetryAfter: wait})
	}
	accessKey := in.AccessKey
	identity, err := s.verify(ctx, in, now)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDenied) {
			if lockout := s.throttle.fail(in.Client, in.AccessKey, now); lockout > 0 {
				s.deps.Logger.WarnContext(ctx, "session: console login locked out after repeated failures",
					"access_key", accessKey, "client", in.Client, "lockout", lockout)
			}
		}
		return nil, err
	}
	s.throttle.reset(in.Client, in.AccessKey)

	id, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("session: generate id: %w", err)
	}
	csrf, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("session: generate csrf token: %w", err)
	}
	exp := now.Add(s.ttl)
	cl := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        id,
			Subject:   accessKey,
			Issuer:    s.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		CSRF:   csrf,
		IsRoot: identity.IsRoot,
		Parent: identity.Parent,
		Cred:   s.credentialTag(accessKey, in.SecretKey),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(s.key)
	if err != nil {
		return nil, fmt.Errorf("session: sign token: %w", err)
	}
	identity.AccessKey = accessKey
	return &Session{
		ID:        id,
		AccessKey: accessKey,
		Identity:  identity,
		IssuedAt:  now,
		ExpiresAt: exp,
		CSRFToken: csrf,
		Token:     token,
	}, nil
}

// verify checks the credentials of a login: unknown, wrong, disabled,
// expired and temporary credentials are domain.ErrAccessDenied.
func (s *service) verify(ctx context.Context, in LoginInput, now time.Time) (domain.Identity, error) {
	if in.AccessKey == "" || in.SecretKey == "" {
		return domain.Identity{}, fmt.Errorf("%w: missing credentials", domain.ErrAccessDenied)
	}
	creds, identity, err := s.deps.IAM.LookupCredentials(ctx, in.AccessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) || errors.Is(err, domain.ErrUserNotFound) {
			return domain.Identity{}, fmt.Errorf("%w: invalid credentials", domain.ErrAccessDenied)
		}
		return domain.Identity{}, fmt.Errorf("session: lookup credentials: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(creds.SecretKey), []byte(in.SecretKey)) != 1 {
		return domain.Identity{}, fmt.Errorf("%w: invalid credentials", domain.ErrAccessDenied)
	}
	if !creds.IsEnabled() {
		return domain.Identity{}, fmt.Errorf("%w: account disabled", domain.ErrAccessDenied)
	}
	if creds.IsExpired(now) {
		return domain.Identity{}, fmt.Errorf("%w: credentials expired", domain.ErrAccessDenied)
	}
	if creds.SessionToken != "" {
		// STS credentials are only valid with their session token, which
		// the console login does not carry.
		return domain.Identity{}, fmt.Errorf("%w: temporary credentials cannot open a console session", domain.ErrAccessDenied)
	}
	return identity, nil
}

// Validate implements Service. The token only proves who logged in: the
// identity (groups, attached policies, status) is resolved again through
// IAM so that revoked permissions and disabled accounts take effect
// immediately.
func (s *service) Validate(ctx context.Context, token string) (*Session, error) {
	cl, err := s.parse(token)
	if err != nil {
		return nil, err
	}
	revoked, err := s.isRevoked(ctx, cl)
	if err != nil {
		return nil, err
	}
	if revoked {
		return nil, fmt.Errorf("%w: session revoked", domain.ErrAccessDenied)
	}
	creds, identity, err := s.deps.IAM.LookupCredentials(ctx, cl.Subject)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) || errors.Is(err, domain.ErrUserNotFound) {
			return nil, fmt.Errorf("%w: account no longer exists", domain.ErrAccessDenied)
		}
		return nil, fmt.Errorf("session: lookup credentials: %w", err)
	}
	if !creds.IsEnabled() {
		return nil, fmt.Errorf("%w: account disabled", domain.ErrAccessDenied)
	}
	if creds.IsExpired(s.deps.Clock.Now()) {
		return nil, fmt.Errorf("%w: credentials expired", domain.ErrAccessDenied)
	}
	if subtle.ConstantTimeCompare([]byte(cl.Cred), []byte(s.credentialTag(cl.Subject, creds.SecretKey))) != 1 {
		return nil, fmt.Errorf("%w: the credentials changed since the login", domain.ErrAccessDenied)
	}
	identity.AccessKey = cl.Subject
	return &Session{
		ID:        cl.ID,
		AccessKey: cl.Subject,
		Identity:  identity,
		IssuedAt:  cl.IssuedAt.Time,
		ExpiresAt: cl.ExpiresAt.Time,
		CSRFToken: cl.CSRF,
	}, nil
}

// Logout implements Service. Expired revocations are purged from the
// cache and the store on the way.
func (s *service) Logout(ctx context.Context, token string) error {
	cl, err := s.parse(token)
	if err != nil {
		// Invalid or expired tokens need no revocation.
		return nil
	}
	value, err := json.Marshal(revocation{ExpiresAt: cl.ExpiresAt.UTC()})
	if err != nil {
		return fmt.Errorf("session: encode revocation: %w", err)
	}
	if err := s.deps.Store.Put(ctx, iam.NamespaceSessions, cl.ID, value); err != nil {
		return fmt.Errorf("session: store revocation: %w", err)
	}
	now := s.deps.Clock.Now()
	s.mu.Lock()
	for id, exp := range s.revoked {
		if !now.Before(exp) {
			delete(s.revoked, id)
		}
	}
	s.revoked[cl.ID] = cl.ExpiresAt.Time
	s.mu.Unlock()
	s.purgeStored(ctx, now)
	return nil
}

// isRevoked reports whether a session was revoked, on this node (cache)
// or on another one, or before a restart (store). A store failure fails
// closed.
func (s *service) isRevoked(ctx context.Context, cl *claims) (bool, error) {
	s.mu.Lock()
	_, cached := s.revoked[cl.ID]
	s.mu.Unlock()
	if cached {
		return true, nil
	}
	_, err := s.deps.Store.Get(ctx, iam.NamespaceSessions, cl.ID)
	switch {
	case errors.Is(err, iam.ErrEntryNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("session: read revocations: %w", err)
	}
	s.mu.Lock()
	s.revoked[cl.ID] = cl.ExpiresAt.Time
	s.mu.Unlock()
	return true, nil
}

// purgeStored deletes the stored revocations of expired sessions; a
// failure is logged (the entries are retried at the next logout).
func (s *service) purgeStored(ctx context.Context, now time.Time) {
	all, err := s.deps.Store.List(ctx, iam.NamespaceSessions)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "session: list revocations", "err", err)
		return
	}
	for id, raw := range all {
		var r revocation
		if err := json.Unmarshal(raw, &r); err == nil && now.Before(r.ExpiresAt) {
			continue
		}
		if err := s.deps.Store.Delete(ctx, iam.NamespaceSessions, id); err != nil {
			s.deps.Logger.WarnContext(ctx, "session: purge revocation", "session", id, "err", err)
		}
	}
}

// credentialTag binds a session to the secret key of its credentials: an
// HMAC with the signing key, so that the token (readable by its holder)
// discloses nothing about the secret.
func (s *service) credentialTag(accessKey, secretKey string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("nexo-console-credential\x00" + accessKey + "\x00" + secretKey))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// ValidateCSRF implements Service.
func (s *service) ValidateCSRF(_ context.Context, sess *Session, csrfToken string) error {
	if sess == nil || csrfToken == "" || sess.CSRFToken == "" ||
		subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(csrfToken)) != 1 {
		return fmt.Errorf("%w: invalid csrf token", domain.ErrAccessDenied)
	}
	return nil
}

func (s *service) parse(token string) (*claims, error) {
	if token == "" {
		return nil, fmt.Errorf("%w: missing session token", domain.ErrAccessDenied)
	}
	cl := &claims{}
	_, err := jwt.ParseWithClaims(token, cl, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return s.key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.deps.Clock.Now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid session token: %v", domain.ErrAccessDenied, err)
	}
	if cl.ID == "" || cl.Subject == "" || cl.CSRF == "" || cl.Cred == "" {
		return nil, fmt.Errorf("%w: malformed session token", domain.ErrAccessDenied)
	}
	return cl, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func randomHex(n int) (string, error) {
	b, err := randomBytes(n)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
