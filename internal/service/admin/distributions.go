package admin

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// DistributionManagerDeps are the dependencies of the distribution manager.
type DistributionManagerDeps struct {
	// Distributions manages the distributions and key groups (required).
	Distributions distribution.Service
	// Signer signs the URLs and cookies (required).
	Signer distribution.Signer
	// Authz authorizes the origin buckets of the saved distributions
	// (required).
	Authz Authorizer
	// Cache, when set, drops the cache settings and the entries of a
	// removed distribution (docs/11), so that a distribution created again
	// with the same id starts from a clean cache.
	Cache cache.Manager
	// Clock dates the relative expirations of the signatures.
	Clock  clock.Clock
	Logger *slog.Logger
}

// DistributionManagerOption configures the distribution manager.
type DistributionManagerOption func(*distributionManager)

type distributionManager struct {
	deps DistributionManagerDeps
}

var _ DistributionManager = (*distributionManager)(nil)

// NewDistributionManager returns the distribution manager of the Admin
// API. A missing dependency is a wiring error (domain.ErrInvalidArgument).
func NewDistributionManager(deps DistributionManagerDeps, opts ...DistributionManagerOption) (DistributionManager, error) {
	if deps.Distributions == nil || deps.Signer == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: distribution manager: the distributions, signer and authorizer are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &distributionManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// List implements DistributionManager.
func (m *distributionManager) List(ctx context.Context) ([]distribution.Distribution, error) {
	return m.deps.Distributions.List(ctx)
}

// Get implements DistributionManager.
func (m *distributionManager) Get(ctx context.Context, id string) (distribution.Distribution, error) {
	return m.deps.Distributions.Get(ctx, id)
}

// Create implements DistributionManager.
func (m *distributionManager) Create(ctx context.Context, in SaveDistributionInput) (distribution.Distribution, error) {
	if err := m.authorizeOrigin(ctx, in.Requester, in.Distribution.Origin.Bucket); err != nil {
		return distribution.Distribution{}, err
	}
	return m.deps.Distributions.Create(ctx, in.Distribution)
}

// Update implements DistributionManager: the requester must be allowed to
// expose both the current origin bucket and the new one.
func (m *distributionManager) Update(ctx context.Context, in SaveDistributionInput) (distribution.Distribution, error) {
	current, err := m.deps.Distributions.Get(ctx, in.Distribution.ID)
	if err != nil {
		return distribution.Distribution{}, err
	}
	if err := m.authorizeOrigin(ctx, in.Requester, current.Origin.Bucket); err != nil {
		return distribution.Distribution{}, err
	}
	if b := strings.TrimSpace(in.Distribution.Origin.Bucket); b != current.Origin.Bucket {
		if err := m.authorizeOrigin(ctx, in.Requester, b); err != nil {
			return distribution.Distribution{}, err
		}
	}
	return m.deps.Distributions.Update(ctx, in.Distribution, in.IfMatch)
}

// authorizeOrigin authorizes admin:ManageDistributions on the origin
// bucket of a distribution: an explicit Deny on the bucket keeps it from
// being exposed. An empty or invalid name is left to the validation of
// the distribution.
func (m *distributionManager) authorizeOrigin(ctx context.Context, req Requester, bucket string) error {
	bucket = strings.TrimSpace(bucket)
	if bucket == "" || domain.ValidateBucketName(bucket) != nil {
		return nil
	}
	return authorizeAll(ctx, m.deps.Authz, req, bucket, "", policy.ActionAdminManageDistributions)
}

// authorizeDistribution authorizes admin:ManageDistributions on the
// origin bucket of the distribution id.
func (m *distributionManager) authorizeDistribution(ctx context.Context, req Requester, id string) error {
	d, err := m.deps.Distributions.Get(ctx, id)
	if err != nil {
		return err
	}
	return m.authorizeOrigin(ctx, req, d.Origin.Bucket)
}

// authorizeKeyGroup authorizes admin:ManageDistributions on the origin
// bucket of every distribution whose behaviors sign with the key group:
// its keys sign the URLs of all of them.
func (m *distributionManager) authorizeKeyGroup(ctx context.Context, req Requester, group string) error {
	ds, err := m.deps.Distributions.List(ctx)
	if err != nil {
		return err
	}
	for _, d := range ds {
		if usesKeyGroup(d, group) {
			if err := m.authorizeOrigin(ctx, req, d.Origin.Bucket); err != nil {
				return err
			}
		}
	}
	return nil
}

// usesKeyGroup reports a distribution with a behavior signed by group.
func usesKeyGroup(d distribution.Distribution, group string) bool {
	group = strings.TrimSpace(group)
	signs := func(b distribution.Behavior) bool { return b.Signing != nil && b.Signing.KeyGroup == group }
	if signs(d.DefaultBehavior) {
		return true
	}
	for _, b := range d.Behaviors {
		if signs(b) {
			return true
		}
	}
	return false
}

// Delete implements DistributionManager: the cache settings of the
// distribution go with it (a failure there is logged: the distribution is
// removed and serves nothing any more).
func (m *distributionManager) Delete(ctx context.Context, req Requester, id string) error {
	if err := m.authorizeDistribution(ctx, req, id); err != nil {
		return err
	}
	if err := m.deps.Distributions.Delete(ctx, id); err != nil {
		return err
	}
	if m.deps.Cache != nil {
		if err := m.deps.Cache.DeleteDistribution(context.WithoutCancel(ctx), id); err != nil {
			m.deps.Logger.WarnContext(ctx, "admin: cache settings of a removed distribution not removed", "distribution", id, "err", err)
		}
	}
	return nil
}

// Test implements DistributionManager.
func (m *distributionManager) Test(ctx context.Context, in TestDistributionInput) (distribution.TestResult, error) {
	d, err := m.deps.Distributions.Get(ctx, in.ID)
	if err != nil {
		return distribution.TestResult{}, err
	}
	u, err := m.targetURL(ctx, d, in.URL, "", "")
	if err != nil {
		return distribution.TestResult{}, err
	}
	return m.deps.Distributions.Test(ctx, distribution.TestInput{URL: u.String(), Method: in.Method, ClientIP: in.ClientIP})
}

// targetURL completes the URL of a test or of a signature: an absolute
// URL is kept, a path gets scheme (default https) and host (default: the
// first exact alias of d). The host must be an alias of d.
func (m *distributionManager) targetURL(ctx context.Context, d distribution.Distribution, raw, scheme, host string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: the URL or the path is required", domain.ErrInvalidArgument)
	}
	if strings.HasPrefix(raw, "/") {
		if scheme = strings.ToLower(strings.TrimSpace(scheme)); scheme == "" {
			scheme = "https"
		}
		if host = strings.TrimSpace(host); host == "" {
			host = exactAlias(d)
		}
		if host == "" {
			return nil, fmt.Errorf("%w: distribution %s has no exact alias: give the host of the URL", domain.ErrInvalidArgument, d.ID)
		}
		raw = scheme + "://" + host + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL nor a path", domain.ErrInvalidArgument, raw)
	}
	route := m.deps.Distributions.Resolve(ctx, u.Host)
	if route.Kind != distribution.RouteDistribution || route.Distribution == nil || route.Distribution.ID != d.ID {
		return nil, fmt.Errorf("%w: the host %s is not an alias of distribution %s", domain.ErrInvalidArgument, u.Hostname(), d.ID)
	}
	return u, nil
}

