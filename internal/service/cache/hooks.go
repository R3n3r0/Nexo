package cache

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// --- event hook --------------------------------------------------------------------

// events decorates the notification service of the object layer: every
// event of a change (published synchronously by the layer, before the
// write is acknowledged) invalidates the cache first.
type events struct {
	event.Service
	deps EventsDeps
}

var _ event.Service = (*events)(nil)

// NewEvents returns the event hook of the automatic invalidation: the
// object layer publishes its events to it.
func NewEvents(deps EventsDeps) (event.Service, error) {
	if deps.Events == nil || deps.Invalidator == nil {
		return nil, fmt.Errorf("%w: cache: the event hook needs the events and the invalidator", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &events{Service: deps.Events, deps: deps}, nil
}

// Publish implements event.Service.
func (e *events) Publish(ctx context.Context, ev event.Event) error {
	if change, ok := changeOf(ev); ok {
		if err := e.deps.Invalidator.ObjectChanged(ctx, change); err != nil {
			e.deps.Logger.WarnContext(ctx, "cache: invalidation incomplete", "bucket", change.Bucket, "key", change.Key, "err", err)
		}
	}
	return e.Service.Publish(ctx, ev)
}

// changeOf returns the change an event reports (none for the reads).
func changeOf(ev event.Event) (ObjectChange, bool) {
	name := ev.EventName
	switch {
	case name.Match(event.ObjectAccessedAll), name == event.TestEvent, name == "":
		return ObjectChange{}, false
	case name.Match(event.BucketCreated), name.Match(event.BucketRemoved):
		return ObjectChange{Bucket: ev.S3.Bucket.Name, Event: string(name), BucketWide: true}, ev.S3.Bucket.Name != ""
	}
	o := ev.S3.Object
	if ev.S3.Bucket.Name == "" || o.Key == "" {
		return ObjectChange{}, false
	}
	var tags []string
	for k, v := range o.UserMetadata {
		if strings.EqualFold(k, cacheTagKey) || strings.EqualFold(k, "x-amz-meta-"+cacheTagKey) {
			tags = append(tags, v)
		}
	}
	return ObjectChange{Bucket: ev.S3.Bucket.Name, Key: o.Key, VersionID: o.VersionID, Event: string(name), Tags: splitTags(tags...)}, true
}

// --- object layer ---------------------------------------------------------------------

// objects decorates the object layer of the distributions: the reads of a
// request begun with Runtime.Begin go through the cache, every other call
// to the object layer.
type objects struct {
	object.Service
	deps ObjectsDeps
}

var _ object.Service = (*objects)(nil)

// NewObjects returns the object layer decorated by the cache.
func NewObjects(deps ObjectsDeps) (object.Service, error) {
	if deps.Objects == nil || deps.Cache == nil {
		return nil, fmt.Errorf("%w: cache: the decorated object layer needs the objects and the cache", domain.ErrInvalidArgument)
	}
	return &objects{Service: deps.Objects, deps: deps}, nil
}

// GetObject implements object.Service.
func (o *objects) GetObject(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (*object.Reader, error) {
	if rd, handled, err := o.deps.Cache.GetObject(ctx, bucket, key, opts); handled {
		return rd, err
	}
	return o.Service.GetObject(ctx, bucket, key, opts)
}

// GetObjectInfo implements object.Service.
func (o *objects) GetObjectInfo(ctx context.Context, bucket, key string, opts object.GetObjectOptions) (domain.ObjectInfo, error) {
	if info, handled, err := o.deps.Cache.GetObjectInfo(ctx, bucket, key, opts); handled {
		return info, err
	}
	return o.Service.GetObjectInfo(ctx, bucket, key, opts)
}
