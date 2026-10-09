package object

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// PutObjectTags implements Service. The tag set replaces the existing one;
// an empty set removes the tags.
func (l *layer) PutObjectTags(ctx context.Context, bucket, key string, tags map[string]string, opts TagOptions) (domain.ObjectInfo, error) {
	if err := validateTags(tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.updateMeta(ctx, bucket, key, opts.VersionID, func(_ storage.FileInfo, meta map[string]string) error {
		if len(tags) == 0 {
			delete(meta, metaTagging)
			return nil
		}
		meta[metaTagging] = encodeTags(tags)
		return nil
	}, event.ObjectCreatedPutTagging)
}

// DeleteObjectTags implements Service.
func (l *layer) DeleteObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (domain.ObjectInfo, error) {
	return l.updateMeta(ctx, bucket, key, opts.VersionID, func(_ storage.FileInfo, meta map[string]string) error {
		delete(meta, metaTagging)
		return nil
	}, event.ObjectCreatedDeleteTagging)
}

// GetObjectTags implements Service. An object without tags yields an
// empty, non-nil map.
func (l *layer) GetObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (map[string]string, error) {
	info, err := l.describe(ctx, bucket, key, opts.VersionID)
	if err != nil {
		return nil, err
	}
	if info.Tags == nil {
		return map[string]string{}, nil
	}
	return info.Tags, nil
}

// describe returns the description of a version for the subresources of
// an object (tags, retention, legal hold). Unlike GetObjectInfo it needs
// no SSE-C key: S3 serves them without one.
func (l *layer) describe(ctx context.Context, bucket, key, versionID string) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	ctx, unlock, err := l.lock(ctx, bucket, key, false)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	st, err := l.storeFor(ctx, bucket, key, versionID)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: describe %s/%s: %w", l.name, bucket, key, err)
	}
	fi, err := l.readVersion(ctx, st, bucket, key, versionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: describe %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	l.labelNull(ctx, bucket, &info)
	return info, nil
}

// updateMeta rewrites the metadata of one version in place under the
// object write lock (the content and the modification time are kept):
// mutate receives the version and a copy of its metadata to change, and
// fails to leave the version untouched. Tags and the Object Lock settings
// are changed this way; the event name is published after the change (""
// publishes none).
func (l *layer) updateMeta(ctx context.Context, bucket, key, versionID string, mutate func(fi storage.FileInfo, meta map[string]string) error, name event.Name) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	ctx, unlock, err := l.lock(ctx, bucket, key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	st, err := l.storeFor(ctx, bucket, key, versionID)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: update %s/%s: %w", l.name, bucket, key, err)
	}
	fi, err := l.readVersion(ctx, st, bucket, key, versionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: update %s/%s: %w", l.name, bucket, key, err)
	}
	meta := make(map[string]string, len(fi.Metadata)+1)
	for k, v := range fi.Metadata {
		meta[k] = v
	}
	if err := mutate(fi, meta); err != nil {
		return domain.ObjectInfo{}, err
	}
	// The stamp orders the metadata updates of the version and, as the
	// sequencer of the event, the events of the key: after the version
	// itself and after the latest version of the key, whatever the clock
	// of this node (commitTime).
	stamp := l.commitTime(ctx, bucket, key).UnixNano()
	if min := fi.ModTime.UnixNano() + 1; stamp < min {
		stamp = min
	}
	if prev := metaStamp(fi.Metadata); stamp <= prev {
		stamp = prev + 1
	}
	meta[metaUpdated] = strconv.FormatInt(stamp, 10)
	fi.Metadata = meta
	if err := st.UpdateMetadata(ctx, bucket, key, fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: update %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	l.labelNull(ctx, bucket, &info)
	l.publishAt(ctx, name, bucket, info, time.Unix(0, stamp))
	return info, nil
}
