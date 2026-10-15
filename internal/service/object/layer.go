package object

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
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
	return time.Unix(0, int64(binary.BigEndian.Uint64(raw[16:]))).UTC(), true // #nosec G115 -- the bits of the int64 UnixNano written by newUploadID
}

// --- locking -----------------------------------------------------------------

// Every namespace lock helper returns, with the release function, a
// context bound to the locks: the caller's context, canceled (with a
// cause wrapping domain.ErrLockLost) as soon as one of the locks is lost.
// A distributed lock is lost when its lease cannot be renewed on a quorum
// of the lock servers (docs/03 §3.6): another node may then take it, so
// the operations run under the lock use the bound context and stop before
// their commit point instead of committing alongside another writer.

// lock acquires the namespace lock of bucket/key (key "" locks the bucket).
func (l *layer) lock(ctx context.Context, bucket, key string, write bool) (context.Context, func(), error) {
	return l.lockAll(ctx, []lockRequest{{bucket: bucket, key: key, write: write}})
}

// lockUpload acquires the lock of a multipart upload: part uploads share
// it, completion and abort hold it exclusively.
func (l *layer) lockUpload(ctx context.Context, bucket, uploadID string, write bool) (context.Context, func(), error) {
	return l.lockAll(ctx, []lockRequest{{bucket: domain.SystemBucket, key: uploadDir(bucket, uploadID), write: write}})
}

// acquire takes one namespace lock.
func (l *layer) acquire(ctx context.Context, volume, path string, write bool) (lock.LockContext, error) {
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
func (l *layer) lockWrite(ctx context.Context, bucket, key string) (context.Context, func(), error) {
	return l.lockAll(ctx, append(ancestorLocks(bucket, key), lockRequest{bucket: bucket, key: key, write: true}))
}

// lockCopy locks the source of a copy for reading and its destination as
// lockWrite does. A self-copy takes the write lock only.
func (l *layer) lockCopy(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string) (context.Context, func(), error) {
	reqs := append(ancestorLocks(dstBucket, dstKey), lockRequest{bucket: dstBucket, key: dstKey, write: true})
	if srcBucket != dstBucket || srcKey != dstKey {
		reqs = append(reqs, lockRequest{bucket: srcBucket, key: srcKey})
	}
	return l.lockAll(ctx, reqs)
}

// lockAll acquires several namespace locks in resource order, a resource
// requested twice being locked once (for writing when either request
// writes), so that concurrent multi-lock acquisitions (crossed copies
// a→b and b→a, a write of "k" with one of "k/x") cannot deadlock. It
// returns the context bound to the locks and the function that releases
// them.
func (l *layer) lockAll(ctx context.Context, reqs []lockRequest) (context.Context, func(), error) {
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
	locks := make([]lock.LockContext, 0, len(reqs))
	for _, r := range reqs {
		lc, err := l.acquire(ctx, r.bucket, r.key, r.write)
		if err != nil {
			for i := len(locks) - 1; i >= 0; i-- {
				locks[i].Unlock(ctx)
			}
			return nil, nil, err
		}
		locks = append(locks, lc)
	}
	h := newHeld(locks)
	return h.bind(ctx), func() { h.release(ctx) }, nil
}

// held is the set of namespace locks of one acquisition. lost is canceled
// when one of the locks is lost, with the cause of the loss, and when the
// locks are released (cause errReleased), which releases the contexts
// bound to it.
type held struct {
	locks    []lock.LockContext
	lost     context.Context
	cancel   context.CancelCauseFunc
	watchers []func() bool
}

// errReleased is the cause of the cancellation of a context bound to
// locks that were released (the operation is over).
var errReleased = errors.New("object: namespace locks released")

// heldKey is the context key of the lock sets a context is bound to.
type heldKey struct{}

func newHeld(locks []lock.LockContext) *held {
	h := &held{locks: locks}
	h.lost, h.cancel = context.WithCancelCause(context.Background())
	for _, lc := range locks {
		lctx := lc.Context()
		if lctx == nil {
			continue
		}
		h.watchers = append(h.watchers, context.AfterFunc(lctx, func() {
			if cause := context.Cause(lctx); errors.Is(cause, domain.ErrLockLost) {
				h.cancel(cause)
			}
		}))
	}
	return h
}

// bind returns parent, canceled with the cause of the loss when one of
// the locks is lost. The lock set is recorded in the context, so that
// detached can bind a context outliving parent to it.
func (h *held) bind(parent context.Context) context.Context {
	sets, _ := parent.Value(heldKey{}).([]*held)
	ctx, cancel := context.WithCancelCause(context.WithValue(parent, heldKey{}, append(slices.Clip(sets), h)))
	context.AfterFunc(h.lost, func() { cancel(context.Cause(h.lost)) })
	return ctx
}

// release releases the locks, in reverse acquisition order.
func (h *held) release(ctx context.Context) {
	h.cancel(errReleased)
	for _, stop := range h.watchers {
		stop()
	}
	for i := len(h.locks) - 1; i >= 0; i-- {
		h.locks[i].Unlock(ctx)
	}
}

// detached returns a context that survives the cancellation of ctx (a
// client that went away must not interrupt a commit or its rollback) but
// not the loss of the namespace locks ctx is bound to.
func detached(ctx context.Context) context.Context {
	out := context.WithoutCancel(ctx)
	sets, _ := ctx.Value(heldKey{}).([]*held)
	for _, h := range sets {
		out = h.bind(out)
	}
	return out
}

// lockLost reports whether ctx was canceled because a namespace lock it
// is bound to was lost.
func lockLost(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), domain.ErrLockLost)
}

