package heal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// job is a healing job of this node.
type job struct {
	mu          sync.Mutex
	j           Job
	drives      map[[3]int]*JobDrive
	maxItems    int
	lastPersist time.Time
}

// snapshot returns a deep copy of the job state.
func (jb *job) snapshot() Job {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	out := jb.j
	out.Items = slices.Clone(jb.j.Items)
	out.Drives = make([]JobDrive, 0, len(jb.drives))
	for _, d := range jb.drives {
		out.Drives = append(out.Drives, *d)
	}
	slices.SortFunc(out.Drives, func(a, b JobDrive) int {
		return cmp.Or(cmp.Compare(a.Pool, b.Pool), cmp.Compare(a.Set, b.Set), cmp.Compare(a.Disk, b.Disk))
	})
	return out
}

// record adds the outcome of one item to the job.
func (jb *job) record(now time.Time, item JobItem, bytes int64, dangling, removed int, err error) {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	j := &jb.j
	j.Scanned++
	j.UpdatedAt = now
	j.Current = strings.TrimSuffix(item.Bucket+"/"+item.Object, "/")
	j.Bytes += uint64(max(bytes, 0))
	j.Dangling += uint64(dangling) // #nosec G115 -- non-negative counters of a heal result
	j.Removed += uint64(removed)   // #nosec G115 -- non-negative counters of a heal result
	switch {
	case err != nil:
		j.Failed++
		item.Error = err.Error()
	case item.Healed > 0:
		j.Healed++
	}
	damaged := false
	for _, hd := range item.Drives {
		key := [3]int{hd.Pool, hd.Set, hd.Disk}
		jd, ok := jb.drives[key]
		if !ok {
			jd = &JobDrive{Endpoint: hd.Endpoint, Pool: hd.Pool, Set: hd.Set, Disk: hd.Disk}
			jb.drives[key] = jd
		}
		jd.State = "online"
		switch hd.Before {
		case object.HealDriveMissing:
			jd.Missing++
			damaged = true
		case object.HealDriveCorrupt:
			jd.Corrupt++
			damaged = true
		case object.HealDriveOffline:
			jd.Offline++
			jd.State = "offline"
		}
		if hd.Before != object.HealDriveOK && hd.Before != object.HealDriveOffline && hd.After == object.HealDriveOK {
			jd.Healed++
		}
	}
	item.Dangling, item.Removed = dangling > 0, removed > 0
	if err == nil && !damaged && dangling == 0 && item.Healed == 0 {
		return
	}
	if len(j.Items) < jb.maxItems {
		j.Items = append(j.Items, item)
	} else {
		j.ItemsTruncated = true
	}
}

// due reports whether the job state should be persisted again.
func (jb *job) due(now time.Time, every time.Duration) bool {
	jb.mu.Lock()
	defer jb.mu.Unlock()
	if now.Sub(jb.lastPersist) < every {
		return false
	}
	jb.lastPersist = now
	return true
}

// StartJob implements Service.
func (s *service) StartJob(ctx context.Context, in JobRequest) (Job, error) {
	if err := validateRequest(in); err != nil {
		return Job{}, err
	}
	now := s.deps.Clock.Now().UTC()
	s.mu.Lock()
	running := 0
	for _, jb := range s.jobs {
		snap := jb.snapshot()
		if snap.State != JobRunning {
			continue
		}
		if snap.Request == in {
			s.mu.Unlock()
			return snap, nil
		}
		running++
	}
	if running >= s.opts.maxJobs {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("%w: heal: %d healing jobs are running", domain.ErrTooManyRequests, running)
	}
	jb := &job{
		j:        Job{ID: s.deps.IDs.NewID(), Request: in, State: JobRunning, Node: s.deps.NodeID, StartedAt: now, UpdatedAt: now},
		drives:   map[[3]int]*JobDrive{},
		maxItems: s.opts.jobItems,
	}
	s.jobs[jb.j.ID] = jb
	s.mu.Unlock()
	s.persistJob(ctx, jb.snapshot())
	if !s.spawn(func(ctx context.Context) { s.runJob(ctx, jb) }) {
		s.finishJob(ctx, jb, context.Canceled)
	}
	s.deps.Logger.InfoContext(ctx, "heal: job started", "id", jb.j.ID, "bucket", in.Bucket, "prefix", in.Prefix,
		"recursive", in.Recursive, "dry_run", in.DryRun, "deep", in.Deep, "remove", in.Remove)
	return jb.snapshot(), nil
}

