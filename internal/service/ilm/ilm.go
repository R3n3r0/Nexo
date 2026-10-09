package ilm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// Option configures the lifecycle service.
type Option func(*options)

type options struct {
	// tiers are the storage classes the transitions may name.
	tiers    map[string]bool
	pageSize int
}

// WithStorageClasses accepts the transitions to the given storage classes
// (the remote tiers, P2): the other transitions are refused with
// NotImplemented. The transitions are accepted but not applied yet.
func WithStorageClasses(classes ...string) Option {
	return func(o *options) {
		for _, c := range classes {
			if c != "" {
				o.tiers[c] = true
			}
		}
	}
}

// WithPageSize sets the keys listed per page by ApplyNow and the uploads
// per page (default 1000).
func WithPageSize(n int) Option {
	return func(o *options) {
		if n > 0 && n <= 1000 {
			o.pageSize = n
		}
	}
}

// cachedConfig is the parsed lifecycle document of a bucket.
type cachedConfig struct {
	doc []byte
	cfg *Configuration
}

// keyState is what the scanner hook decided for the versions of the key
// being scanned.
type keyState struct {
	bucket, key string
	results     map[string]scanner.LifecycleAction
}

// service is the lifecycle service.
type service struct {
	deps Deps
	opts options

	mu      sync.Mutex
	configs map[string]cachedConfig

	// hookMu serializes the scanner hook and guards cur.
	hookMu sync.Mutex
	cur    *keyState
}

var _ Service = (*service)(nil)

// New returns the lifecycle service. Objects and Buckets are required (a
// wiring error otherwise, domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil || deps.Buckets == nil {
		return nil, fmt.Errorf("%w: ilm: the object layer and the bucket settings are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{tiers: map[string]bool{}, pageSize: 1000}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &service{deps: deps, opts: o, configs: map[string]cachedConfig{}}, nil
}

// Parse implements Service.
func (s *service) Parse(_ context.Context, doc []byte) (*Configuration, error) {
	return parseConfiguration(doc, s.opts.tiers)
}

// Validate implements Service.
func (s *service) Validate(ctx context.Context, doc []byte) ([]byte, error) {
	cfg, err := s.Parse(ctx, doc)
	if err != nil {
		return nil, err
	}
	return encodeConfiguration(cfg)
}

// Evaluate implements Service.
func (s *service) Evaluate(_ context.Context, in EvaluateInput) ([]Action, error) {
	return evaluate(s.deps.Clock.Now(), in), nil
}

// bucketSettings returns the lifecycle configuration of a bucket (nil when
// it has none) and its versioning.
func (s *service) bucketSettings(ctx context.Context, bucket string) (*Configuration, domain.Versioning, error) {
	m, err := s.deps.Buckets.Load(ctx, bucket)
	if err != nil {
		return nil, domain.VersioningOff, fmt.Errorf("ilm: settings of %s: %w", bucket, err)
	}
	if m == nil {
		return nil, domain.VersioningOff, nil
	}
	if len(m.Lifecycle) == 0 {
		return nil, m.Versioning, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.configs[bucket]; ok && bytes.Equal(c.doc, m.Lifecycle) {
		return c.cfg, m.Versioning, nil
	}
	// Stored documents were validated; a configuration this release
	// cannot parse (a transition to a tier no longer configured) applies
	// nothing rather than half of its rules.
	cfg, err := parseConfiguration(m.Lifecycle, s.opts.tiers)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "ilm: lifecycle configuration not applied", "bucket", bucket, "err", err)
		cfg = nil
	}
	s.configs[bucket] = cachedConfig{doc: bytes.Clone(m.Lifecycle), cfg: cfg}
	return cfg, m.Versioning, nil
}

// Expiration implements Service.
func (s *service) Expiration(ctx context.Context, bucket, key, versionID string) (Expiry, error) {
	cfg, _, err := s.bucketSettings(ctx, bucket)
	if err != nil || cfg == nil || !cfg.expiresKey(key) {
		return Expiry{}, err
	}
	info, err := s.deps.Objects.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{VersionID: versionID, NoLock: true})
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNoSuchVersion) || errors.Is(err, domain.ErrMethodNotAllowed) {
			return Expiry{}, nil
		}
		return Expiry{}, fmt.Errorf("ilm: expiration of %s/%s: %w", bucket, key, err)
	}
	if !info.IsLatest {
		return Expiry{}, nil
	}
	d := currentExpiry(cfg, info)
	if !d.ok {
		return Expiry{}, nil
	}
	return Expiry{Date: d.at, RuleID: d.ruleID}, nil
}

// expiresKey reports whether an enabled Expiration rule (by days or date)
// may select key (its tags and size are not known yet).
func (c *Configuration) expiresKey(key string) bool {
	for _, r := range c.Rules {
		e := r.Expiration
		if r.Enabled && e != nil && (e.Days > 0 || !e.Date.IsZero()) && strings.HasPrefix(key, r.Filter.Prefix) {
			return true
		}
	}
	return false
}

