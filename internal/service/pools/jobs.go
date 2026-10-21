package pools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// walk moves the objects of pool, from the cursor of job, bucket by
// bucket in name order, page by page in key order; every page saves the
// progress. move moves one key and reports whether the walk goes on
// (false: stop where it stands, the job is not over). It returns whether
// the walk reached the end of the pool, and stops early (false) when the
// context ends, the node stops leading or keep says the job is no longer
// wanted.
func (s *service) walk(ctx context.Context, pool int, job *Job, path string, keep func(context.Context) bool,
	move func(bucket, key string) bool) (bool, error) {
	buckets, err := s.deps.Objects.ListBuckets(ctx)
	if err != nil {
		return false, fmt.Errorf("list the buckets: %w", err)
	}
	names := make([]string, 0, len(buckets))
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	slices.Sort(names)
	for bi, name := range names {
		if name < job.Bucket {
			continue
		}
		after := ""
		if name == job.Bucket {
			after = job.Object
		}
		for {
			if ctx.Err() != nil || !s.leading() || !keep(ctx) {
				return false, nil
			}
			keys, err := s.deps.Mover.PoolKeys(ctx, pool, name, after, s.opts.batch)
			if errors.Is(err, domain.ErrBucketNotFound) {
				break // deleted meanwhile
			}
			if err != nil {
				return false, fmt.Errorf("list the objects of %s: %w", name, err)
			}
			if len(keys) == 0 {
				break
			}
			for _, key := range keys {
				if !move(name, key) {
					return false, s.saveProgress(ctx, path, job)
				}
				job.Bucket, job.Object = name, key
			}
			after = keys[len(keys)-1]
			if err := s.writeJob(ctx, path, job); err != nil {
				return false, err
			}
		}
		// The next bucket starts from its beginning.
		if bi+1 < len(names) {
			job.Bucket, job.Object = names[bi+1], ""
		}
	}
	return true, nil
}

// runDecommission advances the decommission of pool requested at
// requested: passes over its objects until one finds nothing left, then
// waits for its multipart uploads to end.
func (s *service) runDecommission(ctx context.Context, pool int, requested time.Time) {
	id := s.deps.PoolIDs[pool]
	path := progressPath(id)
	logger := s.deps.Logger.With("pool", pool+1)
	job, err := s.readJob(ctx, path)
	if err != nil {
		logger.WarnContext(ctx, "pools: decommission", "err", err)
		return
	}
	if job == nil || !job.StartedAt.Equal(requested) {
		job = &Job{State: JobRunning, StartedAt: requested, Pool: pool, Pass: 1}
		logger.InfoContext(ctx, "pools: decommission started")
		if err := s.writeJob(ctx, path, job); err != nil {
			logger.WarnContext(ctx, "pools: decommission", "err", err)
			return
		}
	}
	if job.State != JobRunning || s.deps.Clock.Now().Before(job.NextPassAt) {
		return
	}
	keep := func(ctx context.Context) bool {
		return s.stillRequested(ctx, func(c control) bool {
			at, ok := c.Decommission[id]
			return ok && at.Equal(requested)
		})
	}
	for {
		done, err := s.walk(ctx, pool, job, path, keep, func(bucket, key string) bool {
			job.Seen++
			res, err := s.deps.Mover.MoveObject(ctx, object.MoveObjectInput{Bucket: bucket, Key: key, From: pool})
			if err != nil {
				if ctx.Err() != nil {
					return false
				}
				job.Failed++
				job.Error = err.Error()
				logger.WarnContext(ctx, "pools: decommission: object not moved, retried at the next pass", "bucket", bucket, "key", key, "err", err)
				return true
			}
			if res.Versions > 0 {
				job.Objects++
				job.Versions += int64(res.Versions)
				job.Bytes += res.Bytes
			}
			return true
		})
		if err != nil {
			job.Error = err.Error()
			logger.WarnContext(ctx, "pools: decommission", "err", err)
			_ = s.writeJob(ctx, path, job)
			return
		}
		if !done {
			return
		}
		if job.Seen > 0 {
			// Something was there: another pass checks that nothing is
			// left (objects written while the pass ran included).
			if job.Failed > 0 && job.Pass >= s.opts.maxPasses {
				job.State, job.FinishedAt = JobFailed, s.deps.Clock.Now().UTC()
				job.Error = fmt.Sprintf("%d objects could not be moved after %d passes, last error: %s", job.Failed, job.Pass, job.Error)
				logger.ErrorContext(ctx, "pools: decommission failed", "err", job.Error)
				_ = s.writeJob(ctx, path, job)
				return
			}
			retry := job.Failed > 0
			job.Pass++
			job.Seen, job.Failed, job.Bucket, job.Object = 0, 0, "", ""
			if retry {
				job.NextPassAt = s.deps.Clock.Now().Add(s.opts.retry).UTC()
				_ = s.writeJob(ctx, path, job)
				return
			}
			job.NextPassAt = time.Time{}
			continue
		}
		uploads, err := s.deps.Mover.PoolUploads(ctx, pool)
		if err != nil {
			job.Error = err.Error()
			_ = s.writeJob(ctx, path, job)
			return
		}
		job.PendingUploads = uploads
		if uploads > 0 {
			// The uploads complete in the pool: the next pass moves
			// their objects. Nothing to do until then.
			job.Bucket, job.Object = "", ""
			_ = s.writeJob(ctx, path, job)
			return
		}
		job.State, job.FinishedAt, job.Error = JobComplete, s.deps.Clock.Now().UTC(), ""
		logger.InfoContext(ctx, "pools: decommission complete: the pool can be removed from the command line",
			"objects", job.Objects, "versions", job.Versions, "bytes", job.Bytes, "passes", job.Pass)
		_ = s.writeJob(ctx, path, job)
		return
	}
}

