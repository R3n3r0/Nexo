package object

import (
	"context"
	"fmt"
	"strconv"

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
	info, err := l.GetObjectInfo(ctx, bucket, key, GetObjectOptions{VersionID: opts.VersionID})
	if err != nil {
		return nil, err
	}
	if info.Tags == nil {
		return map[string]string{}, nil
	}
	return info.Tags, nil
}

// updateMeta rewrites the metadata of one version in place under the
// object write lock (the content and the modification time are kept):
// mutate receives the version and a copy of its metadata to change, and
// fails to leave the version untouched. Tags and the Object Lock settings
// are changed this way.
func (l *layer) updateMeta(ctx context.Context, bucket, key, versionID string, mutate func(fi storage.FileInfo, meta map[string]string) error, name event.Name) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	ctx, unlock, err := l.lock(ctx, bucket, key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	st, err := l.place.forVersion(ctx, bucket, key, versionID)
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
	stamp := l.now().UnixNano()
	if prev := metaStamp(fi.Metadata); stamp <= prev {
		stamp = prev + 1
	}
	meta[metaUpdated] = strconv.FormatInt(stamp, 10)
	fi.Metadata = meta
	if err := st.UpdateMetadata(ctx, bucket, key, fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: update %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	l.publish(ctx, name, bucket, info)
	return info, nil
}
