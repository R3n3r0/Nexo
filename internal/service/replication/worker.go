package replication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Start implements lifecycle.Runner: the workers of the queue of this
// node, and the resyncs on the leader.
func (s *service) Start(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.started {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done, s.started = cancel, make(chan struct{}), true
	go func(done chan struct{}) {
		defer close(done)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			s.resyncLoop(runCtx)
		}()
		s.dispatch(runCtx)
		<-finished
	}(s.done)
	return nil
}

// Stop implements lifecycle.Runner: the tasks in progress end (or fail,
// to be retried after a restart).
func (s *service) Stop(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if !s.started {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.started = false
	return nil
}

// dispatch hands the due tasks to the workers until ctx ends.
func (s *service) dispatch(ctx context.Context) {
	sem := make(chan struct{}, s.opts.workers)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		t, ok, next := s.queue.Next(s.deps.Clock.Now())
		if ok {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				s.queue.Retry(t, t.NotBefore)
				s.drain(sem)
				return
			}
			go func(t task) {
				defer func() { <-sem }()
				s.handle(ctx, t)
			}(t)
			continue
		}
		wait := time.Hour
		if !next.IsZero() {
			wait = max(next.Sub(s.deps.Clock.Now()), time.Millisecond)
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			s.drain(sem)
			return
		case <-s.queue.wake:
		case <-timer.C:
		}
	}
}

// drain waits for the running tasks.
func (s *service) drain(sem chan struct{}) {
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
}

// handle runs a task and requeues it on failure, with a backoff.
func (s *service) handle(ctx context.Context, t task) {
	err := s.process(ctx, t)
	switch {
	case err == nil, errors.Is(err, errNoTarget):
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "replication: task dropped", "bucket", t.Bucket, "key", t.Key, "err", err)
		}
		s.queue.Done(t)
	case ctx.Err() != nil:
		s.queue.Retry(t, t.NotBefore)
	default:
		t.Attempts++
		if t.Attempts >= s.opts.maxAttempts {
			s.deps.Logger.ErrorContext(ctx, "replication: task given up, a resync replicates the version", "bucket", t.Bucket, "key", t.Key,
				"versionId", t.VersionID, "op", t.Op, "attempts", t.Attempts, "err", err)
			s.queue.Done(t)
			return
		}
		delay := s.opts.backoffMin << min(t.Attempts-1, 20)
		if delay > s.opts.backoffMax || delay <= 0 {
			delay = s.opts.backoffMax
		}
		s.deps.Logger.DebugContext(ctx, "replication: task failed, retried", "bucket", t.Bucket, "key", t.Key, "op", t.Op, "in", delay, "err", err)
		s.queue.Retry(t, s.deps.Clock.Now().Add(delay))
	}
}

// process runs one task; an error retries it.
func (s *service) process(ctx context.Context, t task) error {
	switch t.Op {
	case opObject, opTags:
		return s.replicateVersion(ctx, t)
	case opDeleteMarker, opDelete:
		r, err := s.remote(ctx, t.Bucket, t.Target)
		if err != nil {
			s.record(t.Bucket, t.Target, err, nil)
			return err
		}
		if t.Op == opDeleteMarker {
			err = r.DeleteMarker(ctx, t.Key, t.VersionID, t.ModTime)
		} else {
			err = r.DeleteVersion(ctx, t.Key, t.VersionID)
		}
		s.record(t.Bucket, t.Target, err, func(st *TargetStatus) { st.Deletes++ })
		return err
	}
	return nil
}

// replicateVersion sends a version (or its tags) to the destinations it
// records as pending or failed (t.Target alone when set), and records the
// outcome on the version.
func (s *service) replicateVersion(ctx context.Context, t task) error {
	info, err := s.deps.Objects.GetObjectInfo(ctx, t.Bucket, t.Key, object.GetObjectOptions{VersionID: t.VersionID})
	if err != nil {
		if gone(err) {
			return nil
		}
		return err
	}
	if info.IsDeleteMarker || info.ReplicationStatus == domain.ReplicationReplica {
		return nil
	}
	var targets []string
	switch {
	case t.Target != "":
		targets = []string{t.Target}
	case t.Op == opTags:
		for arn, st := range info.ReplicationTargets {
			if st == domain.ReplicationCompleted {
				targets = append(targets, arn)
			}
		}
	default:
		for arn, st := range info.ReplicationTargets {
			if st == domain.ReplicationPending || st == domain.ReplicationFailed {
				targets = append(targets, arn)
			}
		}
	}
	var first error
	for _, arn := range targets {
		var err error
		if t.Op == opTags {
			err = s.sendTags(ctx, t.Bucket, info, arn)
		} else {
			err = s.sendVersion(ctx, t.Bucket, info, arn, t.Resync)
		}
		if errors.Is(err, errNoTarget) {
			// The target is gone: the version cannot get there.
			if t.Op == opObject {
				s.setState(ctx, t.Bucket, info, arn, domain.ReplicationFailed)
			}
			continue
		}
		if t.Op == opObject {
			state := domain.ReplicationCompleted
			if err != nil {
				state = domain.ReplicationFailed
			}
			s.setState(ctx, t.Bucket, info, arn, state)
		}
		if err != nil && first == nil {
			first = fmt.Errorf("replicate %s/%s (%s) to %s: %w", t.Bucket, t.Key, info.VersionID, arn, err)
		}
	}
	return first
}

