package heal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// driveRef is a drive with its position in the deployment.
type driveRef struct {
	pool, set, disk int
	drive           storage.StorageAPI
}

// service is the healing service.
type service struct {
	deps Deps
	opts options

	// sets lists the drives of every erasure set, pool by pool.
	sets []setRef

	mu sync.Mutex
	// disks marks the local drives being healed (HealDisk).
	disks map[string]bool
	// jobs are the jobs of this node, by id.
	jobs map[string]*job

	runMu   sync.Mutex
	started bool
	// base is canceled by Stop: it bounds the background work (workers,
	// monitor, jobs, drive healings). stopping (under mu) refuses new
	// background work once Stop began.
	base     context.Context
	stopping bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// setRef is an erasure set with its position.
type setRef struct {
	pool, set int
	drives    []storage.StorageAPI
}

var _ Service = (*service)(nil)

// New returns the healing service. Healer and Pools are required (a
// wiring error otherwise, domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Healer == nil {
		return nil, fmt.Errorf("%w: heal: the healer (erasure object layer) is required", domain.ErrInvalidArgument)
	}
	if len(deps.Pools) == 0 {
		return nil, fmt.Errorf("%w: heal: the drives of at least one pool are required", domain.ErrInvalidArgument)
	}
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	if deps.IDs == nil {
		deps.IDs = topology.NewUUIDGenerator()
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
	s := &service{deps: deps, opts: o, disks: map[string]bool{}, jobs: map[string]*job{}, base: context.Background()}
	for p, pool := range deps.Pools {
		if len(pool.Sets) == 0 {
			return nil, fmt.Errorf("%w: heal: pool %d has no erasure set", domain.ErrInvalidArgument, p+1)
		}
		for i, drives := range pool.Sets {
			s.sets = append(s.sets, setRef{pool: p, set: i, drives: drives})
		}
	}
	return s, nil
}

// allDrives returns every drive of the deployment with its position.
func (s *service) allDrives() []driveRef {
	var out []driveRef
	for _, st := range s.sets {
		for i, d := range st.drives {
			out = append(out, driveRef{pool: st.pool, set: st.set, disk: i, drive: d})
		}
	}
	return out
}

// isLocal reports a drive attached to this node.
func isLocal(d storage.StorageAPI) bool { return d != nil && d.Endpoint().IsLocal }

// Start implements lifecycle.Runner.
func (s *service) Start(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.started {
		return nil
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.base, s.stopping = base, false
	s.mu.Unlock()
	s.cancel, s.started = cancel, true
	if s.deps.Queue != nil {
		for range s.opts.mrfWorkers {
			s.wg.Add(1)
			go s.mrfWorker(base)
		}
	}
	if s.opts.monitorInterval > 0 {
		s.wg.Add(1)
		go s.monitor(base)
	}
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if !s.started {
		return nil
	}
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("heal: stop: %w", ctx.Err())
	}
	s.started = false
	return nil
}

// spawn runs fn in the background, tracked by Stop; it reports false
// (and runs nothing) once Stop began.
func (s *service) spawn(fn func(ctx context.Context)) bool {
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return false
	}
	ctx := s.base
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		fn(ctx)
	}()
	return true
}

func healOptions(o Options) object.HealOptions {
	return object.HealOptions{Deep: o.Deep, DryRun: o.DryRun, Remove: o.Remove}
}

func objectResult(r object.HealResult) ObjectResult {
	return ObjectResult{
		Bucket: r.Bucket, Object: r.Key, VersionIDs: r.VersionIDs, Drives: r.DriveStates,
		Healed: r.Healed, Dangling: r.Dangling, Removed: r.Removed, Bytes: r.Bytes,
	}
}

// HealObject implements Service.
func (s *service) HealObject(ctx context.Context, bucket, obj, versionID string, opts Options) (ObjectResult, error) {
	r, err := s.deps.Healer.HealObject(ctx, bucket, obj, versionID, healOptions(opts))
	res := objectResult(r)
	if err != nil {
		return res, fmt.Errorf("heal: object %s/%s: %w", bucket, obj, err)
	}
	return res, nil
}

// healAll heals every version of bucket/obj.
func (s *service) healAll(ctx context.Context, bucket, obj string, opts Options) (ObjectResult, error) {
	ho := healOptions(opts)
	ho.AllVersions = true
	r, err := s.deps.Healer.HealObject(ctx, bucket, obj, "", ho)
	res := objectResult(r)
	if res.Bucket == "" {
		res.Bucket, res.Object = bucket, obj
	}
	if err != nil {
		return res, fmt.Errorf("heal: object %s/%s: %w", bucket, obj, err)
	}
	return res, nil
}

