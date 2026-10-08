package rpc

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Token parameters of the internode authentication.
const (
	tokenIssuer   = "nexo"
	tokenAudience = "nexo-internode"
	// keyInfo is the HKDF context of the signing key: changing it changes
	// every key, so it is versioned like the protocol.
	keyInfo = "nexo internode rpc v1"
	// DefaultTokenTTL is the validity of an internode token.
	DefaultTokenTTL = time.Minute
	// defaultClockSkew is the clock difference between nodes tolerated on
	// the expiry of a token.
	defaultClockSkew = time.Minute
)

// AuthDeps are the dependencies of the authenticator. Every field is
// optional.
type AuthDeps struct {
	Clock  clock.Clock
	Logger *slog.Logger
}

// AuthOption configures the authenticator.
type AuthOption func(*authOptions)

type authOptions struct {
	accessKey    string
	secretKey    string
	nodeID       string
	deploymentID string
	version      string
	ttl          time.Duration
	skew         time.Duration
}

// WithCredentials sets the root credentials the signing key derives from
// (required): every node of a deployment shares them.
func WithCredentials(accessKey, secretKey string) AuthOption {
	return func(o *authOptions) { o.accessKey, o.secretKey = accessKey, secretKey }
}

// WithNodeID sets the identity of this node in its tokens (required).
func WithNodeID(id string) AuthOption {
	return func(o *authOptions) { o.nodeID = id }
}

// WithDeploymentID sets the deployment id when it is known at startup
// (see Authenticator.SetDeploymentID).
func WithDeploymentID(id string) AuthOption {
	return func(o *authOptions) { o.deploymentID = id }
}

// WithBuildVersion sets the build version announced in the tokens (default
// "dev"); nodes whose major versions differ refuse each other.
func WithBuildVersion(v string) AuthOption {
	return func(o *authOptions) { o.version = v }
}

// WithTokenTTL sets the validity of the issued tokens (default 1 min).
func WithTokenTTL(d time.Duration) AuthOption {
	return func(o *authOptions) {
		if d > 0 {
			o.ttl = d
		}
	}
}

// WithClockSkew sets the clock difference tolerated on the expiry of the
// received tokens (default 1 min).
func WithClockSkew(d time.Duration) AuthOption {
	return func(o *authOptions) {
		if d >= 0 {
			o.skew = d
		}
	}
}

// tokenClaims is the JWT payload of an internode token.
type tokenClaims struct {
	jwt.RegisteredClaims
	Node       string `json:"node"`
	Deployment string `json:"dep"`
	Protocol   int    `json:"pv"`
	Version    string `json:"ver"`
}

// authenticator implements Authenticator with HS256 JWTs. The signing key
// of a deployment id is HKDF-SHA256(root credentials, salt = deployment id).
type authenticator struct {
	deps AuthDeps
	opts authOptions
	// secret is the HKDF input keying material.
	secret []byte

	mu           sync.Mutex
	deploymentID string
	keys         map[string][]byte
	token        string
	tokenExpiry  time.Time
}

var _ Authenticator = (*authenticator)(nil)

// NewAuthenticator returns the internode authenticator of this node.
func NewAuthenticator(deps AuthDeps, opts ...AuthOption) (Authenticator, error) {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := authOptions{version: "dev", ttl: DefaultTokenTTL, skew: defaultClockSkew}
	for _, opt := range opts {
		opt(&o)
	}
	if o.accessKey == "" || o.secretKey == "" {
		return nil, fmt.Errorf("%w: rpc: the internode authentication needs the root credentials", domain.ErrInvalidArgument)
	}
	if o.nodeID == "" {
		return nil, fmt.Errorf("%w: rpc: the internode authentication needs the node id", domain.ErrInvalidArgument)
	}
	secret := make([]byte, 0, len(o.accessKey)+len(o.secretKey)+1)
	secret = append(append(append(secret, o.accessKey...), 0), o.secretKey...)
	return &authenticator{deps: deps, opts: o, secret: secret, deploymentID: o.deploymentID, keys: make(map[string][]byte)}, nil
}

// NodeID implements Authenticator.
func (a *authenticator) NodeID() string { return a.opts.nodeID }