// setState records the state of a version at a target, unless it already
// has it.
func (s *service) setState(ctx context.Context, bucket string, info domain.ObjectInfo, arn string, state domain.ReplicationStatus) {
	if info.ReplicationTargets[arn] == state {
		return
	}
	if _, err := s.deps.Objects.SetReplicationStatus(ctx, bucket, info.Key, info.VersionID, arn, state); err != nil && !gone(err) {
		s.deps.Logger.WarnContext(ctx, "replication: state not recorded", "bucket", bucket, "key", info.Key, "versionId", info.VersionID, "state", state, "err", err)
	}
}

// gone reports a version that no longer exists.
func gone(err error) bool {
	return errors.Is(err, domain.ErrNoSuchVersion) || errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrBucketNotFound)
}

// sendTags replaces the tags of the replica.
func (s *service) sendTags(ctx context.Context, bucket string, info domain.ObjectInfo, arn string) error {
	r, err := s.remote(ctx, bucket, arn)
	if err != nil {
		s.record(bucket, arn, err, nil)
		return err
	}
	err = r.PutTags(ctx, info.Key, info.VersionID, info.Tags)
	s.record(bucket, arn, err, nil)
	return err
}

// sendVersion writes the version to the bucket of a target, in one
// request or in parts. A resync first asks whether the destination has it.
func (s *service) sendVersion(ctx context.Context, bucket string, info domain.ObjectInfo, arn string, check bool) error {
	r, err := s.remote(ctx, bucket, arn)
	if err != nil {
		s.record(bucket, arn, err, nil)
		return err
	}
	if check {
		if v, err := r.Head(ctx, info.Key, info.VersionID); err == nil && v.Exists && v.Size == info.Size {
			return nil
		}
	}
	err = s.copyVersion(ctx, bucket, info, r)
	s.record(bucket, arn, err, func(st *TargetStatus) {
		st.Replicated++
		st.Bytes += info.Size
	})
	return err
}

// copyVersion streams the content of the version to the remote.
func (s *service) copyVersion(ctx context.Context, bucket string, info domain.ObjectInfo, r Remote) error {
	body, err := s.deps.Objects.GetObject(ctx, bucket, info.Key, object.GetObjectOptions{VersionID: info.VersionID})
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	put := remotePut(body.Info)
	size := body.Info.Size
	if size <= s.opts.singleLimit {
		data := make([]byte, size)
		if _, err := io.ReadFull(body, data); err != nil {
			return fmt.Errorf("read the version: %w", err)
		}
		return r.Put(ctx, put, data)
	}
	uploadID, err := r.CreateUpload(ctx, put)
	if err != nil {
		return err
	}
	var parts []RemotePart
	buf := make([]byte, s.opts.partSize)
	for n, left := 1, size; left > 0; n++ {
		chunk := buf[:min(int64(len(buf)), left)]
		if _, err := io.ReadFull(body, chunk); err != nil {
			s.abort(ctx, r, info.Key, uploadID)
			return fmt.Errorf("read the version: %w", err)
		}
		etag, err := r.UploadPart(ctx, info.Key, uploadID, n, chunk)
		if err != nil {
			s.abort(ctx, r, info.Key, uploadID)
			return err
		}
		parts = append(parts, RemotePart{Number: n, ETag: etag})
		left -= int64(len(chunk))
	}
	if err := r.CompleteUpload(ctx, info.Key, uploadID, parts); err != nil {
		s.abort(ctx, r, info.Key, uploadID)
		return err
	}
	return nil
}

func (s *service) abort(ctx context.Context, r Remote, key, uploadID string) {
	if err := r.AbortUpload(context.WithoutCancel(ctx), key, uploadID); err != nil {
		s.deps.Logger.DebugContext(ctx, "replication: upload not aborted", "key", key, "uploadId", uploadID, "err", err)
	}
}

// remotePut describes a version for the remote.
func remotePut(info domain.ObjectInfo) RemotePut {
	p := RemotePut{
		Key: info.Key, VersionID: info.VersionID, ModTime: info.ModTime, Size: info.Size,
		ContentType: info.ContentType, ContentEncoding: info.ContentEncoding, ContentDisposition: info.ContentDisposition,
		ContentLanguage: info.ContentLanguage, CacheControl: info.CacheControl, Expires: info.Expires,
		UserMetadata: info.UserMetadata, Tags: info.Tags, Retention: info.Retention, LegalHold: info.LegalHold,
		Encryption: info.Encryption,
	}
	if info.Encryption.Type == domain.SSENone {
		// The ETag of an unencrypted version is its content's: the
		// replica keeps it (that of an encrypted one is opaque).
		p.ETag = info.ETag
	}
	return p
}
