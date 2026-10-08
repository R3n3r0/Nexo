package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	IAM    iam.Service
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

// DeriveKey derives a deterministic signing key from the root credentials
// so that every node of a deployment accepts the same session cookies.
func DeriveKey(accessKey, secretKey string) []byte {
	sum := sha256.Sum256([]byte("nexo-console-session\x00" + accessKey + "\x00" + secretKey))
	return sum[:]
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
}

// service issues HS256 JWT sessions and keeps an in-memory revocation list.
type service struct {
	deps   Deps
	key    []byte
	ttl    time.Duration
	issuer string

	mu      sync.Mutex
	revoked map[string]time.Time
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
	s := &service{deps: deps, ttl: 12 * time.Hour, issuer: "nexo", revoked: make(map[string]time.Time)}
	for _, o := range opts {
		o(s)
	}
	if len(s.key) == 0 {
		key, err := randomBytes(32)
		if err != nil {
			return nil, fmt.Errorf("session: generate signing key: %w", err)
		}
		s.key = key
	}
	return s, nil
}

// Login implements Service.
func (s *service) Login(ctx context.Context, accessKey, secretKey string) (*Session, error) {
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("%w: missing credentials", domain.ErrAccessDenied)
	}
	creds, identity, err := s.deps.IAM.LookupCredentials(ctx, accessKey)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidAccessKey) || errors.Is(err, domain.ErrUserNotFound) {
			return nil, fmt.Errorf("%w: invalid credentials", domain.ErrAccessDenied)
		}
		return nil, fmt.Errorf("session: lookup credentials: %w", err)
	}
	now := s.deps.Clock.Now()
	if subtle.ConstantTimeCompare([]byte(creds.SecretKey), []byte(secretKey)) != 1 {
		return nil, fmt.Errorf("%w: invalid credentials", domain.ErrAccessDenied)
	}
	if !creds.IsEnabled() {
		return nil, fmt.Errorf("%w: account disabled", domain.ErrAccessDenied)
	}
	if creds.IsExpired(now) {
		return nil, fmt.Errorf("%w: credentials expired", domain.ErrAccessDenied)
	}

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

// Validate implements Service. The token only proves who logged in: the
// identity (groups, attached policies, status) is resolved again through
// IAM so that revoked permissions and disabled accounts take effect
// immediately.
func (s *service) Validate(ctx context.Context, token string) (*Session, error) {
	cl, err := s.parse(token)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	_, revoked := s.revoked[cl.ID]
	s.mu.Unlock()
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

// Logout implements Service.
func (s *service) Logout(_ context.Context, token string) error {
	cl, err := s.parse(token)
	if err != nil {
		// Invalid or expired tokens need no revocation.
		return nil
	}
	now := s.deps.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, exp := range s.revoked {
		if !now.Before(exp) {
			delete(s.revoked, id)
		}
	}
	s.revoked[cl.ID] = cl.ExpiresAt.Time
	return nil
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
	if cl.ID == "" || cl.Subject == "" || cl.CSRF == "" {
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
