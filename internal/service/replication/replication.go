package replication

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Documents of the system store.
const (
	// TargetsDir holds the targets of every bucket: <bucket>.json.
	TargetsDir = "replication/targets"
	// ResyncDir holds the resync of every bucket: <bucket>.json.
	ResyncDir = "replication/resync"

	arnPrefix = "arn:nexo:replication::"
)

// Deps are the dependencies of the service.
type Deps struct {
	// Objects reads the versions to replicate and records their state
	// (required).
	Objects object.Service
	// Buckets provides the replication configuration of the buckets
	// (required).
	Buckets object.BucketConfigSource
	// Docs keeps the targets and the resyncs (required).
	Docs sysstore.Store
	// Sealer seals the secrets of the targets (required).
	Sealer Sealer
	// Dialer opens the clients of the targets (default: S3 over HTTP).
	Dialer Dialer
	// Leader runs the resyncs on one node; nil runs them here.
	Leader Leader
	// QueueDir keeps the tasks of this node on a local drive; "" keeps
	// them in memory (lost with the process).
	QueueDir string
	Clock    clock.Clock
	Logger   *slog.Logger
}

// Option configures the service.
type Option func(*options)

type options struct {
	workers     int
	partSize    int64
	singleLimit int64
	queueLimit  int
	maxAttempts int
	backoffMin  time.Duration
	backoffMax  time.Duration
	poll        time.Duration
	batch       int
	highWater   int
	cacheTTL    time.Duration
}

func defaultOptions() options {
	return options{
		workers: 4, partSize: 16 << 20, singleLimit: 32 << 20, queueLimit: 500_000, maxAttempts: 30,
		backoffMin: time.Second, backoffMax: 5 * time.Minute, poll: 5 * time.Second, batch: 500, highWater: 10_000,
		cacheTTL: 30 * time.Second,
	}
}

// WithWorkers sets the number of tasks replicated at once by this node
// (default 4).
func WithWorkers(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.workers = n
		}
	}
}

// WithPartSize sets the size of the parts of the versions sent in parts
// and the size up to which a version is sent in one request (defaults
// 16 MiB and 32 MiB). Each worker buffers one part.
func WithPartSize(part, single int64) Option {
	return func(o *options) {
		if part >= 5<<20 {
			o.partSize = part
		}
		if single > 0 {
			o.singleLimit = single
		}
	}
}

// WithBackoff sets the delays between the attempts of a failing task
// (defaults 1s, doubling up to 5m) and the attempts after which it is
// dropped, its version left FAILED for a resync (default 30).
func WithBackoff(minDelay, maxDelay time.Duration, attempts int) Option {
	return func(o *options) {
		if minDelay > 0 {
			o.backoffMin = minDelay
		}
		if maxDelay >= minDelay && maxDelay > 0 {
			o.backoffMax = maxDelay
		}
		if attempts > 0 {
			o.maxAttempts = attempts
		}
	}
}

// WithPollInterval sets how often the leader resumes the resyncs (default
// 5s).
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.poll = d
		}
	}
}

// WithQueueLimit bounds the tasks of the queue of this node (default
// 500000): beyond, the versions stay PENDING for a resync.
func WithQueueLimit(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.queueLimit = n
		}
	}
}

// storedTarget is a target in the system store, its secret sealed.
type storedTarget struct {
	Target
	Secret []byte `json:"secret"`
}

// targetsDoc is the document of the targets of a bucket.
type targetsDoc struct {
	Targets []storedTarget `json:"targets"`
}

type service struct {
	deps  Deps
	opts  options
	queue *queue

	mu      sync.Mutex
	stats   map[string]map[string]*TargetStatus
	remotes map[string]cachedRemote

	runMu   sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

// cachedRemote is the client of a target, opened at most cacheTTL ago.
type cachedRemote struct {
	remote Remote
	at     time.Time
}

var _ Service = (*service)(nil)

// New returns the service.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil || deps.Buckets == nil || deps.Docs == nil || deps.Sealer == nil {
		return nil, fmt.Errorf("%w: replication: the object layer, the bucket settings, the system store and the sealer are required", domain.ErrInvalidArgument)
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
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	q, err := openQueue(deps.QueueDir, o.queueLimit)
	if err != nil {
		return nil, err
	}
	return &service{deps: deps, opts: o, queue: q, stats: map[string]map[string]*TargetStatus{}, remotes: map[string]cachedRemote{}}, nil
}