// --- scanner hook ----------------------------------------------------------------

// Apply implements scanner.Lifecycle. The first version of a key the
// scanner hands (the current one) evaluates the key: the versions it
// needs are listed, the actions due applied, and the outcome of every
// version kept for the next calls on the same key.
func (s *service) Apply(ctx context.Context, obj domain.ObjectInfo) (scanner.LifecycleAction, error) {
	cfg, versioning, err := s.bucketSettings(ctx, obj.Bucket)
	if err != nil || cfg == nil || !cfg.mayApply(obj.Key) {
		return scanner.LifecycleNone, err
	}
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	if cur := s.cur; cur != nil && !obj.IsLatest && cur.bucket == obj.Bucket && cur.key == obj.Key {
		return cur.results[obj.VersionID], nil
	}
	versions := []domain.ObjectInfo{obj}
	if versioning.Versioned() && (obj.IsDeleteMarker || !obj.IsLatest || cfg.needsVersions()) {
		if versions, err = s.keyVersions(ctx, obj.Bucket, obj.Key); err != nil {
			s.cur = nil
			return scanner.LifecycleNone, err
		}
	}
	var res ApplyResult
	results := s.applyKey(ctx, cfg, obj.Bucket, versioning, versions, &res)
	s.cur = &keyState{bucket: obj.Bucket, key: obj.Key, results: results}
	return results[obj.VersionID], nil
}

// AbortUploads implements scanner.Lifecycle.
func (s *service) AbortUploads(ctx context.Context, bucket string) (int, error) {
	cfg, _, err := s.bucketSettings(ctx, bucket)
	if err != nil || cfg == nil || !cfg.abortsUploads() {
		return 0, err
	}
	var res ApplyResult
	err = s.abortUploads(ctx, cfg, bucket, &res)
	return res.UploadsAborted, err
}

// keyVersions lists every version of key, newest first.
func (s *service) keyVersions(ctx context.Context, bucket, key string) ([]domain.ObjectInfo, error) {
	in := domain.ListObjectsInput{Bucket: bucket, Prefix: key, MaxKeys: s.opts.pageSize, IncludeVersions: true}
	var out []domain.ObjectInfo
	for {
		page, err := s.deps.Objects.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("ilm: versions of %s/%s: %w", bucket, key, err)
		}
		for _, v := range page.Objects {
			if v.Key != key {
				// The versions of the other keys of the prefix come after.
				return out, nil
			}
			out = append(out, v)
		}
		if !page.IsTruncated || (page.NextMarker == "" && page.NextVersionIDMarker == "") {
			return out, nil
		}
		in.Marker, in.VersionIDMarker = page.NextMarker, page.NextVersionIDMarker
	}
}

// applyKey evaluates the versions of a key and applies the actions due;
// it returns what happened to each version.
func (s *service) applyKey(ctx context.Context, cfg *Configuration, bucket string, versioning domain.Versioning, versions []domain.ObjectInfo, res *ApplyResult) map[string]scanner.LifecycleAction {
	results := map[string]scanner.LifecycleAction{}
	res.Versions += len(versions)
	for _, a := range evaluate(s.deps.Clock.Now(), EvaluateInput{Bucket: bucket, Config: cfg, Versioning: versioning, Versions: versions}) {
		results[a.VersionID] = s.execute(ctx, a, res)
	}
	return results
}

// lifecycleContext marks the removals of the lifecycle for the
// notifications (s3:LifecycleExpiration events).
func lifecycleContext(ctx context.Context) context.Context {
	return event.WithRequest(ctx, event.Request{PrincipalID: event.LifecyclePrincipal, Lifecycle: true})
}