// exactAlias returns the first alias of d that is not a wildcard.
func exactAlias(d distribution.Distribution) string {
	for _, a := range d.Aliases {
		if !strings.HasPrefix(a, "*.") {
			return a
		}
	}
	return ""
}

// RotateOriginSecret implements DistributionManager.
func (m *distributionManager) RotateOriginSecret(ctx context.Context, req Requester, in distribution.RotateSecretInput) (*RotatedOriginSecret, error) {
	if err := m.authorizeDistribution(ctx, req, in.ID); err != nil {
		return nil, err
	}
	secret, d, err := m.deps.Distributions.RotateOriginSecret(ctx, in)
	if err != nil {
		return nil, err
	}
	out := &RotatedOriginSecret{Secret: secret, Distribution: d}
	if h := d.Restrictions.OriginSecretHeader; h != nil {
		out.Header = h.Name
	}
	return out, nil
}

// Sign implements DistributionManager.
func (m *distributionManager) Sign(ctx context.Context, in SignDistributionInput) (*SignedDistributionRequest, error) {
	expires := in.Expires
	switch {
	case !expires.IsZero() && in.ExpiresIn != 0:
		return nil, fmt.Errorf("%w: give the expiration or its delay, not both", domain.ErrInvalidArgument)
	case in.ExpiresIn < 0:
		return nil, fmt.Errorf("%w: the delay of the expiration must be positive", domain.ErrInvalidArgument)
	case in.ExpiresIn > 0:
		expires = m.deps.Clock.Now().Add(in.ExpiresIn)
	case expires.IsZero():
		return nil, fmt.Errorf("%w: the expiration of the signature is required", domain.ErrInvalidArgument)
	}
	d, err := m.deps.Distributions.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	u, err := m.targetURL(ctx, d, in.URL, in.Scheme, in.Host)
	if err != nil {
		return nil, err
	}
	res, err := m.deps.Distributions.Test(ctx, distribution.TestInput{URL: u.String()})
	if err != nil {
		return nil, err
	}
	if res.Behavior.Access != distribution.AccessSigned || res.Behavior.Signing == nil || res.Behavior.Signing.KeyGroup == "" {
		return nil, fmt.Errorf("%w: %s is served by a behavior with access %q: only the signed behaviors verify signatures",
			domain.ErrInvalidArgument, u.EscapedPath(), res.Behavior.Access)
	}
	group, err := m.deps.Distributions.GetKeyGroup(ctx, res.Behavior.Signing.KeyGroup)
	if err != nil {
		return nil, err
	}
	key, err := signingKey(group, in.PrivateKeyPEM, in.KeyPairID)
	if err != nil {
		return nil, err
	}
	si := distribution.SignInput{
		URL: u.String(), KeyPairID: key.ID, PrivateKeyPEM: in.PrivateKeyPEM, Expires: expires,
		NotBefore: in.NotBefore, IPRange: in.IPRange, Resource: in.Resource,
	}
	out := &SignedDistributionRequest{URL: u.String(), KeyGroup: group.ID, KeyPairID: key.ID, Expires: expires.UTC()}
	if in.Cookies {
		if out.Cookies, err = m.deps.Signer.SignCookies(ctx, si); err != nil {
			return nil, err
		}
		return out, nil
	}
	if out.URL, err = m.deps.Signer.SignURL(ctx, si); err != nil {
		return nil, err
	}
	return out, nil
}

