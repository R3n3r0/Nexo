package object

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// PutObjectTags implements Service. The tag set replaces the existing one;
// an empty set removes the tags.
func (l *layer) PutObjectTags(ctx context.Context, bucket, key string, tags map[string]string, opts TagOptions) (domain.ObjectInfo, error) {
	if err := validateTags(tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	return l.updateTags(ctx, bucket, key, opts.VersionID, func(meta map[string]string) {
		if len(tags) == 0 {
			delete(meta, metaTagging)
			return
		}
		meta[metaTagging] = encodeTags(tags)
	}, event.ObjectCreatedPutTagging)
}

// DeleteObjectTags implements Service.
func (l *layer) DeleteObjectTags(ctx context.Context, bucket, key string, opts TagOptions) (domain.ObjectInfo, error) {
	return l.updateTags(ctx, bucket, key, opts.VersionID, func(meta map[string]string) { delete(meta, metaTagging) }, event.ObjectCreatedDeleteTagging)
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

// updateTags rewrites the metadata of one version in place under the
// object write lock (the content and the modification time are kept).
func (l *layer) updateTags(ctx context.Context, bucket, key, versionID string, mutate func(map[string]string), name event.Name) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	lc, err := l.lock(ctx, bucket, key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer lc.Unlock(ctx)
	st := l.pick(key)
	fi, err := l.readVersion(ctx, st, bucket, key, versionID, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: tag %s/%s: %w", l.name, bucket, key, err)
	}
	meta := make(map[string]string, len(fi.Metadata)+1)
	for k, v := range fi.Metadata {
		meta[k] = v
	}
	mutate(meta)
	fi.Metadata = meta
	if err := st.UpdateMetadata(ctx, bucket, key, fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: tag %s/%s: %w", l.name, bucket, key, err)
	}
	info := objectInfo(bucket, key, fi)
	l.publish(ctx, name, bucket, info)
	return info, nil
}
