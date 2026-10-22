package tier

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// configPath is the system document of the tiers.
const configPath = "tiers/config.json"

// The metadata of the objects of a tier: the version they hold the data
// of (the key base64url-encoded, metadata values being ASCII).
const (
	metaDeployment = "nexo-deployment"
	metaBucket     = "nexo-bucket"
	metaKey        = "nexo-key"
	metaVersion    = "nexo-version"
)

// Deps are the dependencies of the service.
type Deps struct {
	// Docs keeps the tiers (required).
	Docs sysstore.Store
	// Sealer seals their secrets (required).
	Sealer Sealer
	// DeploymentID names the objects of this deployment (required).
	DeploymentID string
	// Objects finds the orphaned objects (required by the sweep; nil
	// disables it).
	Objects Objects
	// Dialer opens the clients of the tiers (default: S3 over HTTP).
	Dialer Dialer
	// Leader runs the sweep on one node; nil runs it here.
	Leader Leader
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*options)

type options struct {
	sweepEvery time.Duration
	grace      time.Duration
	cacheTTL   time.Duration
}

// WithSweep sets the period of the sweep (default 1h) and the age an
// object of a tier must reach before the sweep may remove it (default 1h):
// an object written by a transition still in progress is never removed.
func WithSweep(every, grace time.Duration) Option {
	return func(o *options) {
		if every > 0 {
			o.sweepEvery = every
		}
		if grace > 0 {
			o.grace = grace
		}
	}
}

// storedTier is a tier in the system store, its secret sealed.
type storedTier struct {
	Tier
	Secret []byte `json:"secret"`
}

// configDoc is the document of the tiers.
type configDoc struct {
	Tiers []storedTier `json:"tiers"`
}

// cachedRemote is the client of a tier, opened at most cacheTTL ago.
type cachedRemote struct {
	remote Remote
	at     time.Time
}

type service struct {
	deps Deps
	opts options

	// mu serializes the changes of the tiers on this node.
	mu sync.Mutex

	cacheMu sync.Mutex
	remotes map[string]cachedRemote

	sweepMu sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
}

var _ Service = (*service)(nil)

// New returns the service.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Docs == nil || deps.Sealer == nil || deps.DeploymentID == "" {
		return nil, fmt.Errorf("%w: tier: the system store, the sealer and the deployment id are required", domain.ErrInvalidArgument)
	}
	if deps.Dialer == nil {
		deps.Dialer = NewS3Dialer(nil)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{sweepEvery: time.Hour, grace: time.Hour, cacheTTL: time.Minute}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &service{deps: deps, opts: o, remotes: map[string]cachedRemote{}}, nil
}

// DeriveKey derives the key sealing the secrets of the tiers from the root
// credentials and the deployment id: every node of the deployment derives
// the same key, no other deployment does.
func DeriveKey(rootUser, rootPassword, deploymentID string) []byte {
	mac := hmac.New(sha256.New, []byte(rootPassword))
	mac.Write([]byte("nexo-tier-v1\x00" + rootUser + "\x00" + deploymentID))
	return mac.Sum(nil)
}

// aad binds a sealed secret to its tier.
func aad(name string) []byte { return []byte("nexo-tier\x00" + name) }

// validName checks the name of a tier.
func validName(name string) error {
	if name == "" || len(name) > 64 || name == domain.StorageClassStandard || name == domain.StorageClassReducedRedundancy {
		return fmt.Errorf("%w: invalid tier name %q", domain.ErrInvalidArgument, name)
	}
	for _, c := range name {
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return fmt.Errorf("%w: invalid tier name %q: upper case letters, digits, '-' and '_' only", domain.ErrInvalidArgument, name)
		}
	}
	return nil
}

// validEndpoint checks the URL of a tier.
func validEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("%w: the endpoint must be http(s)://host[:port], not %q", domain.ErrInvalidArgument, endpoint)
	}
	return nil
}

// validPrefix checks the prefix of the objects of a tier.
func validPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "//") || len(prefix) > 256 {
		return fmt.Errorf("%w: the prefix of a tier must end with '/', not %q", domain.ErrInvalidArgument, prefix)
	}
	return nil
}

func (s *service) load(ctx context.Context) (configDoc, error) {
	var doc configDoc
	data, err := s.deps.Docs.Get(ctx, configPath)
	if errors.Is(err, sysstore.ErrNotFound) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("tier: read the tiers: %w", err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("tier: decode the tiers: %w", err)
	}
	return doc, nil
}

func (s *service) save(ctx context.Context, doc configDoc) error {
	slices.SortFunc(doc.Tiers, func(a, b storedTier) int { return strings.Compare(a.Name, b.Name) })
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("tier: encode the tiers: %w", err)
	}
	if err := s.deps.Docs.Put(ctx, configPath, data); err != nil {
		return fmt.Errorf("tier: write the tiers: %w", err)
	}
	return nil
}