// signingKey returns the key of group whose public key is the one of the
// private key (the key named keyID when set).
func signingKey(group distribution.KeyGroup, privatePEM, keyID string) (distribution.PublicKey, error) {
	pub, err := publicKeyDER(privatePEM)
	if err != nil {
		return distribution.PublicKey{}, err
	}
	for _, k := range group.Keys {
		if keyID != "" && k.ID != keyID {
			continue
		}
		der, err := pkixDER(k.PublicKeyPEM)
		if err == nil && bytes.Equal(der, pub) {
			return k, nil
		}
		if keyID != "" {
			return distribution.PublicKey{}, fmt.Errorf("%w: the private key is not the one of key %s", domain.ErrInvalidArgument, keyID)
		}
	}
	if keyID != "" {
		return distribution.PublicKey{}, fmt.Errorf("%w: key %s is not in key group %s, the key group of the behavior", domain.ErrInvalidArgument, keyID, group.ID)
	}
	return distribution.PublicKey{}, fmt.Errorf("%w: the private key is not the one of a key of key group %s, the key group of the behavior", domain.ErrInvalidArgument, group.ID)
}

// publicKeyDER returns the PKIX encoding of the public key of a PEM
// private key (PKCS #8, or PKCS #1 for RSA).
func publicKeyDER(privatePEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(privatePEM)))
	if block == nil {
		return nil, fmt.Errorf("%w: the private key is required, in PEM", domain.ErrInvalidArgument)
	}
	var (
		key any
		err error
	)
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("%w: PEM block %q is not a private key", domain.ErrInvalidArgument, block.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: private key: %v", domain.ErrInvalidArgument, err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("%w: unsupported private key", domain.ErrInvalidArgument)
	}
	der, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, fmt.Errorf("%w: private key: %v", domain.ErrInvalidArgument, err)
	}
	return der, nil
}

