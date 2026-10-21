package idp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// provider is the discovered metadata and the keys of an OpenID provider.
type provider struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`

	fetched time.Time
	keys    map[string]crypto.PublicKey
	keysAt  time.Time
	anyKeys []crypto.PublicKey
}

const (
	// discoveryTTL is how long the metadata of a provider is reused.
	discoveryTTL = time.Hour
	// keysMinRefresh bounds the refreshes of the keys asked for by tokens
	// signed with an unknown key.
	keysMinRefresh = time.Minute
	maxProviderDoc = 1 << 20
)

// getJSON fetches a JSON document of the provider.
func (s *service) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.deps.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxProviderDoc)).Decode(v)
}

// metadata returns the provider of cfg, discovered or cached; refresh
// reloads its keys (a token names a key not seen yet).
func (s *service) metadata(ctx context.Context, cfg *openIDConfig, refreshKeys bool) (*provider, error) {
	now := s.deps.Clock.Now()
	s.mu.Lock()
	p := s.discovery[cfg.configURL]
	stale := p == nil || now.Sub(p.fetched) > discoveryTTL
	s.mu.Unlock()
	if stale {
		var np provider
		if err := s.getJSON(ctx, cfg.configURL, &np); err != nil {
			return nil, fmt.Errorf("%w: idp: discovery of the OpenID provider: %w", domain.ErrServiceUnavailable, err)
		}
		if np.Issuer == "" || np.JWKSURI == "" || np.AuthorizationEndpoint == "" || np.TokenEndpoint == "" {
			return nil, fmt.Errorf("%w: idp: incomplete OpenID provider metadata at %s", domain.ErrServiceUnavailable, cfg.configURL)
		}
		np.fetched = now
		s.mu.Lock()
		s.discovery[cfg.configURL] = &np
		s.mu.Unlock()
		p, refreshKeys = &np, true
	}
	s.mu.Lock()
	needKeys := p.keys == nil || (refreshKeys && now.Sub(p.keysAt) > keysMinRefresh)
	s.mu.Unlock()
	if needKeys {
		keys, all, err := s.fetchKeys(ctx, p.JWKSURI)
		if err != nil {
			return nil, fmt.Errorf("%w: idp: keys of the OpenID provider: %w", domain.ErrServiceUnavailable, err)
		}
		s.mu.Lock()
		p.keys, p.anyKeys, p.keysAt = keys, all, now
		s.mu.Unlock()
	}
	return p, nil
}

// jwk is a JSON Web Key (RSA or EC public key).
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (s *service) fetchKeys(ctx context.Context, u string) (map[string]crypto.PublicKey, []crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := s.getJSON(ctx, u, &set); err != nil {
		return nil, nil, err
	}
	keys := map[string]crypto.PublicKey{}
	var all []crypto.PublicKey
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "idp: OpenID provider key skipped", "kid", k.Kid, "err", err)
			continue
		}
		if k.Kid != "" {
			keys[k.Kid] = pub
		}
		all = append(all, pub)
	}
	if len(all) == 0 {
		return nil, nil, errors.New("no usable signing key")
	}
	return keys, all, nil
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64int(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64int(k.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31 {
			return nil, fmt.Errorf("invalid RSA exponent")
		}
		if n.BitLen() < 2048 {
			return nil, fmt.Errorf("RSA key of %d bits", n.BitLen())
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.X, "="))
		if err != nil {
			return nil, err
		}
		y, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.Y, "="))
		if err != nil {
			return nil, err
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) > size || len(y) > size {
			return nil, fmt.Errorf("invalid EC point")
		}
		// The uncompressed point, coordinates left-padded to the size of
		// the curve; the parser checks that it is on the curve.
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):1+size], x)
		copy(point[1+2*size-len(y):], y)
		return ecdsa.ParseUncompressedPublicKey(curve, point)
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

// signingMethods are the algorithms an ID token may use.
func signingMethods() []string {
	return []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}
}

// verify checks an ID token against the provider and returns its user.
func (s *service) verify(ctx context.Context, cfg *openIDConfig, raw, nonce string) (Identity, error) {
	p, err := s.metadata(ctx, cfg, false)
	if err != nil {
		return Identity{}, err
	}
	claims := jwt.MapClaims{}
	parse := func(p *provider) error {
		_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if kid, _ := t.Header["kid"].(string); kid != "" {
				if k, ok := p.keys[kid]; ok {
					return k, nil
				}
				return nil, errUnknownKey
			}
			if len(p.anyKeys) == 1 {
				return p.anyKeys[0], nil
			}
			return nil, errUnknownKey
		}, jwt.WithValidMethods(signingMethods()), jwt.WithIssuer(p.Issuer), jwt.WithAudience(cfg.clientID),
			jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithTimeFunc(s.deps.Clock.Now), jwt.WithLeeway(time.Minute))
		return err
	}
	err = parse(p)
	if errors.Is(err, errUnknownKey) {
		// The provider may have rotated its keys.
		if p, err = s.metadata(ctx, cfg, true); err != nil {
			return Identity{}, err
		}
		claims = jwt.MapClaims{}
		err = parse(p)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%w: idp: invalid ID token: %w", domain.ErrAccessDenied, err)
	}
	if nonce != "" {
		if got, _ := claims["nonce"].(string); got != nonce {
			return Identity{}, fmt.Errorf("%w: idp: the ID token does not answer this login (nonce)", domain.ErrAccessDenied)
		}
	}
	if aud, _ := claims.GetAudience(); len(aud) > 1 {
		// Several audiences: the authorized party must be this client.
		if azp, _ := claims["azp"].(string); azp != cfg.clientID {
			return Identity{}, fmt.Errorf("%w: idp: the ID token was issued to another client", domain.ErrAccessDenied)
		}
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		return Identity{}, fmt.Errorf("%w: idp: the ID token has no subject", domain.ErrAccessDenied)
	}
	id := Identity{Provider: ProviderOpenID, Subject: sub, Claims: map[string]any(claims)}
	id.Username, _ = claims[cfg.usernameClaim].(string)
	if id.Username == "" {
		if email, _ := claims["email"].(string); email != "" {
			id.Username = email
		} else {
			id.Username = sub
		}
	}
	id.Groups = stringsClaim(claims[cfg.groupsClaim])
	id.PolicyNames = stringsClaim(claims[cfg.claimName])
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		id.Expiry = exp.Time
	}
	return id, nil
}

var errUnknownKey = errors.New("the token is signed with an unknown key")

// stringsClaim reads a claim holding names: an array of strings or a
// comma separated string.
func stringsClaim(v any) []string {
	switch t := v.(type) {
	case string:
		return list(t)
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// WebIdentity implements Service.
func (s *service) WebIdentity(ctx context.Context, token string) (Identity, error) {
	cfg, err := s.openID(ctx)
	if err != nil {
		return Identity{}, err
	}
	if cfg == nil {
		return Identity{}, fmt.Errorf("%w: idp: no OpenID provider is configured", domain.ErrNotImplemented)
	}
	if token == "" || len(token) > 64<<10 {
		return Identity{}, fmt.Errorf("%w: idp: invalid ID token", domain.ErrAccessDenied)
	}
	return s.verify(ctx, cfg, token, "")
}

// AuthCodeURL implements Service.
func (s *service) AuthCodeURL(ctx context.Context, in AuthCodeInput) (string, error) {
	cfg, err := s.openID(ctx)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return "", fmt.Errorf("%w: idp: no OpenID provider is configured", domain.ErrNotImplemented)
	}
	p, err := s.metadata(ctx, cfg, false)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(p.AuthorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("%w: idp: authorization endpoint: %w", domain.ErrServiceUnavailable, err)
	}
	scopes := cfg.scopes
	if !slices.Contains(scopes, "openid") {
		scopes = append([]string{"openid"}, scopes...)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.clientID)
	q.Set("redirect_uri", in.RedirectURI)
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", in.State)
	q.Set("nonce", in.Nonce)
	q.Set("code_challenge", in.CodeChallenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange implements Service.
func (s *service) Exchange(ctx context.Context, in ExchangeInput) (Identity, error) {
	cfg, err := s.openID(ctx)
	if err != nil {
		return Identity{}, err
	}
	if cfg == nil {
		return Identity{}, fmt.Errorf("%w: idp: no OpenID provider is configured", domain.ErrNotImplemented)
	}
	p, err := s.metadata(ctx, cfg, false)
	if err != nil {
		return Identity{}, err
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {in.Code}, "redirect_uri": {in.RedirectURI},
		"code_verifier": {in.CodeVerifier}, "client_id": {cfg.clientID},
	}
	basic := cfg.clientSecret != "" && (len(p.TokenAuthMethods) == 0 || slices.Contains(p.TokenAuthMethods, "client_secret_basic"))
	if cfg.clientSecret != "" && !basic {
		form.Set("client_secret", cfg.clientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(cfg.clientID), url.QueryEscape(cfg.clientSecret))
	}
	resp, err := s.deps.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: idp: token endpoint: %w", domain.ErrServiceUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderDoc)).Decode(&out); err != nil && resp.StatusCode == http.StatusOK {
		return Identity{}, fmt.Errorf("%w: idp: token endpoint: %w", domain.ErrServiceUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		return Identity{}, fmt.Errorf("%w: idp: the provider refused the code: %s %s %s", domain.ErrAccessDenied, resp.Status, out.Error, out.Desc)
	}
	return s.verify(ctx, cfg, out.IDToken, in.Nonce)
}
