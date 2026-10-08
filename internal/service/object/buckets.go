package object

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// bucketDocFile is the object layer's record of a bucket, kept in the
// system volume next to the bucket service's configuration document:
// buckets/<bucket>/.bucket.json. A volume's modification time changes with
// its content, so the creation time is persisted here.
const bucketDocFile = ".bucket.json"

// bucketDoc is the content of bucketDocFile.
type bucketDoc struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Region  string    `json:"region,omitempty"`
}

func bucketDocPath(bucket string) string {
	return storage.BucketsDir + "/" + bucket + "/" + bucketDocFile
}

func bucketSysDir(bucket string) string { return storage.BucketsDir + "/" + bucket }

func validBucket(name string) error {
	if domain.IsReservedBucket(name) {
		return fmt.Errorf("%w: %q is reserved", domain.ErrInvalidBucketName, name)
	}
	return domain.ValidateBucketName(name)
}

// isNotFound reports a missing file, object or upload.
func isNotFound(err error) bool { return errors.Is(err, domain.ErrObjectNotFound) }

// MakeBucket implements Service.
func (l *layer) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	lc, err := l.lock(ctx, bucket, "", true)
	if err != nil {
		return err
	}
	defer lc.Unlock(ctx)

	errs := parallel(len(l.sets), func(i int) error { return l.sets[i].MakeVol(ctx, bucket) })
	created := make([]bool, len(l.sets))
	existed := false
	var failure error
	for i, err := range errs {
		switch {
		case err == nil:
			created[i] = true
		case errors.Is(err, domain.ErrBucketAlreadyExists):
			existed = true
			if !opts.ForceCreate && failure == nil {
				failure = fmt.Errorf("%s: make bucket %s: %w", l.name, bucket, err)
			}
		case failure == nil:
			failure = fmt.Errorf("%s: make bucket %s: %w", l.name, bucket, err)
		}
	}
	rollback := func() {
		// The compensation runs even when the client went away.
		ctx := context.WithoutCancel(ctx)
		for i, ok := range created {
			if !ok {
				continue
			}
			if err := l.sets[i].DeleteVol(ctx, bucket, false); err != nil {
				l.deps.Logger.WarnContext(ctx, "object: cannot roll back bucket creation", "bucket", bucket, "err", err)
			}
		}
	}
	if failure != nil {
		rollback()
		return failure
	}
	if existed {
		if _, ok := l.readBucketDoc(ctx, bucket); ok {
			return nil // forced re-creation keeps the original record
		}
	}
	region := opts.Region
	if region == "" {
		region = l.opts.region
	}
	data, err := json.Marshal(bucketDoc{Name: bucket, Created: l.now(), Region: region})
	if err != nil {
		rollback()
		return fmt.Errorf("%s: encode bucket record of %s: %w", l.name, bucket, err)
	}
	errs = parallel(len(l.sets), func(i int) error { return l.sets[i].WriteAll(ctx, bucketDocPath(bucket), data) })
	for _, err := range errs {
		if err != nil {
			rollback()
			return fmt.Errorf("%s: record bucket %s: %w", l.name, bucket, err)
		}
	}
	if !existed {
		l.publish(ctx, event.BucketCreated, bucket, domain.ObjectInfo{Bucket: bucket})
	}
	return nil
}

// readBucketDoc reads the bucket record; ok is false when it is missing
// or unreadable (buckets created before the record existed).
func (l *layer) readBucketDoc(ctx context.Context, bucket string) (bucketDoc, bool) {
	data, err := l.sets[0].ReadAll(ctx, bucketDocPath(bucket))
	if err != nil {
		return bucketDoc{}, false
	}
	var doc bucketDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		l.deps.Logger.WarnContext(ctx, "object: unreadable bucket record", "bucket", bucket, "err", err)
		return bucketDoc{}, false
	}
	return doc, true
}

// GetBucketInfo implements Service.
func (l *layer) GetBucketInfo(ctx context.Context, bucket string) (domain.BucketInfo, error) {
	if err := validBucket(bucket); err != nil {
		return domain.BucketInfo{}, err
	}
	vi, err := l.sets[0].StatVol(ctx, bucket)
	if err != nil {
		return domain.BucketInfo{}, fmt.Errorf("%s: stat bucket %s: %w", l.name, bucket, err)
	}
	return l.bucketInfo(ctx, vi), nil
}

func (l *layer) bucketInfo(ctx context.Context, vi storage.VolInfo) domain.BucketInfo {
	info := domain.BucketInfo{Bucket: domain.Bucket{Name: vi.Name, Created: vi.Created.UTC(), Region: l.opts.region}}
	if doc, ok := l.readBucketDoc(ctx, vi.Name); ok {
		if !doc.Created.IsZero() {
			info.Created = doc.Created.UTC()
		}
		if doc.Region != "" {
			info.Region = doc.Region
		}
	}
	if cfg, err := l.bucketConfig(ctx, vi.Name); err == nil {
		info.Versioning = cfg.Versioning
		info.ObjectLockEnabled = cfg.ObjectLock
		info.QuotaBytes = cfg.QuotaBytes
	}
	return info
}

