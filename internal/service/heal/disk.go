package heal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// systemRoots are the system directories whose files every drive holds a
// copy of (bucket metadata, cluster configuration and IAM): a healed drive
// gets the copies it misses.
func systemRoots() []string { return []string{storage.BucketsDir, storage.ConfigDir} }

// localDrive returns the local drive of endpoint with its position.
func (s *service) localDrive(endpoint string) (driveRef, error) {
	for _, d := range s.allDrives() {
		if d.drive != nil && d.drive.Endpoint().String() == endpoint {
			if !isLocal(d.drive) {
				return driveRef{}, fmt.Errorf("%w: heal: drive %s belongs to another node", domain.ErrInvalidArgument, endpoint)
			}
			return d, nil
		}
	}
	return driveRef{}, fmt.Errorf("%w: heal: no drive %s in the deployment", domain.ErrDiskNotFound, endpoint)
}

// claimDisk marks a drive as being healed; false when it already is.
func (s *service) claimDisk(endpoint string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disks[endpoint] {
		return false
	}
	s.disks[endpoint] = true
	return true
}

func (s *service) releaseDisk(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.disks, endpoint)
}

// diskHealer is the state of one HealDisk run.
type diskHealer struct {
	s       *service
	ref     driveRef
	tracker storage.HealingTracker
	res     DiskResult
	// since counts the objects healed since the last checkpoint.
	since     int
	lastSaved time.Time
}

// HealDisk implements Service.
func (s *service) HealDisk(ctx context.Context, endpoint string) (DiskResult, error) {
	ref, err := s.localDrive(endpoint)
	if err != nil {
		return DiskResult{Endpoint: endpoint}, err
	}
	d := ref.drive
	if !d.IsOnline() {
		return DiskResult{Endpoint: endpoint}, fmt.Errorf("%w: heal: drive %s is offline", domain.ErrDiskNotFound, endpoint)
	}
	if !s.claimDisk(endpoint) {
		return DiskResult{Endpoint: endpoint}, fmt.Errorf("%w: heal: drive %s is already being healed", domain.ErrConflict, endpoint)
	}
	defer s.releaseDisk(endpoint)

	now := s.deps.Clock.Now().UTC()
	h := &diskHealer{s: s, ref: ref, lastSaved: now}
	if t := d.Healing(); t != nil {
		h.tracker = *t
		h.res.Resumed = t.Bucket != "" || len(t.HealedBuckets) > 0
	} else {
		h.tracker = storage.HealingTracker{
			PoolIndex: ref.pool, SetIndex: ref.set, DiskIndex: ref.disk,
			Path: d.Endpoint().Path, Endpoint: endpoint, Started: now,
		}
		if info, err := d.DiskInfo(ctx, storage.DiskInfoOptions{NoOp: true}); err == nil {
			h.tracker.ID = info.ID
		}
	}
	h.tracker.Finished = false
	h.res.Endpoint, h.res.Started = endpoint, h.tracker.Started
	if err := h.checkpoint(ctx); err != nil {
		return h.res, fmt.Errorf("heal: drive %s: %w", endpoint, err)
	}
	s.deps.Logger.Info("heal: healing drive", "drive", endpoint, "pool", ref.pool+1, "set", ref.set+1, "disk", ref.disk+1,
		"resumed", h.res.Resumed, "bucket", h.tracker.Bucket, "object", h.tracker.Object)

	if err := h.run(ctx); err != nil {
		// Keep the progress: the next run resumes from it.
		if cerr := h.checkpoint(context.WithoutCancel(ctx)); cerr != nil {
			s.deps.Logger.Warn("heal: cannot save the drive healing checkpoint", "drive", endpoint, "err", cerr)
		}
		return h.res, fmt.Errorf("heal: drive %s: %w", endpoint, err)
	}
	h.res.Finished = s.deps.Clock.Now().UTC()
	if err := d.Delete(ctx, domain.SystemBucket, storage.HealingFile, storage.DeleteOptions{}); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
		return h.res, fmt.Errorf("heal: drive %s: remove the healing tracker: %w", endpoint, err)
	}
	s.deps.Logger.Info("heal: drive healed", "drive", endpoint, "buckets", h.res.Buckets, "objects", h.res.Objects,
		"healed", h.res.Healed, "failed", h.res.Failed, "bytes", h.res.Bytes, "took", h.res.Finished.Sub(h.res.Started).String())
	return h.res, nil
}

