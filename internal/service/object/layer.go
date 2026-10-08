package object

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Limits shared with S3.
const (
	maxTags           = 10
	maxTagKeyLength   = 128
	maxTagValueLength = 256
	maxListKeys       = 1000
	maxPartNumber     = 10000
	// maxCopySize is the largest source of a single CopyObject (5 GiB);
	// bigger objects are copied part by part with UploadPartCopy.
	maxCopySize = 5 << 30
)

// layer holds the S3 logic shared by the filesystem and erasure
// implementations. Every object lives in exactly one store (erasure set),
// chosen by place; buckets exist in every store.
type layer struct {
	deps Deps
	opts options
	// sets are all the stores, pool by pool.
	sets  []store
	place placement
	name  string
}

func newLayer(deps Deps, opts []Option, sets []store, name string) *layer {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Locks == nil {
		deps.Locks = lock.NewLocal(lock.Deps{})
	}
	if deps.IDs == nil {
		deps.IDs = uuidV7{}
	}
	if deps.Heal == nil {
		deps.Heal = &noopHeal{}
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &layer{deps: deps, opts: o, sets: sets, place: fixedPlacement{st: sets[0]}, name: name}
}

func (l *layer) now() time.Time { return l.deps.Clock.Now().UTC() }

func (l *layer) newID() string { return l.deps.IDs.NewID() }

// uuidV7 is the default identifier generator: time-ordered UUID v7
// strings, so that version ids sort by creation time.
type uuidV7 struct{}

var _ topology.IDGenerator = uuidV7{}

// NewID implements topology.IDGenerator.
func (uuidV7) NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

// uploadIDLen is the length of the raw upload id: a UUID followed by the
// big-endian initiation time in Unix nanoseconds.
const uploadIDLen = 16 + 8

// newUploadID returns base64url(uuid || initiated): unique, URL-safe and
// carrying its initiation time, which orders the uploads of a key when
// listings resume from an upload-id-marker.
func (l *layer) newUploadID(initiated time.Time) string {
	u, err := uuid.Parse(l.newID())
	if err != nil {
		u = uuid.New()
	}
	var raw [uploadIDLen]byte
	copy(raw[:16], u[:])
	binary.BigEndian.PutUint64(raw[16:], uint64(initiated.UnixNano()))
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// uploadIDTime decodes the initiation time embedded in an upload id.
func uploadIDTime(id string) (time.Time, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(raw) != uploadIDLen {
		return time.Time{}, false
	}
	return time.Unix(0, int64(binary.BigEndian.Uint64(raw[16:]))).UTC(), true
}

// --- locking -----------------------------------------------------------------

// lock acquires the namespace lock of bucket/key (key "" locks the bucket).
func (l *layer) lock(ctx context.Context, bucket, key string, write bool) (lock.LockContext, error) {
	return l.lockPath(ctx, bucket, key, write)
}

// lockUpload acquires the lock of a multipart upload: part uploads share
// it, completion and abort hold it exclusively.
func (l *layer) lockUpload(ctx context.Context, bucket, uploadID string, write bool) (lock.LockContext, error) {
	return l.lockPath(ctx, domain.SystemBucket, uploadDir(bucket, uploadID), write)
}

func (l *layer) lockPath(ctx context.Context, volume, path string, write bool) (lock.LockContext, error) {
	ns := l.deps.Locks.NewNSLock(ctx, volume, path)
	var (
		lc  lock.LockContext
		err error
	)
	if write {
		lc, err = ns.GetLock(ctx, l.opts.lockTimeout)
	} else {
		lc, err = ns.GetRLock(ctx, l.opts.lockTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: lock %s: %w", l.name, lock.Resource(volume, path), err)
	}
	return lc, nil
}

// lockRequest is one namespace lock of a multi-lock acquisition.
type lockRequest struct {
	bucket, key string
	write       bool
}

// ancestorLocks returns the read locks of the ancestor prefixes of key
// ("a" and "a/b" for "a/b/c" and "a/b/c/").
func ancestorLocks(bucket, key string) []lockRequest {
	var out []lockRequest
	for p := strings.TrimSuffix(key, "/"); ; {
		i := strings.LastIndexByte(p, '/')
		if i <= 0 {
			return out
		}
		p = p[:i]
		out = append(out, lockRequest{bucket: bucket, key: p})
	}
}

// lockWrite acquires the namespace locks of a write that may create or
// remove bucket/key: the key for writing and its ancestor prefixes for
// reading. Whether a key can exist depends on its ancestors ("k/x" cannot
// be written under an object "k", nor "k" over the prefix of "k/x"), so
// the writers of "k" and "k/x" must be serialized here: on an erasure set
// the drives would otherwise see them in different orders and each could
// commit on part of the drives. Writers of sibling keys share the read
// locks and do not wait on each other.
func (l *layer) lockWrite(ctx context.Context, bucket, key string) (func(), error) {
	return l.lockAll(ctx, append(ancestorLocks(bucket, key), lockRequest{bucket: bucket, key: key, write: true}))
}

// lockCopy locks the source of a copy for reading and its destination as
// lockWrite does. A self-copy takes the write lock only.
func (l *layer) lockCopy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (func(), error) {
	reqs := append(ancestorLocks(dstBucket, dstKey), lockRequest{bucket: dstBucket, key: dstKey, write: true})
	if srcBucket != dstBucket || srcKey != dstKey {
		reqs = append(reqs, lockRequest{bucket: srcBucket, key: srcKey})
	}
	return l.lockAll(ctx, reqs)
}

// lockAll acquires several namespace locks in resource order, a resource
// requested twice being locked once (for writing when either request
// writes), so that concurrent multi-lock acquisitions (crossed copies
// a→b and b→a, a write of "k" with one of "k/x") cannot deadlock. The
// returned function releases what was acquired.
func (l *layer) lockAll(ctx context.Context, reqs []lockRequest) (func(), error) {
	slices.SortFunc(reqs, func(a, b lockRequest) int {
		if c := strings.Compare(lock.Resource(a.bucket, a.key), lock.Resource(b.bucket, b.key)); c != 0 {
			return c
		}
		switch {
		case a.write == b.write:
			return 0
		case a.write:
			return -1
		default:
			return 1
		}
	})
	reqs = slices.CompactFunc(reqs, func(a, b lockRequest) bool { return a.bucket == b.bucket && a.key == b.key })
	held := make([]lock.LockContext, 0, len(reqs))
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock(ctx)
		}
	}
	for _, r := range reqs {
		lc, err := l.lock(ctx, r.bucket, r.key, r.write)
		if err != nil {
			release()
			return nil, err
		}
		held = append(held, lc)
	}
	return release, nil
}

// --- bucket configuration -------------------------------------------------

func (l *layer) bucketConfig(ctx context.Context, bucket string) (BucketConfig, error) {
	if l.deps.Buckets == nil {
		return BucketConfig{}, nil
	}
	cfg, err := l.deps.Buckets.BucketConfig(ctx, bucket)
	if err != nil {
		return BucketConfig{}, fmt.Errorf("%s: bucket config %s: %w", l.name, bucket, err)
	}
	return cfg, nil
}

// statBucket fails with domain.ErrBucketNotFound when the bucket is missing
// from the store st.
func (l *layer) statBucket(ctx context.Context, st store, bucket string) error {
	if _, err := st.StatVol(ctx, bucket); err != nil {
		return fmt.Errorf("%s: bucket %s: %w", l.name, bucket, err)
	}
	return nil
}

// validObject validates a bucket name and an object key.
func validObject(bucket, key string) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	return domain.ValidateObjectKey(key)
}