func find(doc configDoc, name string) int {
	return slices.IndexFunc(doc.Tiers, func(t storedTier) bool { return t.Name == name })
}

// check dials a tier and checks its bucket.
func (s *service) check(ctx context.Context, cfg RemoteConfig) error {
	r, err := s.deps.Dialer.Dial(ctx, cfg)
	if err != nil {
		return fmt.Errorf("%w: tier: %w", domain.ErrInvalidArgument, err)
	}
	if err := r.CheckBucket(ctx); err != nil {
		return fmt.Errorf("%w: tier %s/%s: %w", domain.ErrInvalidArgument, cfg.Endpoint, cfg.Bucket, err)
	}
	return nil
}

// Add implements Service.
func (s *service) Add(ctx context.Context, in Input) (Tier, error) {
	in.Endpoint = strings.TrimSuffix(strings.TrimSpace(in.Endpoint), "/")
	if err := validName(in.Name); err != nil {
		return Tier{}, err
	}
	if err := validEndpoint(in.Endpoint); err != nil {
		return Tier{}, err
	}
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return Tier{}, fmt.Errorf("%w: remote bucket %q", domain.ErrInvalidArgument, in.Bucket)
	}
	if err := validPrefix(in.Prefix); err != nil {
		return Tier{}, err
	}
	if in.AccessKey == "" || in.SecretKey == "" {
		return Tier{}, fmt.Errorf("%w: the credentials of the tier are required", domain.ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return Tier{}, err
	}
	if find(doc, in.Name) >= 0 {
		return Tier{}, fmt.Errorf("%w: tier %s exists", domain.ErrConflict, in.Name)
	}
	cfg := RemoteConfig{Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region, StorageClass: in.StorageClass, AccessKey: in.AccessKey, SecretKey: in.SecretKey}
	if err := s.check(ctx, cfg); err != nil {
		return Tier{}, err
	}
	sealed, err := s.deps.Sealer.Seal(ctx, []byte(in.SecretKey), aad(in.Name))
	if err != nil {
		return Tier{}, fmt.Errorf("tier: seal the secret of %s: %w", in.Name, err)
	}
	now := s.deps.Clock.Now().UTC()
	t := Tier{Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket, Prefix: in.Prefix, Region: in.Region,
		StorageClass: in.StorageClass, AccessKey: in.AccessKey, Created: now, Updated: now}
	doc.Tiers = append(doc.Tiers, storedTier{Tier: t, Secret: sealed})
	if err := s.save(ctx, doc); err != nil {
		return Tier{}, err
	}
	s.forget(in.Name)
	s.deps.Logger.InfoContext(ctx, "tier: added", "tier", t.Name, "endpoint", t.Endpoint, "bucket", t.Bucket)
	return t, nil
}

// Edit implements Service.
func (s *service) Edit(ctx context.Context, name string, in Input) (Tier, error) {
	if in.AccessKey == "" || in.SecretKey == "" {
		return Tier{}, fmt.Errorf("%w: the credentials of the tier are required", domain.ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return Tier{}, err
	}
	idx := find(doc, name)
	if idx < 0 {
		return Tier{}, fmt.Errorf("%w: tier %s", domain.ErrNotFound, name)
	}
	t := doc.Tiers[idx].Tier
	if in.Region != "" {
		t.Region = in.Region
	}
	if in.StorageClass != "" {
		t.StorageClass = in.StorageClass
	}
	t.AccessKey = in.AccessKey
	cfg := RemoteConfig{Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region, StorageClass: t.StorageClass, AccessKey: in.AccessKey, SecretKey: in.SecretKey}
	if err := s.check(ctx, cfg); err != nil {
		return Tier{}, err
	}
	sealed, err := s.deps.Sealer.Seal(ctx, []byte(in.SecretKey), aad(name))
	if err != nil {
		return Tier{}, fmt.Errorf("tier: seal the secret of %s: %w", name, err)
	}
	t.Updated = s.deps.Clock.Now().UTC()
	doc.Tiers[idx] = storedTier{Tier: t, Secret: sealed}
	if err := s.save(ctx, doc); err != nil {
		return Tier{}, err
	}
	s.forget(name)
	s.deps.Logger.InfoContext(ctx, "tier: credentials replaced", "tier", name)
	return t, nil
}

// List implements Service.
func (s *service) List(ctx context.Context) ([]Tier, error) {
	doc, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Tier, 0, len(doc.Tiers))
	for _, t := range doc.Tiers {
		out = append(out, t.Tier)
	}
	return out, nil
}

// Exists implements Service.
func (s *service) Exists(ctx context.Context, name string) (bool, error) {
	doc, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	return find(doc, name) >= 0, nil
}