// sources returns the other drives of the set of the healed drive.
func (h *diskHealer) sources() []storage.StorageAPI {
	set := h.s.deps.Pools[h.ref.pool].Sets[h.ref.set]
	out := make([]storage.StorageAPI, 0, len(set)-1)
	for i, d := range set {
		if i != h.ref.disk {
			out = append(out, d)
		}
	}
	return out
}

// run heals the system files, then bucket by bucket every object of the
// set, skipping what the tracker records as done.
func (h *diskHealer) run(ctx context.Context) error {
	sources := h.sources()
	if _, err := h.s.healSystemFiles(ctx, sources, h.ref.drive); err != nil {
		h.s.deps.Logger.Warn("heal: system files not healed", "drive", h.res.Endpoint, "err", err)
	}
	buckets, err := listBuckets(ctx, sources)
	if err != nil {
		return err
	}
	for _, b := range buckets {
		if slices.Contains(h.tracker.HealedBuckets, b) {
			continue
		}
		after := ""
		if b == h.tracker.Bucket {
			after = h.tracker.Object
		} else {
			h.tracker.Bucket, h.tracker.Object = b, ""
		}
		if _, err := h.s.HealBucket(ctx, b, Options{}); err != nil {
			if errors.Is(err, domain.ErrBucketNotFound) {
				continue // removed meanwhile
			}
			return err
		}
		h.res.Buckets++
		err := walkKeys(ctx, sources, b, "", after, func(key string) error {
			return h.healObject(ctx, b, key)
		})
		if err != nil && !errors.Is(err, domain.ErrBucketNotFound) {
			return err
		}
		h.tracker.HealedBuckets = append(h.tracker.HealedBuckets, b)
		h.tracker.Bucket, h.tracker.Object = b, ""
		if err := h.checkpoint(ctx); err != nil {
			return err
		}
	}
	return nil
}

// healObject heals every version of one object of the set and records it
// in the tracker.
func (h *diskHealer) healObject(ctx context.Context, bucket, key string) error {
	res, err := h.s.healAll(ctx, bucket, key, Options{})
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case gone(err):
	case err != nil:
		h.res.Failed++
		h.tracker.ObjectsFailed++
		h.tracker.BytesFailed += uint64(max(res.Bytes, 0))
		h.s.deps.Logger.Warn("heal: object not healed", "drive", h.res.Endpoint, "bucket", bucket, "object", key, "err", err)
	default:
		h.res.Objects++
		h.res.Bytes += uint64(max(res.Bytes, 0))
		if res.Healed > 0 {
			h.res.Healed++
		}
		h.tracker.ObjectsHealed++
		h.tracker.BytesDone += uint64(max(res.Bytes, 0))
	}
	h.tracker.Bucket, h.tracker.Object = bucket, key
	h.since++
	if h.since >= h.s.opts.checkpointEvery || h.s.deps.Clock.Now().Sub(h.lastSaved) >= h.s.opts.checkpointAfter {
		return h.checkpoint(ctx)
	}
	return nil
}

// checkpoint saves the tracker on the healed drive.
func (h *diskHealer) checkpoint(ctx context.Context) error {
	now := h.s.deps.Clock.Now().UTC()
	h.tracker.LastUpdate = now
	if err := writeTracker(ctx, h.ref.drive, h.tracker); err != nil {
		return err
	}
	h.since, h.lastSaved = 0, now
	return nil
}