// pkixDER returns the canonical PKIX encoding of a PEM public key.
func pkixDER(publicPEM string) ([]byte, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(publicPEM)))
	if block == nil {
		return nil, fmt.Errorf("%w: public key is not PEM", domain.ErrInvalidArgument)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: public key: %v", domain.ErrInvalidArgument, err)
	}
	return x509.MarshalPKIXPublicKey(pub)
}

// Resolve implements DistributionManager.
func (m *distributionManager) Resolve(ctx context.Context, host string) (distribution.Route, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return distribution.Route{}, fmt.Errorf("%w: the host is required", domain.ErrInvalidArgument)
	}
	return m.deps.Distributions.Resolve(ctx, host), nil
}

// ListKeyGroups implements DistributionManager.
func (m *distributionManager) ListKeyGroups(ctx context.Context) ([]distribution.KeyGroup, error) {
	return m.deps.Distributions.ListKeyGroups(ctx)
}

// GetKeyGroup implements DistributionManager.
func (m *distributionManager) GetKeyGroup(ctx context.Context, id string) (distribution.KeyGroup, error) {
	return m.deps.Distributions.GetKeyGroup(ctx, id)
}

// CreateKeyGroup implements DistributionManager.
func (m *distributionManager) CreateKeyGroup(ctx context.Context, in CreateKeyGroupInput) (*CreatedKeyGroup, error) {
	g, err := m.deps.Distributions.CreateKeyGroup(ctx, in.ID, in.Comment)
	if err != nil {
		return nil, err
	}
	out := &CreatedKeyGroup{KeyGroup: g}
	if in.Key == nil {
		return out, nil
	}
	key, err := m.deps.Distributions.GenerateKey(ctx, distribution.GenerateKeyInput{
		KeyGroup: g.ID, Algorithm: in.Key.Algorithm, Bits: in.Key.Bits, Comment: in.Key.Comment,
	})
	if err != nil {
		if derr := m.deps.Distributions.DeleteKeyGroup(context.WithoutCancel(ctx), g.ID); derr != nil {
			m.deps.Logger.WarnContext(ctx, "admin: remove the key group whose key could not be generated", "keygroup", g.ID, "err", derr)
		}
		return nil, err
	}
	out.Key = &key
	if out.KeyGroup, err = m.deps.Distributions.GetKeyGroup(ctx, g.ID); err != nil {
		// The group was created: report it with its new key.
		out.KeyGroup = g
		out.KeyGroup.Keys = append(out.KeyGroup.Keys, key.PublicKey)
	}
	return out, nil
}

// DeleteKeyGroup implements DistributionManager.
func (m *distributionManager) DeleteKeyGroup(ctx context.Context, req Requester, id string) error {
	if err := m.authorizeKeyGroup(ctx, req, id); err != nil {
		return err
	}
	return m.deps.Distributions.DeleteKeyGroup(ctx, id)
}

// AddKey implements DistributionManager.
func (m *distributionManager) AddKey(ctx context.Context, in AddKeyInput) (*AddedKey, error) {
	if err := m.authorizeKeyGroup(ctx, in.Requester, in.KeyGroup); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.PublicKeyPEM) != "" {
		if in.Algorithm != "" || in.Bits != 0 {
			return nil, fmt.Errorf("%w: the algorithm and the size are those of the public key provided", domain.ErrInvalidArgument)
		}
		k, err := m.deps.Distributions.AddPublicKey(ctx, distribution.AddKeyInput{KeyGroup: in.KeyGroup, PEM: in.PublicKeyPEM, Comment: in.Comment})
		if err != nil {
			return nil, err
		}
		return &AddedKey{PublicKey: k}, nil
	}
	g, err := m.deps.Distributions.GenerateKey(ctx, distribution.GenerateKeyInput{
		KeyGroup: in.KeyGroup, Algorithm: in.Algorithm, Bits: in.Bits, Comment: in.Comment,
	})
	if err != nil {
		return nil, err
	}
	return &AddedKey{PublicKey: g.PublicKey, PrivateKeyPEM: g.PrivateKeyPEM}, nil
}

// DeleteKey implements DistributionManager.
func (m *distributionManager) DeleteKey(ctx context.Context, req Requester, group, keyID string) error {
	if err := m.authorizeKeyGroup(ctx, req, group); err != nil {
		return err
	}
	return m.deps.Distributions.DeleteKey(ctx, group, keyID)
}