// validateRequest checks the target of a job.
func validateRequest(in JobRequest) error {
	if in.Bucket == "" {
		if in.Prefix != "" {
			return fmt.Errorf("%w: heal: a prefix needs a bucket", domain.ErrInvalidArgument)
		}
		return nil
	}
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return fmt.Errorf("%w: heal: %w", domain.ErrInvalidArgument, err)
	}
	if in.Prefix != "" {
		if err := domain.ValidateObjectPrefix(in.Prefix); err != nil {
			return fmt.Errorf("%w: heal: %w", domain.ErrInvalidArgument, err)
		}
	}
	return nil
}

// runJob runs a job to its end.
func (s *service) runJob(ctx context.Context, jb *job) {
	req := jb.snapshot().Request
	opts := req.Options
	err := func() error {
		if req.Bucket == "" {
			res, err := s.HealFormat(ctx, opts)
			s.recordItem(ctx, jb, JobItem{Kind: ItemFormat, Drives: res.Drives, Healed: len(res.Healed)}, 0, 0, 0, err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		onBucket := func(res BucketResult, err error) {
			s.recordItem(ctx, jb, JobItem{Kind: ItemBucket, Bucket: res.Bucket, Drives: res.Drives, Healed: res.Healed}, 0, 0, 0, err)
		}
		switch {
		case req.Recursive:
			return s.healPrefix(ctx, req.Bucket, req.Prefix, opts, onBucket, func(res ObjectResult, err error) {
				item := JobItem{Kind: ItemObject, Bucket: res.Bucket, Object: res.Object, Drives: res.Drives, Healed: res.Healed}
				s.recordItem(ctx, jb, item, res.Bytes, res.Dangling, res.Removed, err)
			}, nil)
		case req.Bucket == "":
			buckets, err := s.buckets(ctx)
			if err != nil {
				return err
			}
			for _, b := range buckets {
				res, err := s.HealBucket(ctx, b, opts)
				if res.Bucket == "" {
					res.Bucket = b
				}
				onBucket(res, err)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
			return nil
		case req.Prefix == "":
			res, err := s.HealBucket(ctx, req.Bucket, opts)
			onBucket(res, err)
			return err
		default:
			res, err := s.healAll(ctx, req.Bucket, req.Prefix, opts)
			item := JobItem{Kind: ItemObject, Bucket: req.Bucket, Object: req.Prefix, Drives: res.Drives, Healed: res.Healed}
			s.recordItem(ctx, jb, item, res.Bytes, res.Dangling, res.Removed, err)
			return err
		}
	}()
	s.finishJob(ctx, jb, err)
}

// recordItem adds an item to the job and persists its progress now and
// then.
func (s *service) recordItem(ctx context.Context, jb *job, item JobItem, bytes int64, dangling, removed int, err error) {
	now := s.deps.Clock.Now().UTC()
	jb.record(now, item, bytes, dangling, removed, err)
	if jb.due(now, s.opts.persistAfter) {
		s.persistJob(ctx, jb.snapshot())
	}
}

// finishJob records the end of a job and persists its final state.
func (s *service) finishJob(ctx context.Context, jb *job, err error) {
	now := s.deps.Clock.Now().UTC()
	jb.mu.Lock()
	j := &jb.j
	j.FinishedAt, j.UpdatedAt, j.Current = now, now, ""
	switch {
	case err == nil:
		j.State = JobDone
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		j.State = JobCanceled
		j.Error = "canceled"
	default:
		j.State = JobFailed
		j.Error = err.Error()
	}
	jb.mu.Unlock()
	snap := jb.snapshot()
	s.persistJob(context.WithoutCancel(ctx), snap)
	s.deps.Logger.Info("heal: job finished", "id", snap.ID, "state", snap.State, "scanned", snap.Scanned,
		"healed", snap.Healed, "failed", snap.Failed, "dangling", snap.Dangling, "error", snap.Error)
}

// jobDrives are the drives holding the persisted job states: those of the
// first erasure set, which every node reaches.
func (s *service) jobDrives() []storage.StorageAPI { return online(s.sets[0].drives) }

func jobPath(id string) string { return JobsDir + "/" + id + ".json" }

// validJobID refuses the ids that are not a single path segment.
func validJobID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\\x00")
}

// persistJob saves the state of a job on the job drives (best effort).
func (s *service) persistJob(ctx context.Context, j Job) {
	data, err := json.Marshal(j)
	if err != nil {
		s.deps.Logger.Warn("heal: encode job", "id", j.ID, "err", err)
		return
	}
	for _, d := range s.jobDrives() {
		if err := d.WriteAll(ctx, domain.SystemBucket, jobPath(j.ID), data); err != nil && ctx.Err() == nil {
			s.deps.Logger.Debug("heal: cannot persist job", "id", j.ID, "drive", d.Endpoint().String(), "err", err)
		}
	}
}

// loadJob reads the newest persisted state of job id.
func (s *service) loadJob(ctx context.Context, id string) (Job, bool) {
	var best Job
	found := false
	for _, d := range s.jobDrives() {
		data, err := d.ReadAll(ctx, domain.SystemBucket, jobPath(id))
		if err != nil {
			continue
		}
		var j Job
		if err := json.Unmarshal(data, &j); err != nil || j.ID != id {
			continue
		}
		if !found || j.UpdatedAt.After(best.UpdatedAt) {
			best, found = j, true
		}
	}
	return best, found
}

// persistedIDs lists the ids of the persisted jobs.
func (s *service) persistedIDs(ctx context.Context) []string {
	seen := map[string]bool{}
	for _, d := range s.jobDrives() {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, JobsDir+"/*.json", true)
		if err != nil {
			continue
		}
		for _, info := range infos {
			if info.Dir {
				continue
			}
			if id := strings.TrimSuffix(path.Base(info.Name), ".json"); validJobID(id) {
				seen[id] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// expired reports a job finished longer than the retention ago.
func (s *service) expired(j Job, now time.Time) bool {
	return j.State != JobRunning && !j.FinishedAt.IsZero() && now.Sub(j.FinishedAt) > s.opts.jobRetention
}

// Job implements Service.
func (s *service) Job(ctx context.Context, id string) (Job, error) {
	if !validJobID(id) {
		return Job{}, fmt.Errorf("%w: healing job %q", domain.ErrNotFound, id)
	}
	s.mu.Lock()
	jb, ok := s.jobs[id]
	s.mu.Unlock()
	if ok {
		return jb.snapshot(), nil
	}
	if j, ok := s.loadJob(ctx, id); ok && !s.expired(j, s.deps.Clock.Now()) {
		return j, nil
	}
	return Job{}, fmt.Errorf("%w: healing job %q", domain.ErrNotFound, id)
}

// Jobs implements Service.
func (s *service) Jobs(ctx context.Context) ([]Job, error) {
	now := s.deps.Clock.Now()
	s.mu.Lock()
	local := make(map[string]*job, len(s.jobs))
	for id, jb := range s.jobs {
		local[id] = jb
	}
	s.mu.Unlock()
	out := make([]Job, 0, len(local))
	for _, jb := range local {
		if j := jb.snapshot(); !s.expired(j, now) {
			out = append(out, j)
		}
	}
	for _, id := range s.persistedIDs(ctx) {
		if _, ok := local[id]; ok {
			continue
		}
		if j, ok := s.loadJob(ctx, id); ok && !s.expired(j, now) {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b Job) int {
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), strings.Compare(a.ID, b.ID))
	})
	return out, ctx.Err()
}

// expireJobs forgets the jobs finished longer than the retention ago and
// removes their persisted state.
func (s *service) expireJobs(ctx context.Context) {
	now := s.deps.Clock.Now()
	var drop []string
	s.mu.Lock()
	for id, jb := range s.jobs {
		if s.expired(jb.snapshot(), now) {
			delete(s.jobs, id)
			drop = append(drop, id)
		}
	}
	s.mu.Unlock()
	for _, id := range s.persistedIDs(ctx) {
		if slices.Contains(drop, id) {
			continue
		}
		if j, ok := s.loadJob(ctx, id); ok && s.expired(j, now) {
			drop = append(drop, id)
		}
	}
	for _, id := range drop {
		for _, d := range s.jobDrives() {
			if err := d.Delete(ctx, domain.SystemBucket, jobPath(id), storage.DeleteOptions{}); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
				s.deps.Logger.Debug("heal: cannot remove expired job", "id", id, "drive", d.Endpoint().String(), "err", err)
			}
		}
	}
}