// healSystemFiles copies onto target the system files (bucket metadata,
// configuration) the other drives of its set hold and it misses: the copy
// most of them agree on. It returns the number of files written.
func (s *service) healSystemFiles(ctx context.Context, sources []storage.StorageAPI, target storage.StorageAPI) (int, error) {
	sources = online(sources)
	files := map[string]bool{}
	for _, root := range systemRoots() {
		collectFiles(ctx, sources, root, files, 0)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	written := 0
	var errs []error
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if _, err := target.StatInfoFile(ctx, domain.SystemBucket, name, false); err == nil {
			continue
		} else if !errors.Is(err, domain.ErrObjectNotFound) && !errors.Is(err, domain.ErrBucketNotFound) {
			errs = append(errs, err)
			continue
		}
		data, ok := majorityCopy(ctx, sources, name)
		if !ok {
			continue
		}
		if err := target.WriteAll(ctx, domain.SystemBucket, name, data); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		written++
	}
	return written, errors.Join(errs...)
}

// maxSystemDepth bounds the recursion into the system directories.
const maxSystemDepth = 8

// collectFiles adds to files the paths of the files below dir on any of
// drives (union).
func collectFiles(ctx context.Context, drives []storage.StorageAPI, dir string, files map[string]bool, depth int) {
	if depth > maxSystemDepth {
		return
	}
	dirs := map[string]bool{}
	for _, d := range drives {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, dir+"/*", true)
		if err != nil {
			continue
		}
		for _, info := range infos {
			name := path.Clean(info.Name)
			if !strings.HasPrefix(name, dir+"/") {
				continue
			}
			if info.Dir {
				dirs[name] = true
			} else {
				files[name] = true
			}
		}
	}
	for sub := range dirs {
		collectFiles(ctx, drives, sub, files, depth+1)
	}
}

// majorityCopy reads name from drives and returns the content held by
// most of them.
func majorityCopy(ctx context.Context, drives []storage.StorageAPI, name string) ([]byte, bool) {
	var copies [][]byte
	var counts []int
	for _, d := range drives {
		data, err := d.ReadAll(ctx, domain.SystemBucket, name)
		if err != nil {
			continue
		}
		found := false
		for i, c := range copies {
			if bytes.Equal(c, data) {
				counts[i]++
				found = true
				break
			}
		}
		if !found {
			copies = append(copies, data)
			counts = append(counts, 1)
		}
	}
	best := -1
	for i, c := range counts {
		if best < 0 || c > counts[best] {
			best = i
		}
	}
	if best < 0 {
		return nil, false
	}
	return copies[best], true
}

// monitor checks the local drives periodically: it formats the empty ones
// and heals the ones marked for healing (resuming the healings a restart
// interrupted), and drops the expired jobs.
func (s *service) monitor(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.opts.monitorInterval)
	defer t.Stop()
	for {
		s.checkDrives(ctx)
		s.expireJobs(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkDrives runs one round of the monitor.
func (s *service) checkDrives(ctx context.Context) {
	if _, err := s.HealFormat(ctx, Options{}); err != nil && ctx.Err() == nil {
		s.deps.Logger.Warn("heal: format check", "err", err)
	}
	for _, ref := range s.allDrives() {
		d := ref.drive
		if !isLocal(d) || !d.IsOnline() {
			continue
		}
		if t := d.Healing(); t == nil || t.Finished {
			continue
		}
		ep := d.Endpoint().String()
		s.mu.Lock()
		busy := s.disks[ep]
		s.mu.Unlock()
		if busy {
			continue
		}
		s.spawn(func(ctx context.Context) {
			if _, err := s.HealDisk(ctx, ep); err != nil && ctx.Err() == nil && !errors.Is(err, domain.ErrConflict) {
				s.deps.Logger.Warn("heal: drive healing interrupted", "drive", ep, "err", err)
			}
		})
	}
}