// SetDeploymentID implements Authenticator.
func (a *authenticator) SetDeploymentID(ctx context.Context, id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deploymentID == id {
		return
	}
	a.deploymentID = id
	a.token = "" // re-issue with the deployment id
	a.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "rpc: internode authentication bound to the deployment", slog.String("deployment_id", id))
}

// keyLocked returns the signing key of a deployment id.
func (a *authenticator) keyLocked(deploymentID string) ([]byte, error) {
	if k, ok := a.keys[deploymentID]; ok {
		return k, nil
	}
	k, err := hkdf.Key(sha256.New, a.secret, []byte(deploymentID), keyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("rpc: derive internode key: %w", err)
	}
	if len(a.keys) > 8 { // a handful of ids at most: "" and the deployment
		clear(a.keys)
	}
	a.keys[deploymentID] = k
	return k, nil
}

// Token implements Authenticator. Tokens are reused until half of their
// validity is spent.
func (a *authenticator) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := a.deps.Clock.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && now.Before(a.tokenExpiry.Add(-a.opts.ttl/2)) {
		return a.token, nil
	}
	key, err := a.keyLocked(a.deploymentID)
	if err != nil {
		return "", err
	}
	exp := now.Add(a.opts.ttl)
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    tokenIssuer,
			Subject:   a.opts.nodeID,
			Audience:  jwt.ClaimStrings{tokenAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Node: a.opts.nodeID, Deployment: a.deploymentID, Protocol: ProtocolVersion, Version: a.opts.version,
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("rpc: sign internode token: %w", err)
	}
	a.token, a.tokenExpiry = token, exp
	return token, nil
}

// Verify implements Authenticator. The key is derived from the deployment
// id claimed by the caller: a caller that has not read the format yet (no
// deployment id) proves the root credentials all the same, while a caller
// of another deployment is refused once both ids are known.
func (a *authenticator) Verify(ctx context.Context, token string) (Claims, error) {
	if err := ctx.Err(); err != nil {
		return Claims{}, err
	}
	if token == "" {
		return Claims{}, fmt.Errorf("%w: missing bearer token", ErrInvalidToken)
	}
	a.mu.Lock()
	local := a.deploymentID
	a.mu.Unlock()
	var claims tokenClaims
	var mismatch error
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		c, ok := t.Claims.(*tokenClaims)
		if !ok {
			return nil, errors.New("unexpected claims")
		}
		if local != "" && c.Deployment != "" && c.Deployment != local {
			mismatch = fmt.Errorf("%w: caller %s belongs to deployment %s, this node to %s", ErrDeploymentMismatch, c.Node, c.Deployment, local)
			return nil, mismatch
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.keyLocked(c.Deployment)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(tokenIssuer),
		jwt.WithAudience(tokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(a.opts.skew),
		jwt.WithTimeFunc(a.deps.Clock.Now),
	)
	if mismatch != nil {
		return Claims{}, mismatch
	}
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !parsed.Valid || claims.Node == "" {
		return Claims{}, fmt.Errorf("%w: incomplete claims", ErrInvalidToken)
	}
	if claims.Protocol != ProtocolVersion {
		return Claims{}, fmt.Errorf("%w: node %s speaks protocol %d, this node %d", ErrIncompatibleVersion, claims.Node, claims.Protocol, ProtocolVersion)
	}
	if !compatibleVersions(claims.Version, a.opts.version) {
		return Claims{}, fmt.Errorf("%w: node %s runs %s, this node %s", ErrIncompatibleVersion, claims.Node, claims.Version, a.opts.version)
	}
	out := Claims{NodeID: claims.Node, DeploymentID: claims.Deployment, Protocol: claims.Protocol, Version: claims.Version}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.UTC()
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.UTC()
	}
	return out, nil
}

// compatibleVersions reports whether two build versions may run in the
// same deployment: same major version, or a version that is not a release
// ("dev" builds, commit hashes) on either side.
func compatibleVersions(a, b string) bool {
	ma, okA := majorVersion(a)
	mb, okB := majorVersion(b)
	return !okA || !okB || ma == mb
}

// majorVersion parses the major of "vX.Y.Z" / "X.Y.Z".
func majorVersion(v string) (int, bool) {
	v = strings.TrimPrefix(v, "v")
	major, _, ok := strings.Cut(v, ".")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(major)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
