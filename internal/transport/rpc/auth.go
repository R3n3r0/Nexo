package rpc

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
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
	// DefaultTokenTTL is the validity of an internode token: one call, the
	// time for it to reach the node.
	DefaultTokenTTL = 20 * time.Second
	// maxTokenLifetime bounds the validity a received token may claim
	// (expiry minus issue time): the replay cache keeps the id of every
	// accepted token that long.
	maxTokenLifetime = 2 * time.Minute
	// defaultClockSkew is the clock difference between nodes tolerated on
	// the expiry of a token: well above the skew the cluster warns about
	// (2s), well below the validity of a token.
	defaultClockSkew = 15 * time.Second
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

// WithTokenTTL sets the validity of the issued tokens (default 20s, at
// most 2 min).
func WithTokenTTL(d time.Duration) AuthOption {
	return func(o *authOptions) {
		if d > 0 {
			o.ttl = min(d, maxTokenLifetime)
		}
	}
}

// WithClockSkew sets the clock difference tolerated on the expiry of the
// received tokens (default 15s).
func WithClockSkew(d time.Duration) AuthOption {
	return func(o *authOptions) {
		if d >= 0 {
			o.skew = d
		}
	}
}

// tokenClaims is the JWT payload of an internode token. The registered
// "jti" is the unique id of the token; "htm", "htu" and "bh" bind it to
// its call (method, target, base64url SHA-256 of the arguments).
type tokenClaims struct {
	jwt.RegisteredClaims
	Node       string `json:"node"`
	Deployment string `json:"dep"`
	Protocol   int    `json:"pv"`
	Version    string `json:"ver"`
	Method     string `json:"htm"`
	Target     string `json:"htu"`
	BodyHash   string `json:"bh,omitempty"`
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

	// seen are the ids of the tokens accepted and not expired yet, with
	// their expiry (skew included): a token is accepted once.
	seenMu    sync.Mutex
	seen      map[string]time.Time
	nextSweep time.Time
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
	return &authenticator{deps: deps, opts: o, secret: secret, deploymentID: o.deploymentID,
		keys: make(map[string][]byte), seen: make(map[string]time.Time)}, nil
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

// Token implements Authenticator: every call gets its own token.
func (a *authenticator) Token(ctx context.Context, audience string, call CallBinding) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if audience == "" {
		return "", fmt.Errorf("%w: rpc: a token needs the node it is issued for", domain.ErrInvalidArgument)
	}
	if call.Method == "" || call.Target == "" {
		return "", fmt.Errorf("%w: rpc: a token is bound to a call", domain.ErrInvalidArgument)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("rpc: token id: %w", err)
	}
	now := a.deps.Clock.Now()
	a.mu.Lock()
	dep := a.deploymentID
	key, err := a.keyLocked(dep)
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	claims := tokenClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        base64.RawURLEncoding.EncodeToString(id[:]),
			Issuer:    tokenIssuer,
			Subject:   a.opts.nodeID,
			Audience:  jwt.ClaimStrings{tokenAudience, audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.opts.ttl)),
		},
		Node: a.opts.nodeID, Deployment: dep, Protocol: ProtocolVersion, Version: a.opts.version,
		Method: call.Method, Target: call.Target,
	}
	if call.BodyHash != nil {
		claims.BodyHash = base64.RawURLEncoding.EncodeToString(call.BodyHash)
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("rpc: sign internode token: %w", err)
	}
	return token, nil
}