// DeriveKey derives the key sealing the secrets of the targets from the
// root credentials and the deployment id: every node of the deployment
// derives the same key, no other deployment does.
func DeriveKey(rootUser, rootPassword, deploymentID string) []byte {
	mac := hmac.New(sha256.New, []byte(rootPassword))
	mac.Write([]byte("nexo-replication-v1\x00" + rootUser + "\x00" + deploymentID))
	return mac.Sum(nil)
}

// --- targets -------------------------------------------------------------------------

func targetsPath(bucket string) string { return TargetsDir + "/" + bucket + ".json" }

func (s *service) loadTargets(ctx context.Context, bucket string) (targetsDoc, error) {
	var doc targetsDoc
	data, err := s.deps.Docs.Get(ctx, targetsPath(bucket))
	if errors.Is(err, sysstore.ErrNotFound) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("replication: read the targets of %s: %w", bucket, err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("replication: decode the targets of %s: %w", bucket, err)
	}
	return doc, nil
}

func (s *service) saveTargets(ctx context.Context, bucket string, doc targetsDoc) error {
	slices.SortFunc(doc.Targets, func(a, b storedTarget) int { return strings.Compare(a.ARN, b.ARN) })
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("replication: encode the targets of %s: %w", bucket, err)
	}
	if err := s.deps.Docs.Put(ctx, targetsPath(bucket), data); err != nil {
		return fmt.Errorf("replication: write the targets of %s: %w", bucket, err)
	}
	return nil
}

// aad binds a sealed secret to its bucket and target.
func aad(bucket, arn string) []byte {
	return []byte("nexo-replication-target\x00" + bucket + "\x00" + arn)
}

// validEndpoint checks the URL of a target.
func validEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return fmt.Errorf("%w: the endpoint must be http(s)://host[:port], not %q", domain.ErrInvalidArgument, endpoint)
	}
	return nil
}

func newTargetID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SetTarget implements Service.
func (s *service) SetTarget(ctx context.Context, bucket string, in TargetInput) (Target, error) {
	in.Endpoint = strings.TrimSuffix(strings.TrimSpace(in.Endpoint), "/")
	if err := validEndpoint(in.Endpoint); err != nil {
		return Target{}, err
	}
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return Target{}, fmt.Errorf("%w: remote bucket %q", domain.ErrInvalidArgument, in.Bucket)
	}
	if in.AccessKey == "" {
		return Target{}, fmt.Errorf("%w: the access key of the target is required", domain.ErrInvalidArgument)
	}
	doc, err := s.loadTargets(ctx, bucket)
	if err != nil {
		return Target{}, err
	}
	now := s.deps.Clock.Now().UTC()
	idx := -1
	if in.ARN != "" {
		idx = slices.IndexFunc(doc.Targets, func(t storedTarget) bool { return t.ARN == in.ARN })
		if idx < 0 {
			return Target{}, fmt.Errorf("%w: replication target %s of bucket %s", domain.ErrNotFound, in.ARN, bucket)
		}
		if !strings.HasSuffix(in.ARN, ":"+in.Bucket) {
			return Target{}, fmt.Errorf("%w: the remote bucket of target %s cannot change", domain.ErrInvalidArgument, in.ARN)
		}
	}
	secret := in.SecretKey
	if secret == "" {
		if idx < 0 {
			return Target{}, fmt.Errorf("%w: the secret key of the target is required", domain.ErrInvalidArgument)
		}
		plain, err := s.deps.Sealer.Open(ctx, doc.Targets[idx].Secret, aad(bucket, in.ARN))
		if err != nil {
			return Target{}, fmt.Errorf("replication: open the secret of %s: %w", in.ARN, err)
		}
		secret = string(plain)
	}
	cfg := RemoteConfig{Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region, AccessKey: in.AccessKey, SecretKey: secret}
	remote, err := s.deps.Dialer.Dial(ctx, cfg)
	if err != nil {
		return Target{}, fmt.Errorf("%w: replication target: %w", domain.ErrInvalidArgument, err)
	}
	if err := remote.CheckBucket(ctx); err != nil {
		return Target{}, fmt.Errorf("%w: replication target %s/%s: %w", domain.ErrInvalidArgument, in.Endpoint, in.Bucket, err)
	}
	t := Target{ARN: in.ARN, Endpoint: in.Endpoint, Bucket: in.Bucket, Region: in.Region, AccessKey: in.AccessKey, Label: in.Label, Created: now, Updated: now}
	if idx >= 0 {
		t.Created = doc.Targets[idx].Created
	} else {
		t.ARN = arnPrefix + newTargetID() + ":" + in.Bucket
	}
	sealed, err := s.deps.Sealer.Seal(ctx, []byte(secret), aad(bucket, t.ARN))
	if err != nil {
		return Target{}, fmt.Errorf("replication: seal the secret of %s: %w", t.ARN, err)
	}
	st := storedTarget{Target: t, Secret: sealed}
	if idx >= 0 {
		doc.Targets[idx] = st
	} else {
		doc.Targets = append(doc.Targets, st)
	}
	if err := s.saveTargets(ctx, bucket, doc); err != nil {
		return Target{}, err
	}
	s.forgetRemote(t.ARN)
	s.deps.Logger.InfoContext(ctx, "replication: target set", "bucket", bucket, "arn", t.ARN, "endpoint", t.Endpoint)
	return t, nil
}

