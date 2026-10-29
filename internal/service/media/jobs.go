package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// JobsDeps are the dependencies of the media jobs.
type JobsDeps struct {
	// Events is the decorated event service (required): it receives every
	// event and the jobs.
	Events event.Service
	// Buckets provides the media rules of the buckets (required): the
	// metadata store, since the bucket service is built on the object
	// layer these events come from.
	Buckets bucket.MetadataStore
	Clock   clock.Clock
	Logger  *slog.Logger
}

type jobs struct {
	event.Service
	deps JobsDeps
}

var _ Jobs = (*jobs)(nil)

// NewJobs returns the media jobs decorating deps.Events.
func NewJobs(deps JobsDeps) (Jobs, error) {
	if deps.Events == nil || deps.Buckets == nil {
		return nil, fmt.Errorf("%w: the media jobs require the events and the bucket metadata", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &jobs{Service: deps.Events, deps: deps}, nil
}

// Publish implements event.Service: the event first, then its job.
func (j *jobs) Publish(ctx context.Context, e event.Event) error {
	err := j.Service.Publish(ctx, e)
	if job := j.jobOf(ctx, e); job != nil {
		if jerr := j.publishJob(ctx, job); jerr != nil {
			j.deps.Logger.WarnContext(ctx, "media: job not queued", "bucket", job.Bucket, "key", job.Key, "action", job.Action, "err", jerr)
		}
	}
	return err
}

// jobOf returns the job an event causes, nil when none.
func (j *jobs) jobOf(ctx context.Context, e event.Event) *domain.MediaJob {
	name := e.EventName
	var action domain.MediaJobAction
	switch {
	case name.Match(event.ObjectCreatedAll):
		action = domain.MediaJobProcess
	case name == event.ObjectRemovedDelete, name == event.LifecycleExpirationDelete:
		// A version is gone for good (a delete marker leaves it).
		action = domain.MediaJobDelete
	default:
		return nil
	}
	o := e.S3.Object
	bkt := e.S3.Bucket.Name
	if bkt == "" || o.Key == "" || domain.IsMediaSidecar(o.Key) {
		return nil
	}
	rule, forensic, ok := j.rule(ctx, bkt, o.Key)
	if !ok {
		return nil
	}
	job := &domain.MediaJob{Action: action, Bucket: bkt, Key: o.Key, VersionID: o.VersionID,
		Sidecar: domain.MediaSidecarDir(o.Key, o.VersionID), Time: j.deps.Clock.Now().UTC()}
	if action == domain.MediaJobProcess {
		job.ETag, job.Size, job.Rule = o.ETag, o.Size, &rule
		job.Forensic = forensic
	}
	return job
}

// rule returns the media rule of the bucket selecting key, and whether
// the protection of the bucket marks the streams of that video with the
// forensic watermark.
func (j *jobs) rule(ctx context.Context, bkt, key string) (rule domain.MediaRule, forensic bool, ok bool) {
	m, err := j.deps.Buckets.Load(ctx, bkt)
	if err != nil {
		if !errors.Is(err, domain.ErrBucketNotFound) {
			j.deps.Logger.WarnContext(ctx, "media: rules unavailable", "bucket", bkt, "err", err)
		}
		return domain.MediaRule{}, false, false
	}
	if m.Media == nil {
		return domain.MediaRule{}, false, false
	}
	rule, ok = m.Media.Match(key)
	p := m.MediaProtection
	return rule, ok && p != nil && p.Forensic && p.Protects(key), ok
}

// publishJob publishes the event carrying a job.
func (j *jobs) publishJob(ctx context.Context, job *domain.MediaJob) error {
	e := event.NewEvent(event.MediaJob, "", job.Time)
	e.S3.Bucket.Name = job.Bucket
	e.S3.Object = event.Object{Key: job.Key, VersionID: job.VersionID, ETag: job.ETag, Size: job.Size}
	e.Media = job
	return j.Service.Publish(ctx, e)
}

// Reprocess implements Jobs.
func (j *jobs) Reprocess(ctx context.Context, bkt, key, versionID string) error {
	rule, forensic, ok := j.rule(ctx, bkt, key)
	if !ok {
		return fmt.Errorf("%w: no media rule of bucket %s selects %s", domain.ErrInvalidArgument, bkt, key)
	}
	// Without a version the worker processes the current one and names
	// its sidecar directory after it (domain.MediaJob).
	job := &domain.MediaJob{Action: domain.MediaJobProcess, Bucket: bkt, Key: key, VersionID: versionID, Rule: &rule, Forensic: forensic, Time: j.deps.Clock.Now().UTC()}
	if versionID != "" {
		job.Sidecar = domain.MediaSidecarDir(key, versionID)
	}
	return j.publishJob(ctx, job)
}