// Verify implements Authenticator. The signature is verified before any
// claim is trusted: the key is derived from the deployment id the caller
// claims only when it is empty (a caller that has not read the format
// yet proves the root credentials all the same) or the one of this node;
// a token claiming another deployment is verified with the key of this
// node, so that it fails like any forged token. Only a token validly
// signed for another deployment (same root credentials, another
// deployment: a misconfiguration) is reported as such. The answers never
// carry the identity of this node or of its deployment (the salt of the
// internode keys): the details are logged here.
func (a *authenticator) Verify(ctx context.Context, token string, call CallBinding) (Claims, error) {
	if err := ctx.Err(); err != nil {
		return Claims{}, err
	}
	if token == "" {
		return Claims{}, fmt.Errorf("%w: missing bearer token", ErrInvalidToken)
	}
	a.mu.Lock()
	local := a.deploymentID
	a.mu.Unlock()
	foreign := false
	parse := func(keyOf func(claimed string) string) (*jwt.Token, *tokenClaims, error) {
		var claims tokenClaims
		parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
			c, ok := t.Claims.(*tokenClaims)
			if !ok {
				return nil, errors.New("unexpected claims")
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.keyLocked(keyOf(c.Deployment))
		},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithIssuer(tokenIssuer),
			jwt.WithAudience(a.opts.nodeID),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			jwt.WithLeeway(a.opts.skew),
			jwt.WithTimeFunc(a.deps.Clock.Now),
		)
		return parsed, &claims, err
	}
	parsed, claims, err := parse(func(claimed string) string {
		if local != "" && claimed != "" && claimed != local {
			foreign = true
			return local
		}
		return claimed
	})
	if err != nil {
		if foreign {
			// Signed for the deployment it claims? Then it is a node of
			// another deployment sharing the root credentials.
			if _, other, oerr := parse(func(claimed string) string { return claimed }); oerr == nil {
				a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "rpc: call from a node of another deployment refused",
					slog.String("caller", other.Node), slog.String("caller_deployment", other.Deployment),
					slog.String("deployment", local))
				return Claims{}, fmt.Errorf("%w: the caller belongs to another deployment", ErrDeploymentMismatch)
			}
		}
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !parsed.Valid || claims.Node == "" || claims.ID == "" || claims.IssuedAt == nil {
		return Claims{}, fmt.Errorf("%w: incomplete claims", ErrInvalidToken)
	}
	if claims.ExpiresAt.Sub(claims.IssuedAt.Time) > maxTokenLifetime {
		return Claims{}, fmt.Errorf("%w: token valid for too long", ErrInvalidToken)
	}
	bodyHash, err := base64.RawURLEncoding.DecodeString(claims.BodyHash)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: malformed argument hash", ErrInvalidToken)
	}
	if claims.Method != call.Method || claims.Target != call.Target ||
		(call.BodyHash != nil && !hmac.Equal(bodyHash, call.BodyHash)) {
		return Claims{}, fmt.Errorf("%w: the token was issued for another call", ErrInvalidToken)
	}
	if !a.firstUse(claims.ID, claims.ExpiresAt.Add(a.opts.skew)) {
		a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "rpc: replayed internode token refused",
			slog.String("caller", claims.Node), slog.String("target", call.Target))
		return Claims{}, fmt.Errorf("%w: token already used", ErrInvalidToken)
	}
	if claims.Protocol != ProtocolVersion {
		return Claims{}, fmt.Errorf("%w: the caller speaks protocol %d, this node %d", ErrIncompatibleVersion, claims.Protocol, ProtocolVersion)
	}
	if !domain.CompatibleVersions(claims.Version, a.opts.version) {
		return Claims{}, fmt.Errorf("%w: the caller runs %s, this node %s", ErrIncompatibleVersion, claims.Version, a.opts.version)
	}
	out := Claims{
		NodeID: claims.Node, DeploymentID: claims.Deployment, Protocol: claims.Protocol, Version: claims.Version,
		Unbound: local != "" && claims.Deployment == "", Unverified: local == "",
	}
	if claims.IssuedAt != nil {
		out.IssuedAt = claims.IssuedAt.UTC()
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.UTC()
	}
	if len(bodyHash) > 0 {
		out.BodyHash = bodyHash
	}
	return out, nil
}

// firstUse records the id of an accepted token until it expires and
// reports whether it was not seen before. Expired ids are swept at most
// once a second; their number is bounded by the call rate times the
// maximum token lifetime.
func (a *authenticator) firstUse(id string, expiry time.Time) bool {
	now := a.deps.Clock.Now()
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	if now.After(a.nextSweep) {
		for k, exp := range a.seen {
			if now.After(exp) {
				delete(a.seen, k)
			}
		}
		a.nextSweep = now.Add(time.Second)
	}
	if exp, ok := a.seen[id]; ok && !now.After(exp) {
		return false
	}
	a.seen[id] = expiry
	return true
}