// ListTargets implements Service.
func (s *service) ListTargets(ctx context.Context, bucket string) ([]Target, error) {
	doc, err := s.loadTargets(ctx, bucket)
	if err != nil {
		return nil, err
	}
	out := make([]Target, 0, len(doc.Targets))
	for _, t := range doc.Targets {
		out = append(out, t.Target)
	}
	return out, nil
}

// RemoveTarget implements Service.
func (s *service) RemoveTarget(ctx context.Context, bucket, arn string) error {
	doc, err := s.loadTargets(ctx, bucket)
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(doc.Targets, func(t storedTarget) bool { return t.ARN == arn })
	if idx < 0 {
		return fmt.Errorf("%w: replication target %s of bucket %s", domain.ErrNotFound, arn, bucket)
	}
	cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
	if err != nil {
		return err
	}
	if slices.Contains(cfg.Replication.DestinationARNs(), arn) {
		return fmt.Errorf("%w: the replication configuration of %s still replicates to %s", domain.ErrInvalidArgument, bucket, arn)
	}
	doc.Targets = slices.Delete(doc.Targets, idx, idx+1)
	if err := s.saveTargets(ctx, bucket, doc); err != nil {
		return err
	}
	s.forgetRemote(arn)
	return nil
}

// RemoveBucket implements Service.
func (s *service) RemoveBucket(ctx context.Context, bucket string) error {
	s.queue.DropBucket(bucket)
	if err := s.deps.Docs.Delete(ctx, targetsPath(bucket)); err != nil {
		return fmt.Errorf("replication: remove the targets of %s: %w", bucket, err)
	}
	if err := s.deps.Docs.Delete(ctx, resyncPath(bucket)); err != nil {
		return fmt.Errorf("replication: remove the resync of %s: %w", bucket, err)
	}
	s.mu.Lock()
	delete(s.stats, bucket)
	s.mu.Unlock()
	return nil
}

// ValidateConfig implements Service.
func (s *service) ValidateConfig(ctx context.Context, bucket string, doc []byte) ([]byte, error) {
	targets, err := s.loadTargets(ctx, bucket)
	if err != nil {
		return nil, err
	}
	cfg, err := domain.ParseReplicationConfig(doc, func(arn string) bool {
		return slices.ContainsFunc(targets.Targets, func(t storedTarget) bool { return t.ARN == arn })
	})
	if err != nil {
		return nil, err
	}
	return cfg.Encode()
}

// remote returns the client of a target of bucket.
func (s *service) remote(ctx context.Context, bucket, arn string) (Remote, error) {
	now := s.deps.Clock.Now()
	s.mu.Lock()
	c, ok := s.remotes[arn]
	s.mu.Unlock()
	if ok && now.Sub(c.at) < s.opts.cacheTTL {
		return c.remote, nil
	}
	doc, err := s.loadTargets(ctx, bucket)
	if err != nil {
		return nil, err
	}
	idx := slices.IndexFunc(doc.Targets, func(t storedTarget) bool { return t.ARN == arn })
	if idx < 0 {
		return nil, fmt.Errorf("%w: replication target %s of bucket %s", errNoTarget, arn, bucket)
	}
	t := doc.Targets[idx]
	secret, err := s.deps.Sealer.Open(ctx, t.Secret, aad(bucket, arn))
	if err != nil {
		return nil, fmt.Errorf("replication: open the secret of %s: %w", arn, err)
	}
	r, err := s.deps.Dialer.Dial(ctx, RemoteConfig{Endpoint: t.Endpoint, Bucket: t.Bucket, Region: t.Region, AccessKey: t.AccessKey, SecretKey: string(secret)})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.remotes[arn] = cachedRemote{remote: r, at: now}
	s.mu.Unlock()
	return r, nil
}