// Remove implements Service.
func (s *service) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := s.load(ctx)
	if err != nil {
		return err
	}
	idx := find(doc, name)
	if idx < 0 {
		return fmt.Errorf("%w: tier %s", domain.ErrNotFound, name)
	}
	r, err := s.remote(ctx, name)
	if err != nil {
		return fmt.Errorf("%w: tier %s cannot be checked: %w", domain.ErrConflict, name, err)
	}
	objs, _, err := r.List(ctx, s.objectPrefix(doc.Tiers[idx].Tier), "")
	if err != nil {
		return fmt.Errorf("%w: tier %s cannot be checked: %w", domain.ErrConflict, name, err)
	}
	if len(objs) > 0 {
		return fmt.Errorf("%w: tier %s holds the data of transitioned versions: bring them back or delete them first", domain.ErrConflict, name)
	}
	doc.Tiers = slices.Delete(doc.Tiers, idx, idx+1)
	if err := s.save(ctx, doc); err != nil {
		return err
	}
	s.forget(name)
	s.deps.Logger.InfoContext(ctx, "tier: removed", "tier", name)
	return nil
}

// objectPrefix is the prefix of the objects of this deployment in a tier.
func (s *service) objectPrefix(t Tier) string {
	return t.Prefix + s.deps.DeploymentID + "/"
}

// remote returns the client of a tier.
func (s *service) remote(ctx context.Context, name string) (Remote, error) {
	now := s.deps.Clock.Now()
	s.cacheMu.Lock()
	c, ok := s.remotes[name]
	s.cacheMu.Unlock()
	if ok && now.Sub(c.at) < s.opts.cacheTTL {
		return c.remote, nil
	}
	doc, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	idx := find(doc, name)
	if idx < 0 {
		return nil, fmt.Errorf("%w: tier %s", domain.ErrNotFound, name)
	}
	t := doc.Tiers[idx]
	secret, err := s.deps.Sealer.Open(ctx, t.Secret, aad(name))
	if err != nil {
		return nil, fmt.Errorf("tier: open the secret of %s: %w", name, err)
	}
	r, err := s.deps.Dialer.Dial(ctx, RemoteConfig{Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region, StorageClass: t.StorageClass,
		AccessKey: t.AccessKey, SecretKey: string(secret)})
	if err != nil {
		return nil, err
	}
	s.cacheMu.Lock()
	s.remotes[name] = cachedRemote{remote: r, at: now}
	s.cacheMu.Unlock()
	return r, nil
}

func (s *service) forget(name string) {
	s.cacheMu.Lock()
	delete(s.remotes, name)
	s.cacheMu.Unlock()
}

// tierOf returns a tier.
func (s *service) tierOf(ctx context.Context, name string) (Tier, error) {
	doc, err := s.load(ctx)
	if err != nil {
		return Tier{}, err
	}
	idx := find(doc, name)
	if idx < 0 {
		return Tier{}, fmt.Errorf("%w: tier %s", domain.ErrNotFound, name)
	}
	return doc.Tiers[idx].Tier, nil
}

// --- object.TierStore -----------------------------------------------------------

// PutTiered implements object.TierStore.
func (s *service) PutTiered(ctx context.Context, name string, r io.Reader, size int64, ref object.TierRef) (string, error) {
	t, err := s.tierOf(ctx, name)
	if err != nil {
		return "", err
	}
	remote, err := s.remote(ctx, name)
	if err != nil {
		return "", err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("tier: object id: %w", err)
	}
	obj := s.objectPrefix(t) + id.String()
	meta := map[string]string{
		metaDeployment: s.deps.DeploymentID,
		metaBucket:     ref.Bucket,
		metaKey:        base64.RawURLEncoding.EncodeToString([]byte(ref.Key)),
		metaVersion:    ref.VersionID,
	}
	if err := remote.Put(ctx, obj, r, size, meta); err != nil {
		// A partial object may be left: the sweep removes it.
		return "", fmt.Errorf("tier %s: write %s: %w", name, obj, err)
	}
	return obj, nil
}

// OpenTiered implements object.TierStore.
func (s *service) OpenTiered(ctx context.Context, name, obj string, offset, length int64) (io.ReadCloser, error) {
	remote, err := s.remote(ctx, name)
	if err != nil {
		return nil, err
	}
	rc, err := remote.Open(ctx, obj, offset, length)
	if err != nil {
		return nil, fmt.Errorf("tier %s: read %s: %w", name, obj, err)
	}
	return rc, nil
}

// DeleteTiered implements object.TierStore.
func (s *service) DeleteTiered(ctx context.Context, name, obj string) error {
	remote, err := s.remote(ctx, name)
	if err != nil {
		return err
	}
	if err := remote.Delete(ctx, obj); err != nil {
		return fmt.Errorf("tier %s: delete %s: %w", name, obj, err)
	}
	return nil
}