// HealBucket implements Service.
func (s *service) HealBucket(ctx context.Context, bucket string, opts Options) (BucketResult, error) {
	r, err := s.deps.Healer.HealBucket(ctx, bucket, healOptions(opts))
	res := BucketResult{Bucket: bucket, Drives: r.DriveStates, Healed: r.Healed}
	if err != nil {
		return res, fmt.Errorf("heal: bucket %s: %w", bucket, err)
	}
	return res, nil
}

// gone reports the errors of an item deleted while it was healed.
func gone(err error) bool {
	return errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNoSuchVersion)
}

// HealPrefix implements Service.
func (s *service) HealPrefix(ctx context.Context, bucket, prefix string, opts Options, fn ObjectFunc) (Summary, error) {
	var sum Summary
	err := s.healPrefix(ctx, bucket, prefix, opts, nil, func(res ObjectResult, err error) {
		sum.add(res, err)
		if fn != nil {
			fn(res, err)
		}
	}, &sum)
	return sum, err
}

func (sum *Summary) add(res ObjectResult, err error) {
	sum.Objects++
	sum.Bytes += uint64(max(res.Bytes, 0))
	sum.Dangling += uint64(res.Dangling) // #nosec G115 -- non-negative counters of a heal result
	sum.Removed += uint64(res.Removed)   // #nosec G115 -- non-negative counters of a heal result
	switch {
	case err != nil:
		sum.Failed++
	case res.Healed > 0:
		sum.Healed++
	}
}

// bucketFunc receives the outcome of every bucket of a prefix walk.
type bucketFunc func(res BucketResult, err error)

// healPrefix heals the buckets (bucket, or every bucket) and every object
// under prefix, set by set.
func (s *service) healPrefix(ctx context.Context, bucket, prefix string, opts Options, onBucket bucketFunc, onObject ObjectFunc, sum *Summary) error {
	buckets := []string{bucket}
	if bucket == "" {
		var err error
		if buckets, err = s.buckets(ctx); err != nil {
			return err
		}
	}
	for _, b := range buckets {
		res, err := s.HealBucket(ctx, b, opts)
		if sum != nil {
			sum.Buckets++
		}
		if onBucket != nil {
			onBucket(res, err)
		}
		if err != nil {
			if bucket != "" || ctx.Err() != nil {
				return err
			}
			continue // removed meanwhile
		}
		for _, st := range s.sets {
			err := walkKeys(ctx, st.drives, b, prefix, "", func(key string) error {
				res, err := s.healAll(ctx, b, key, opts)
				if gone(err) {
					return nil
				}
				if onObject != nil {
					onObject(res, err)
				}
				return ctx.Err()
			})
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, domain.ErrBucketNotFound) {
					continue
				}
				return fmt.Errorf("heal: walk %s (pool %d set %d): %w", b, st.pool+1, st.set+1, err)
			}
		}
	}
	return nil
}

// buckets returns the buckets of the deployment: the union of the drives
// of every set.
func (s *service) buckets(ctx context.Context) ([]string, error) {
	var drives []storage.StorageAPI
	for _, st := range s.sets {
		drives = append(drives, st.drives...)
	}
	names, err := listBuckets(ctx, drives)
	if err != nil {
		return nil, fmt.Errorf("heal: list buckets: %w", err)
	}
	return names, nil
}

// mrfWorker heals the entries of the MRF queue until ctx ends.
func (s *service) mrfWorker(ctx context.Context) {
	defer s.wg.Done()
	for {
		e, err := s.deps.Queue.Next(ctx)
		if err != nil {
			return
		}
		// Deep on the first attempt: a read that met bitrot reports a shard
		// of the right size, which only the verification of its blocks
		// finds. The retries wait for offline drives: the presence and
		// size checks are enough.
		opts := object.HealOptions{Deep: e.Attempts == 0, Remove: s.opts.mrfRemove}
		r, err := s.deps.Healer.HealObject(ctx, e.Bucket, e.Object, e.VersionID, opts)
		if err == nil && r.Healed > 0 {
			s.deps.Logger.Debug("heal: MRF healed", "bucket", e.Bucket, "object", e.Object, "version_id", e.VersionID, "drives", r.Healed)
		}
		done := context.WithoutCancel(ctx)
		switch {
		case ctx.Err() != nil:
			// Interrupted by the shutdown: the entry is not healed, it
			// stays queued (and persisted) for the next run.
			s.deps.Queue.Release(done, e, false)
			return
		case err == nil && r.Offline > 0:
			// A drive of the set is still offline: heal it once back,
			// however long it stays away.
			s.deps.Queue.Release(done, e, true)
		case err == nil, gone(err), errors.Is(err, domain.ErrBucketNotFound):
			s.deps.Queue.Done(done, e, false)
		default:
			s.deps.Logger.Debug("heal: MRF heal failed", "bucket", e.Bucket, "object", e.Object, "version_id", e.VersionID,
				"attempt", e.Attempts+1, "err", err)
			s.deps.Queue.Done(done, e, true)
		}
	}
}