// ListBuckets implements Service. The volumes of every store are merged:
// a bucket is listed when a majority of the stores that answered hold it
// (a creation rolled back on some sets only is not), and the listing
// fails only when no store answers.
func (l *layer) ListBuckets(ctx context.Context) ([]domain.BucketInfo, error) {
	lists := make([][]storage.VolInfo, len(l.sets))
	errs := parallel(len(l.sets), func(i int) error {
		vols, err := l.sets[i].ListVols(ctx)
		lists[i] = vols
		return err
	})
	answered := 0
	counts := map[string]int{}
	first := map[string]storage.VolInfo{}
	var failure error
	for i, err := range errs {
		if err != nil {
			if failure == nil {
				failure = err
			}
			continue
		}
		answered++
		for _, v := range lists[i] {
			if _, ok := first[v.Name]; !ok {
				first[v.Name] = v
			}
			counts[v.Name]++
		}
	}
	if answered == 0 {
		return nil, fmt.Errorf("%s: list buckets: %w", l.name, failure)
	}
	vols := make([]storage.VolInfo, 0, len(counts))
	for name, c := range counts {
		if 2*c > answered {
			vols = append(vols, first[name])
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })
	out := make([]domain.BucketInfo, 0, len(vols))
	for _, v := range vols {
		if validBucket(v.Name) != nil {
			continue
		}
		out = append(out, l.bucketInfo(ctx, v))
	}
	return out, nil
}

// DeleteBucket implements Service.
func (l *layer) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	lc, err := l.lock(ctx, bucket, "", true)
	if err != nil {
		return err
	}
	defer lc.Unlock(ctx)
	if _, err := l.sets[0].StatVol(ctx, bucket); err != nil {
		return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
	}
	if !opts.Force {
		if err := l.checkEmpty(ctx, bucket); err != nil {
			return err
		}
	}
	errs := parallel(len(l.sets), func(i int) error { return l.sets[i].DeleteVol(ctx, bucket, opts.Force) })
	for _, err := range errs {
		if err == nil || errors.Is(err, domain.ErrBucketNotFound) {
			continue
		}
		// Keep the bucket coherent: it is re-created on the sets where it
		// was removed, so that it exists everywhere or nowhere.
		ctx := context.WithoutCancel(ctx)
		for i, derr := range errs {
			if derr == nil {
				if uerr := l.sets[i].MakeVol(ctx, bucket); uerr != nil {
					l.deps.Logger.WarnContext(ctx, "object: cannot roll back bucket removal", "bucket", bucket, "err", uerr)
				}
			}
		}
		return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
	}
	// The bucket's system records (its record, its configuration and its
	// in-progress uploads) go with it.
	for _, st := range l.sets {
		for _, dir := range []string{bucketSysDir(bucket), uploadsDir(bucket)} {
			if err := st.DeletePath(ctx, domain.SystemBucket, dir, true); err != nil && !isNotFound(err) {
				l.deps.Logger.WarnContext(ctx, "object: cannot remove bucket records", "bucket", bucket, "dir", dir, "err", err)
			}
		}
	}
	l.publish(ctx, event.BucketRemoved, bucket, domain.ObjectInfo{Bucket: bucket})
	return nil
}

// checkEmpty fails with domain.ErrBucketNotEmpty when the bucket holds an
// object, a version, a delete marker or an in-progress multipart upload.
func (l *layer) checkEmpty(ctx context.Context, bucket string) error {
	empty := true
	err := l.walkAll(ctx, storage.WalkDirOptions{Bucket: bucket, Recursive: true}, func(e walkEntry) error {
		if e.IsDir {
			return nil
		}
		empty = false
		return errListDone
	})
	if err != nil && !errors.Is(err, errListDone) {
		return fmt.Errorf("%s: delete bucket %s: %w", l.name, bucket, err)
	}
	if !empty {
		return domain.NewS3Error(domain.ErrBucketNotEmpty, "BucketNotEmpty", "The bucket you tried to delete is not empty").WithResource(bucket)
	}
	uploads, err := l.uploadsOf(ctx, bucket)
	if err != nil {
		return err
	}
	if len(uploads) > 0 {
		return domain.NewS3Error(domain.ErrBucketNotEmpty, "BucketNotEmpty",
			fmt.Sprintf("The bucket you tried to delete has %d multipart upload(s) in progress", len(uploads))).WithResource(bucket)
	}
	return nil
}

// DataUsage implements Service.
func (l *layer) DataUsage(ctx context.Context, bucket string) ([]domain.BucketInfo, error) {
	var infos []domain.BucketInfo
	if bucket != "" {
		info, err := l.GetBucketInfo(ctx, bucket)
		if err != nil {
			return nil, err
		}
		infos = []domain.BucketInfo{info}
	} else {
		var err error
		if infos, err = l.ListBuckets(ctx); err != nil {
			return nil, err
		}
	}
	for i := range infos {
		if err := l.usage(ctx, &infos[i]); err != nil {
			return nil, err
		}
	}
	return infos, nil
}

// usage walks a bucket and fills in its usage counters.
func (l *layer) usage(ctx context.Context, info *domain.BucketInfo) error {
	err := l.walkAll(ctx, storage.WalkDirOptions{Bucket: info.Name, Recursive: true}, func(e walkEntry) error {
		if e.IsDir {
			return nil
		}
		for i, fi := range e.Versions {
			switch {
			case fi.Deleted:
				info.DeleteMarkers++
				continue
			case i == 0:
				info.Objects++
			default:
				info.Versions++
			}
			if fi.Size > 0 {
				info.Size += uint64(fi.Size)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%s: usage of %s: %w", l.name, info.Name, err)
	}
	return nil
}
