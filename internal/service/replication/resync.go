package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

func resyncPath(bucket string) string { return ResyncDir + "/" + bucket + ".json" }

func (s *service) readResync(ctx context.Context, bucket string) (*Resync, error) {
	data, err := s.deps.Docs.Get(ctx, resyncPath(bucket))
	if errors.Is(err, sysstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("replication: read the resync of %s: %w", bucket, err)
	}
	r := &Resync{}
	if err := json.Unmarshal(data, r); err != nil {
		return nil, fmt.Errorf("replication: decode the resync of %s: %w", bucket, err)
	}
	return r, nil
}

func (s *service) writeResync(ctx context.Context, bucket string, r *Resync) error {
	r.UpdatedAt = s.deps.Clock.Now().UTC()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("replication: encode the resync of %s: %w", bucket, err)
	}
	if err := s.deps.Docs.Put(ctx, resyncPath(bucket), data); err != nil {
		return fmt.Errorf("replication: write the resync of %s: %w", bucket, err)
	}
	return nil
}

// StartResync implements Service.
func (s *service) StartResync(ctx context.Context, bucket, target string) (Resync, error) {
	cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
	if err != nil {
		return Resync{}, err
	}
	if !slices.Contains(cfg.Replication.DestinationARNs(), target) {
		return Resync{}, fmt.Errorf("%w: the replication configuration of %s does not replicate to %q", domain.ErrInvalidArgument, bucket, target)
	}
	now := s.deps.Clock.Now().UTC()
	r := &Resync{ID: newTargetID(), Target: target, State: ResyncRunning, StartedAt: now}
	if err := s.writeResync(ctx, bucket, r); err != nil {
		return Resync{}, err
	}
	s.deps.Logger.InfoContext(ctx, "replication: resync started", "bucket", bucket, "target", target)
	return *r, nil
}

// resyncLoop advances the running resyncs on the leader.
func (s *service) resyncLoop(ctx context.Context) {
	ticker := time.NewTicker(s.opts.poll)
	defer ticker.Stop()
	for {
		if s.deps.Leader == nil || s.deps.Leader.IsLeader() {
			s.resyncAll(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// resyncAll advances every running resync.
func (s *service) resyncAll(ctx context.Context) {
	docs, err := s.deps.Docs.List(ctx, ResyncDir)
	if err != nil {
		if ctx.Err() == nil {
			s.deps.Logger.DebugContext(ctx, "replication: cannot list the resyncs", "err", err)
		}
		return
	}
	for name := range docs {
		bucket, ok := strings.CutSuffix(name, ".json")
		if !ok || ctx.Err() != nil {
			continue
		}
		r, err := s.readResync(ctx, bucket)
		if err != nil || r == nil || r.State != ResyncRunning {
			continue
		}
		s.resync(ctx, bucket, r)
	}
}

// resync walks the versions of bucket from where r stands, page by page,
// and queues every version the rules for the target select; it stops when
// the queue is high (the next round goes on) or the resync was replaced.
func (s *service) resync(ctx context.Context, bucket string, r *Resync) {
	fail := func(err error) {
		r.State, r.Error, r.FinishedAt = ResyncFailed, err.Error(), s.deps.Clock.Now().UTC()
		s.deps.Logger.WarnContext(ctx, "replication: resync failed", "bucket", bucket, "target", r.Target, "err", err)
		_ = s.writeResync(context.WithoutCancel(ctx), bucket, r)
	}
	for ctx.Err() == nil {
		if s.queue.Len() >= s.opts.highWater {
			return
		}
		cur, err := s.readResync(ctx, bucket)
		if err != nil || cur == nil || cur.ID != r.ID {
			return // replaced or removed meanwhile
		}
		cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
		if err != nil {
			fail(err)
			return
		}
		if cfg.Replication == nil || !slices.Contains(cfg.Replication.DestinationARNs(), r.Target) {
			fail(fmt.Errorf("the replication configuration no longer replicates to %s", r.Target))
			return
		}
		page, err := s.deps.Objects.ListObjectVersions(ctx, domain.ListObjectsInput{
			Bucket: bucket, Marker: r.Key, VersionIDMarker: r.VersionID, MaxKeys: s.opts.batch, IncludeVersions: true,
		})
		if err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			return
		}
		now := s.deps.Clock.Now().UTC()
		for _, v := range page.Objects {
			r.Scanned++
			if v.ReplicationStatus == domain.ReplicationReplica {
				continue
			}
			rule, ok := ruleFor(cfg.Replication, r.Target, v)
			if !ok {
				continue
			}
			t := task{Op: opObject, Bucket: bucket, Key: v.Key, VersionID: v.VersionID, Target: r.Target, Resync: true, Queued: now}
			if v.IsDeleteMarker {
				if !rule.ReplicatesDeleteMarkers() {
					continue
				}
				t = task{Op: opDeleteMarker, Bucket: bucket, Key: v.Key, VersionID: v.VersionID, Target: r.Target, ModTime: v.ModTime, Queued: now}
			}
			if err := s.queue.Put(t); err != nil {
				_ = s.writeResync(ctx, bucket, r)
				return // full: the next round goes on from here
			}
			r.Queued++
		}
		if !page.IsTruncated || len(page.Objects) == 0 {
			r.State, r.FinishedAt, r.Key, r.VersionID = ResyncComplete, now, "", ""
			s.deps.Logger.InfoContext(ctx, "replication: resync complete", "bucket", bucket, "target", r.Target, "scanned", r.Scanned, "queued", r.Queued)
			_ = s.writeResync(ctx, bucket, r)
			return
		}
		last := page.Objects[len(page.Objects)-1]
		r.Key, r.VersionID = last.Key, last.VersionID
		if page.NextMarker != "" {
			r.Key, r.VersionID = page.NextMarker, page.NextVersionIDMarker
		}
		if err := s.writeResync(ctx, bucket, r); err != nil {
			return
		}
	}
}

// ruleFor returns the rule that replicates v to target, if any.
func ruleFor(cfg *domain.ReplicationConfig, target string, v domain.ObjectInfo) (domain.ReplicationRule, bool) {
	for _, r := range cfg.Destinations(v.Key, v.Tags) {
		if r.Destination.Bucket == target {
			return r, true
		}
	}
	return domain.ReplicationRule{}, false
}