// lockErr returns the cause of the loss instead of err when the failure of
// an operation is due to a namespace lock of ctx being lost.
func lockErr(ctx context.Context, err error) error {
	if lockLost(ctx) {
		return context.Cause(ctx)
	}
	return err
}

// checkLocks fails with the cause of the loss when a namespace lock ctx is
// bound to was lost: the operations call it right before their commit
// point.
func checkLocks(ctx context.Context) error {
	if lockLost(ctx) {
		return context.Cause(ctx)
	}
	return nil
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

// storeFor returns the store of version versionID of bucket/key (see
// placement.forVersion) once the version id of the request is checked: a
// malformed one is InvalidArgument, as in S3, not a missing version.
func (l *layer) storeFor(ctx context.Context, bucket, key, versionID string) (store, error) {
	if err := domain.ValidateVersionID(versionID); err != nil {
		return nil, err
	}
	return l.place.forVersion(ctx, bucket, key, versionID)
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

// publish sends a notification; failures are logged, never returned. The
// sequencer of the event, which orders the events of a key (S3), is the
// commit time of the version or delete marker it reports (info.ModTime,
// monotonic per key under the write lock even across nodes with skewed
// clocks), the time of now for the events of no version (buckets).
func (l *layer) publish(ctx context.Context, name event.Name, bucket string, info domain.ObjectInfo) {
	l.publishAt(ctx, name, bucket, info, info.ModTime)
}

// publishAt is publish with the time of the sequencer given: the commit
// time of a change that creates no version (a permanent removal, a tag
// update), decided under the write lock of the key.
func (l *layer) publishAt(ctx context.Context, name event.Name, bucket string, info domain.ObjectInfo, at time.Time) {
	if l.deps.Events == nil || name == "" {
		return
	}
	now := l.now()
	if at.IsZero() {
		at = now
	}
	e := event.NewEvent(name, l.opts.region, now)
	e.S3.Bucket = event.Bucket{Name: bucket, ARN: "arn:aws:s3:::" + bucket}
	e.S3.Object = event.Object{
		Key:          info.Key,
		Size:         info.Size,
		ETag:         info.ETag,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
		VersionID:    info.VersionID,
		Sequencer:    sequencer(at),
	}
	if err := l.deps.Events.Publish(ctx, e); err != nil {
		l.deps.Logger.DebugContext(ctx, "object: event not published", "event", name, "bucket", bucket, "key", info.Key, "err", err)
	}
}

// sequencer returns the S3 sequencer of an event at t: its Unix
// nanoseconds in hexadecimal.
func sequencer(t time.Time) string {
	return strconv.FormatInt(t.UnixNano(), 16)
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