// execute applies one action and returns what happened to its version.
func (s *service) execute(ctx context.Context, a Action, res *ApplyResult) scanner.LifecycleAction {
	lctx := lifecycleContext(ctx)
	log := s.deps.Logger.With("bucket", a.Bucket, "key", a.Key, "version_id", a.VersionID, "rule", a.RuleID)
	switch a.Type {
	case ActionDeleteVersion:
		// The version evaluated, if it is still the same: a version
		// written since the listing (a PUT that replaced the null version
		// of a suspended or unversioned bucket) is not expired. A version
		// without id is the null version, whatever the versioning of the
		// bucket: its removal is permanent and never adds a marker.
		vid := a.VersionID
		if vid == "" {
			vid = domain.NullVersionID
		}
		_, err := s.deps.Objects.DeleteObject(lctx, a.Bucket, a.Key, object.DeleteObjectOptions{VersionID: vid, IfModTime: a.ModTime})
		switch {
		case err == nil:
		case errors.Is(err, domain.ErrPreconditionFailed):
			log.DebugContext(ctx, "ilm: expired version changed since its evaluation")
			return scanner.LifecycleNone
		case errors.Is(err, domain.ErrObjectLocked):
			res.Locked++
			log.DebugContext(ctx, "ilm: expired version protected by object lock")
			return scanner.LifecycleNone
		default:
			log.WarnContext(ctx, "ilm: cannot remove an expired version", "err", err)
			return scanner.LifecycleNone
		}
		res.Deleted++
		log.DebugContext(ctx, "ilm: expired version removed")
		return scanner.LifecycleDeleted
	case ActionDeleteMarker:
		// Only over the version evaluated: a newer one is not expired.
		_, err := s.deps.Objects.DeleteObject(lctx, a.Bucket, a.Key, object.DeleteObjectOptions{IfModTime: a.ModTime})
		switch {
		case err == nil:
		case errors.Is(err, domain.ErrPreconditionFailed):
			log.DebugContext(ctx, "ilm: current version changed since its evaluation")
			return scanner.LifecycleNone
		default:
			log.WarnContext(ctx, "ilm: cannot expire the current version", "err", err)
			return scanner.LifecycleNone
		}
		res.DeleteMarkers++
		log.DebugContext(ctx, "ilm: current version expired under a delete marker")
		return scanner.LifecycleNone
	case ActionAbortUpload:
		if err := s.deps.Objects.AbortMultipartUpload(lctx, a.Bucket, a.Key, a.UploadID); err != nil {
			if !errors.Is(err, domain.ErrNoSuchUpload) {
				log.WarnContext(ctx, "ilm: cannot abort an incomplete upload", "upload_id", a.UploadID, "err", err)
			}
			return scanner.LifecycleNone
		}
		res.UploadsAborted++
		log.DebugContext(ctx, "ilm: incomplete upload aborted", "upload_id", a.UploadID)
	}
	return scanner.LifecycleNone
}

// abortUploads evaluates and aborts the uploads of a bucket.
func (s *service) abortUploads(ctx context.Context, cfg *Configuration, bucket string, res *ApplyResult) error {
	in := domain.ListMultipartUploadsInput{Bucket: bucket, MaxUploads: s.opts.pageSize}
	for {
		page, err := s.deps.Objects.ListMultipartUploads(ctx, in)
		if err != nil {
			return fmt.Errorf("ilm: uploads of %s: %w", bucket, err)
		}
		for _, a := range evaluate(s.deps.Clock.Now(), EvaluateInput{Bucket: bucket, Config: cfg, Uploads: page.Uploads}) {
			s.execute(ctx, a, res)
		}
		if !page.IsTruncated || (page.NextKeyMarker == "" && page.NextUploadIDMarker == "") {
			return nil
		}
		in.KeyMarker, in.UploadIDMarker = page.NextKeyMarker, page.NextUploadIDMarker
	}
}

// --- ApplyNow ----------------------------------------------------------------

// ApplyNow implements Service.
func (s *service) ApplyNow(ctx context.Context, bucket string) (ApplyResult, error) {
	var names []string
	if bucket != "" {
		names = []string{bucket}
	} else {
		list, err := s.deps.Objects.ListBuckets(ctx)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("ilm: list buckets: %w", err)
		}
		for _, b := range list {
			names = append(names, b.Name)
		}
	}
	var res ApplyResult
	var errs []error
	for _, name := range names {
		cfg, versioning, err := s.bucketSettings(ctx, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if cfg == nil {
			continue
		}
		res.Buckets++
		if err := s.applyBucket(ctx, cfg, name, versioning, &res); err != nil {
			errs = append(errs, err)
		}
		if cfg.abortsUploads() {
			if err := s.abortUploads(ctx, cfg, name, &res); err != nil {
				errs = append(errs, err)
			}
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	return res, errors.Join(errs...)
}

// applyBucket walks the versions of a bucket, key by key.
func (s *service) applyBucket(ctx context.Context, cfg *Configuration, bucket string, versioning domain.Versioning, res *ApplyResult) error {
	in := domain.ListObjectsInput{Bucket: bucket, MaxKeys: s.opts.pageSize, IncludeVersions: true}
	var group []domain.ObjectInfo
	flush := func() {
		if len(group) > 0 && cfg.mayApply(group[0].Key) {
			s.applyKey(ctx, cfg, bucket, versioning, group, res)
		}
		group = nil
	}
	for {
		page, err := s.deps.Objects.ListObjectVersions(ctx, in)
		if err != nil {
			return fmt.Errorf("ilm: versions of %s: %w", bucket, err)
		}
		for _, v := range page.Objects {
			if len(group) > 0 && group[0].Key != v.Key {
				flush()
			}
			group = append(group, v)
		}
		if !page.IsTruncated || (page.NextMarker == "" && page.NextVersionIDMarker == "") {
			flush()
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		in.Marker, in.VersionIDMarker = page.NextMarker, page.NextVersionIDMarker
	}
}
