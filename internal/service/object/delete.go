package object

import (
	"context"
	"errors"
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// DeleteObject implements Service. Deletes are idempotent: removing a
// missing object or version succeeds. Without a version id, a versioned
// bucket gets a delete marker (a fresh version when versioning is enabled,
// the null version when it is suspended); an unversioned bucket loses the
// object.
func (l *layer) DeleteObject(ctx context.Context, bucket, key string, opts DeleteObjectOptions) (DeletedObject, error) {
	if err := validObject(bucket, key); err != nil {
		return DeletedObject{}, err
	}
	// A delete marker may create the key: the placement locks are needed.
	ctx, unlock, err := l.lockWrite(ctx, bucket, key)
	if err != nil {
		return DeletedObject{}, err
	}
	defer unlock()
	st, err := l.place.forVersion(ctx, bucket, key, opts.VersionID)
	if err != nil {
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	if err := l.statBucket(ctx, st, bucket); err != nil {
		return DeletedObject{}, err
	}
	if opts.VersionID != "" {
		return l.deleteVersion(ctx, st, bucket, key, opts.VersionID)
	}
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		return DeletedObject{}, err
	}
	modTime := opts.ModTime
	if modTime.IsZero() {
		modTime = l.commitTime(ctx, bucket, key)
	}
	switch cfg.Versioning {
	case domain.VersioningEnabled, domain.VersioningSuspended:
		marker := storage.FileInfo{ModTime: modTime, MarkDeleted: true, Versioned: true}
		reported := nullVersion
		if cfg.Versioning == domain.VersioningEnabled {
			marker.VersionID = l.newID()
			reported = marker.VersionID
		}
		if err := checkLocks(ctx); err != nil {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		if err := st.DeleteVersion(ctx, bucket, key, marker); err != nil {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, lockErr(ctx, err))
		}
		l.publish(ctx, event.ObjectRemovedDeleteMarkerCreated, bucket, domain.ObjectInfo{Bucket: bucket, Key: key, VersionID: marker.VersionID, IsDeleteMarker: true})
		return DeletedObject{Key: key, DeleteMarker: true, DeleteMarkerVersionID: reported}, nil
	default:
		cur, err := st.ReadVersion(ctx, bucket, key, "", false)
		if err != nil {
			if isNotFound(err) {
				return DeletedObject{Key: key}, nil
			}
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		if err := checkLocks(ctx); err != nil {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
		}
		if err := st.DeleteVersion(ctx, bucket, key, storage.FileInfo{VersionID: cur.VersionID}); err != nil && !errors.Is(err, domain.ErrNoSuchVersion) && !isNotFound(err) {
			return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, lockErr(ctx, err))
		}
		l.publish(ctx, event.ObjectRemovedDelete, bucket, objectInfo(bucket, key, cur))
		return DeletedObject{Key: key}, nil
	}
}

// deleteVersion permanently removes one version or delete marker
// ("null" is the null version); a missing version is not an error (S3
// deletes are idempotent).
func (l *layer) deleteVersion(ctx context.Context, st store, bucket, key, versionID string) (DeletedObject, error) {
	stored := versionID
	if versionID == nullVersion {
		stored = ""
	}
	gone := DeletedObject{Key: key, VersionID: versionID}
	cur, err := st.ReadVersion(ctx, bucket, key, versionID, false)
	if err != nil {
		if isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion) {
			return gone, nil
		}
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	if err := checkLocks(ctx); err != nil {
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, err)
	}
	if err := st.DeleteVersion(ctx, bucket, key, storage.FileInfo{VersionID: stored}); err != nil {
		if isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion) {
			return gone, nil
		}
		return DeletedObject{}, fmt.Errorf("%s: delete %s/%s: %w", l.name, bucket, key, lockErr(ctx, err))
	}
	del := DeletedObject{Key: key, VersionID: versionID, DeleteMarker: cur.Deleted}
	if cur.Deleted {
		del.DeleteMarkerVersionID = versionID
	}
	l.publish(ctx, event.ObjectRemovedDelete, bucket, objectInfo(bucket, key, cur))
	return del, nil
}

// DeleteObjects implements Service.
func (l *layer) DeleteObjects(ctx context.Context, in DeleteObjectsInput) (DeleteObjectsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return DeleteObjectsResult{}, err
	}
	if len(in.Objects) == 0 {
		return DeleteObjectsResult{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: no objects to delete")
	}
	if len(in.Objects) > maxListKeys {
		return DeleteObjectsResult{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: too many keys (max 1000)")
	}
	if _, err := l.sets[0].StatVol(ctx, in.Bucket); err != nil {
		return DeleteObjectsResult{}, fmt.Errorf("%s: delete objects in %s: %w", l.name, in.Bucket, err)
	}
	res := DeleteObjectsResult{}
	for _, o := range in.Objects {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		del, err := l.DeleteObject(ctx, in.Bucket, o.Key, DeleteObjectOptions{VersionID: o.VersionID, BypassGovernance: in.BypassGovernance})
		if err != nil {
			res.Errors = append(res.Errors, DeleteError{Key: o.Key, VersionID: o.VersionID, Err: err})
			continue
		}
		if !in.Quiet {
			res.Deleted = append(res.Deleted, del)
		}
	}
	return res, nil
}