// --- events ------------------------------------------------------------------------

// publish sends a notification; failures are logged, never returned.
func (l *layer) publish(ctx context.Context, name event.Name, bucket string, info domain.ObjectInfo) {
	if l.deps.Events == nil {
		return
	}
	now := l.now()
	e := event.NewEvent(name, l.opts.region, now)
	e.S3.Bucket = event.Bucket{Name: bucket, ARN: "arn:aws:s3:::" + bucket}
	e.S3.Object = event.Object{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
		VersionID:    info.VersionID,
		Sequencer:    strconv.FormatInt(now.UnixNano(), 16),
	}
	if err := l.deps.Events.Publish(ctx, e); err != nil {
		l.deps.Logger.DebugContext(ctx, "object: event not published", "event", name, "bucket", bucket, "key", info.Key, "err", err)
	}
}

// cleanupTmp removes a temporary directory of the system volume, logging
// failures (the startup sweep of tmp/ catches what is left). It runs even
// when ctx is canceled: a client that went away mid-upload must not leave
// its staged data behind.
func (l *layer) cleanupTmp(ctx context.Context, st store, dir string) {
	if dir == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := st.DeletePath(ctx, domain.SystemBucket, dir, true); err != nil && !isNotFound(err) {
		l.deps.Logger.DebugContext(ctx, "object: cannot remove temporary directory", "dir", dir, "err", err)
	}
}

// newTmpDir returns a fresh temporary directory of the system volume.
func (l *layer) newTmpDir() string { return storage.TmpDir + "/" + l.newID() }