// ratios returns the used fraction of every pool that is not draining
// (-1 for the others) and the used fraction of all of them together.
func ratios(usage []object.PoolUsage) ([]float64, float64) {
	out := make([]float64, len(usage))
	var used, total uint64
	for i, u := range usage {
		out[i] = -1
		if u.Draining || u.Total == 0 {
			continue
		}
		out[i] = 1 - float64(u.Free)/float64(u.Total)
		used += u.Total - u.Free
		total += u.Total
	}
	if total == 0 {
		return out, 0
	}
	return out, float64(used) / float64(total)
}

// runRebalance advances the rebalance requested at requested: the pools
// fuller than the average by more than the slack give objects, in pool
// order, to the pools emptier than the average, until they are within
// the slack or have nothing left to give.
func (s *service) runRebalance(ctx context.Context, requested time.Time) {
	job, err := s.readJob(ctx, rebalancePath)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "pools: rebalance", "err", err)
		return
	}
	if job == nil || !job.StartedAt.Equal(requested) {
		job = &Job{State: JobRunning, StartedAt: requested, Pass: 1}
		s.deps.Logger.InfoContext(ctx, "pools: rebalance started")
		if err := s.writeJob(ctx, rebalancePath, job); err != nil {
			s.deps.Logger.WarnContext(ctx, "pools: rebalance", "err", err)
			return
		}
	}
	if job.State != JobRunning {
		return
	}
	keep := func(ctx context.Context) bool {
		return s.stillRequested(ctx, func(c control) bool { return c.Rebalance.Equal(requested) })
	}
	finish := func(state JobState, msg string) {
		job.State, job.FinishedAt, job.Error = state, s.deps.Clock.Now().UTC(), msg
		s.deps.Logger.InfoContext(ctx, "pools: rebalance "+string(state), "objects", job.Objects, "bytes", job.Bytes, "err", msg)
		_ = s.writeJob(ctx, rebalancePath, job)
	}
	for {
		usage := s.deps.Mover.PoolUsage(ctx)
		r, goal := ratios(usage)
		src := -1
		for i := job.Pool; i < len(r); i++ {
			if r[i] > goal+s.opts.slack {
				src = i
				break
			}
		}
		if src < 0 {
			finish(JobComplete, "")
			return
		}
		var targets []int
		for i, ri := range r {
			if ri >= 0 && ri < goal {
				targets = append(targets, i)
			}
		}
		if len(targets) == 0 {
			finish(JobComplete, "")
			return
		}
		if src != job.Pool {
			job.Pool, job.Bucket, job.Object = src, "", ""
		}
		balanced := false
		moved := 0
		done, err := s.walk(ctx, src, job, rebalancePath, keep, func(bucket, key string) bool {
			res, err := s.deps.Mover.MoveObject(ctx, object.MoveObjectInput{Bucket: bucket, Key: key, From: src, Targets: targets})
			if err != nil {
				if ctx.Err() != nil {
					return false
				}
				if errors.Is(err, domain.ErrStorageFull) {
					balanced = true // nowhere to go: this pool is done
					return false
				}
				job.Failed++
				job.Error = err.Error()
				s.deps.Logger.WarnContext(ctx, "pools: rebalance: object not moved", "bucket", bucket, "key", key, "err", err)
				return true
			}
			if res.Versions > 0 {
				job.Objects++
				job.Versions += int64(res.Versions)
				job.Bytes += res.Bytes
			}
			moved++
			if moved%s.opts.check != 0 {
				return true
			}
			// Every few objects: the source stops once near the average,
			// and a target that reached it takes no more (it would give
			// the objects back).
			r, goal := ratios(s.deps.Mover.PoolUsage(ctx))
			if r[src] <= goal+s.opts.slack/2 {
				balanced = true
				return false
			}
			targets = slices.DeleteFunc(targets, func(i int) bool { return r[i] >= goal })
			if len(targets) == 0 {
				balanced = true
				return false
			}
			return true
		})
		if err != nil {
			job.Error = err.Error()
			s.deps.Logger.WarnContext(ctx, "pools: rebalance", "err", err)
			_ = s.writeJob(ctx, rebalancePath, job)
			return
		}
		if !done && !balanced {
			return // stopped, or no longer leading
		}
		// This pool gave what it could: the next one.
		job.Pool, job.Bucket, job.Object = src+1, "", ""
		if err := s.writeJob(ctx, rebalancePath, job); err != nil {
			return
		}
	}
}