func (s *service) forgetRemote(arn string) {
	s.mu.Lock()
	delete(s.remotes, arn)
	s.mu.Unlock()
}

// errNoTarget is a task for a target that no longer exists: it is dropped.
var errNoTarget = errors.New("replication target not found")

// --- events ------------------------------------------------------------------------

// Notify implements Service.
func (s *service) Notify(ctx context.Context, e event.Event) {
	if object.IsReplica(ctx) {
		return
	}
	bucket, o := e.S3.Bucket.Name, e.S3.Object
	if bucket != "" && e.EventName.Match(event.BucketRemoved) {
		// The targets, the resync and the tasks of a deleted bucket go.
		go func() {
			if err := s.RemoveBucket(context.WithoutCancel(ctx), bucket); err != nil {
				s.deps.Logger.WarnContext(ctx, "replication: the settings of a deleted bucket stay", "bucket", bucket, "err", err)
			}
		}()
		return
	}
	if bucket == "" || o.Key == "" {
		return
	}
	name := e.EventName
	var tasks []task
	switch {
	case name.Match(event.ObjectCreatedAll):
		tasks = []task{{Op: opObject}}
	case name.Match(event.ObjectTaggingAll):
		tasks = []task{{Op: opTags}}
	case name == event.ObjectRemovedDeleteMarkerCreated || name == event.LifecycleExpirationDeleteMarkerCreated,
		name == event.ObjectRemovedDelete && o.VersionID != "" && o.VersionID != "null":
		cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
		if err != nil || cfg.Replication == nil {
			return
		}
		marker := name != event.ObjectRemovedDelete
		for _, r := range cfg.Replication.Destinations(o.Key, nil) {
			if marker && r.ReplicatesDeleteMarkers() {
				tasks = append(tasks, task{Op: opDeleteMarker, Target: r.Destination.Bucket, ModTime: sequencerTime(o.Sequencer)})
			} else if !marker && r.ReplicatesDeletes() {
				tasks = append(tasks, task{Op: opDelete, Target: r.Destination.Bucket})
			}
		}
	default:
		return
	}
	if len(tasks) == 0 {
		return
	}
	if tasks[0].Op == opObject || tasks[0].Op == opTags {
		// The version records its destinations; a bucket without
		// configuration has none to queue.
		cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
		if err != nil || cfg.Replication == nil {
			return
		}
	}
	now := s.deps.Clock.Now().UTC()
	for _, t := range tasks {
		t.Bucket, t.Key, t.VersionID, t.Queued = bucket, o.Key, o.VersionID, now
		if err := s.queue.Put(t); err != nil {
			s.deps.Logger.WarnContext(ctx, "replication: task not queued, the version stays pending for a resync", "bucket", bucket, "key", o.Key, "versionId", o.VersionID, "err", err)
		}
	}
}

// sequencerTime returns the time of an event sequencer (hexadecimal Unix
// nanoseconds).
func sequencerTime(seq string) time.Time {
	n, err := strconv.ParseInt(seq, 16, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// --- status ------------------------------------------------------------------------

// stat returns the counters of a target (s.mu held).
func (s *service) stat(bucket, arn string) *TargetStatus {
	m, ok := s.stats[bucket]
	if !ok {
		m = map[string]*TargetStatus{}
		s.stats[bucket] = m
	}
	st, ok := m[arn]
	if !ok {
		st = &TargetStatus{ARN: arn, Online: true}
		m[arn] = st
	}
	return st
}

// record updates the counters of a target after an attempt.
func (s *service) record(bucket, arn string, err error, update func(*TargetStatus)) {
	now := s.deps.Clock.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stat(bucket, arn)
	if err != nil {
		st.Online, st.Failed, st.LastError, st.LastFailed = false, st.Failed+1, err.Error(), now
		return
	}
	st.Online, st.LastOK = true, now
	if update != nil {
		update(st)
	}
}

// Status implements Service.
func (s *service) Status(ctx context.Context, bucket string) (Status, error) {
	targets, err := s.ListTargets(ctx, bucket)
	if err != nil {
		return Status{}, err
	}
	total, per := s.queue.Count(bucket)
	out := Status{Queued: total, Targets: make([]TargetStatus, 0, len(targets))}
	s.mu.Lock()
	for _, t := range targets {
		st := *s.stat(bucket, t.ARN)
		st.Queued = per[t.ARN]
		out.Targets = append(out.Targets, st)
	}
	s.mu.Unlock()
	if r, err := s.readResync(ctx, bucket); err != nil {
		return Status{}, err
	} else if r != nil {
		out.Resync = r
	}
	return out, nil
}
