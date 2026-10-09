package quota

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Option configures the decorator.
type Option func(*options)

type options struct {
	// refreshInterval is the minimum age of a base before a write near
	// the quota refreshes it with a walk of the bucket.
	refreshInterval time.Duration
	// refreshRatio is the share of the quota above which a write
	// confirms its estimate with a fresh base.
	refreshRatio float64
}

// WithRefreshInterval sets how old a base must be before a write near the
// quota walks the bucket again (default 1s): the bound of the walks the
// accepted writes of a bucket near its quota cost.
func WithRefreshInterval(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.refreshInterval = d
		}
	}
}

// WithRefreshRatio sets the share of the quota (0-1, default 0.8) above
// which a write confirms its estimate with a fresh base: the writes of the
// other nodes since the base are then counted.
func WithRefreshRatio(r float64) Option {
	return func(o *options) {
		if r >= 0 && r <= 1 {
			o.refreshRatio = r
		}
	}
}

// scannerGrace is how long before the end of a scanner cycle the writes
// of this node still count over the usage of the cycle.
const scannerGrace = 10 * time.Minute

// write is a write committed by this node.
type write struct {
	at   time.Time
	size uint64
}

// bucketState is the state of a bucket with a quota.
type bucketState struct {
	// walk is the usage measured by the last walk, at walkAt.
	walk   uint64
	walkAt time.Time
	// dirty reports a delete on this node since the walk: the next
	// refusal refreshes the base whatever its age.
	dirty bool
	// recent are the writes committed since the oldest base in use.
	recent []write
	// reserved counts the bytes of the writes in progress.
	reserved uint64
	// refreshing serializes the walks of the bucket.
	refreshing sync.Mutex
}

// service is the quota decorator.
type service struct {
	object.Service
	deps Deps
	opts options

	mu      sync.Mutex
	buckets map[string]*bucketState
}

var _ Service = (*service)(nil)

// New returns the object layer deps.Objects decorated with the hard quota
// enforcement.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil || deps.Buckets == nil {
		return nil, fmt.Errorf("%w: quota: the object layer and the bucket settings are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{refreshInterval: time.Second, refreshRatio: 0.8}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return &service{Service: deps.Objects, deps: deps, opts: o, buckets: map[string]*bucketState{}}, nil
}

// errExceeded is the refusal of a write.
func errExceeded(bucket string, usage, size, quota uint64) error {
	return domain.NewS3Error(domain.ErrQuotaExceeded, "XNexoQuotaExceeded",
		fmt.Sprintf("Bucket quota exceeded: the bucket uses %d of its %d bytes, the write adds %d.", usage, quota, size)).WithResource(bucket)
}

// errStreamExceeded is the abort of a write of unknown length.
func errStreamExceeded(bucket string, allowance uint64) error {
	return domain.NewS3Error(domain.ErrQuotaExceeded, "XNexoQuotaExceeded",
		fmt.Sprintf("Bucket quota exceeded: the upload is larger than the %d bytes left in the quota.", allowance)).WithResource(bucket)
}

// quotaOf returns the hard quota of a bucket (0: none).
func (s *service) quotaOf(ctx context.Context, bucket string) (uint64, error) {
	cfg, err := s.deps.Buckets.BucketConfig(ctx, bucket)
	if err != nil {
		return 0, fmt.Errorf("quota: settings of %s: %w", bucket, err)
	}
	return cfg.QuotaBytes, nil
}

func (s *service) state(bucket string) *bucketState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.buckets[bucket]
	if !ok {
		st = &bucketState{}
		s.buckets[bucket] = st
	}
	return st
}

// scannerBase returns the usage of the bucket of the last scanner cycle
// (ok false when there is none).
func (s *service) scannerBase(ctx context.Context, bucket string) (uint64, time.Time, bool) {
	if s.deps.Usage == nil {
		return 0, time.Time{}, false
	}
	u, err := s.deps.Usage.Usage(ctx)
	if err != nil || u.LastUpdate.IsZero() {
		return 0, time.Time{}, false
	}
	return u.Buckets[bucket].Size, u.LastUpdate, true
}

// estimate computes the usage of a bucket from the newest base; s.mu
// guards the state.
func (s *service) estimate(bucket string, st *bucketState, scanSize uint64, scanAt time.Time, scanOK bool) Usage {
	u := Usage{Bucket: bucket, Reserved: st.reserved}
	switch {
	case !st.walkAt.IsZero() && (!scanOK || !st.walkAt.Before(scanAt)):
		u.Base, u.BaseTime, u.Source = st.walk, st.walkAt, SourceWalk
	case scanOK:
		u.Base, u.BaseTime, u.Source = scanSize, scanAt, SourceScanner
	}
	if u.Source == "" {
		return u
	}
	// The writes measured by the base are dropped; the newer ones count.
	// A scanner cycle is dated from its end while it met the bucket
	// earlier: the writes of the grace before its end count again (an
	// over- rather than an underestimate, corrected by a walk near the
	// quota).
	cutoff := u.BaseTime
	if u.Source == SourceScanner {
		cutoff = cutoff.Add(-scannerGrace)
	}
	kept := st.recent[:0]
	for _, w := range st.recent {
		if !w.at.Before(cutoff) {
			kept = append(kept, w)
			u.Recent += w.size
		}
	}
	clear(st.recent[len(kept):])
	st.recent = kept
	return u
}

// current returns the usage estimate of a bucket, refreshing the base
// when there is none or when refresh asks for it.
func (s *service) current(ctx context.Context, bucket string, st *bucketState, refresh func(Usage) bool) (Usage, error) {
	scanSize, scanAt, scanOK := s.scannerBase(ctx, bucket)
	s.mu.Lock()
	u := s.estimate(bucket, st, scanSize, scanAt, scanOK)
	stale := u.Source == "" || refresh(u)
	s.mu.Unlock()
	if !stale {
		return u, nil
	}
	if err := s.refresh(ctx, bucket, st, u.BaseTime); err != nil {
		return Usage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estimate(bucket, st, scanSize, scanAt, scanOK), nil
}

// refresh walks the bucket (the whole deployment) unless another write
// refreshed it after seen.
func (s *service) refresh(ctx context.Context, bucket string, st *bucketState, seen time.Time) error {
	st.refreshing.Lock()
	defer st.refreshing.Unlock()
	s.mu.Lock()
	done := !st.walkAt.IsZero() && st.walkAt.After(seen) && !st.dirty
	s.mu.Unlock()
	if done {
		return nil
	}
	// The writes committed during the walk may or may not be in it: the
	// walk is dated from its start, so that they count again (an over-
	// rather than an underestimate).
	started := s.deps.Clock.Now()
	infos, err := s.DataUsage(ctx, bucket)
	if err != nil {
		return fmt.Errorf("quota: usage of %s: %w", bucket, err)
	}
	var size uint64
	for _, info := range infos {
		if info.Name == bucket {
			size = info.Size
		}
	}
	s.mu.Lock()
	st.walk, st.walkAt, st.dirty = size, started, false
	s.mu.Unlock()
	return nil
}

// stale reports whether a base is old enough to be refreshed.
func (s *service) stale(st *bucketState, u Usage) bool {
	return st.dirty || s.deps.Clock.Now().Sub(u.BaseTime) >= s.opts.refreshInterval
}

// reservation is the space a write holds in its bucket while it runs.
type reservation struct {
	s    *service
	st   *bucketState
	size uint64
	once sync.Once
}

// done releases the reservation and, when the write committed, records
// the bytes it added.
func (r *reservation) done(committed uint64, ok bool) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.s.mu.Lock()
		defer r.s.mu.Unlock()
		r.st.reserved -= min(r.st.reserved, r.size)
		if ok && committed > 0 {
			r.st.recent = append(r.st.recent, write{at: r.s.deps.Clock.Now(), size: committed})
		}
	})
}

// reserve checks that size more bytes fit in the quota of bucket and
// reserves them. A nil reservation means the bucket has no quota.
func (s *service) reserve(ctx context.Context, bucket string, size uint64) (*reservation, error) {
	quota, err := s.quotaOf(ctx, bucket)
	if err != nil || quota == 0 {
		return nil, err
	}
	st := s.state(bucket)
	start := s.deps.Clock.Now()
	threshold := uint64(float64(quota) * s.opts.refreshRatio)
	u, err := s.current(ctx, bucket, st, func(u Usage) bool {
		total := u.Base + u.Recent + u.Reserved + size
		switch {
		case total > quota:
			// A refusal is confirmed by a walk not older than the request:
			// the deletes of the other nodes count (the concurrent
			// refusals share the walk).
			return u.Source != SourceWalk || u.BaseTime.Before(start) || st.dirty
		case total > threshold:
			return s.stale(st, u)
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	total := u.Base + u.Recent + st.reserved
	if total+size > quota || total+size < total {
		return nil, errExceeded(bucket, total, size, quota)
	}
	st.reserved += size
	return &reservation{s: s, st: st, size: size}, nil
}

// allowance returns the bytes a write of unknown length may add to bucket
// (refreshing a stale base first), and false when the bucket has no
// quota.
func (s *service) allowance(ctx context.Context, bucket string) (uint64, bool, error) {
	quota, err := s.quotaOf(ctx, bucket)
	if err != nil || quota == 0 {
		return 0, false, err
	}
	st := s.state(bucket)
	u, err := s.current(ctx, bucket, st, func(u Usage) bool { return s.stale(st, u) })
	if err != nil {
		return 0, false, err
	}
	used := u.Base + u.Recent + u.Reserved
	if used >= quota {
		return 0, true, nil
	}
	return quota - used, true, nil
}

// limitedReader fails with the quota error as soon as more than max bytes
// are read: the write is aborted before anything is committed.
type limitedReader struct {
	r      io.Reader
	max    uint64
	n      uint64
	bucket string
}

// newLimitedReader bounds r (nil: an empty body) to max bytes.
func newLimitedReader(r io.Reader, max uint64, bucket string) *limitedReader {
	if r == nil {
		r = eofReader{}
	}
	return &limitedReader{r: r, max: max, bucket: bucket}
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += uint64(n)
	if l.n > l.max {
		return n, errStreamExceeded(l.bucket, l.max)
	}
	return n, err
}

// BucketUsage implements Service.
func (s *service) BucketUsage(ctx context.Context, bucket string) (Usage, error) {
	quota, err := s.quotaOf(ctx, bucket)
	if err != nil {
		return Usage{}, err
	}
	st := s.state(bucket)
	u, err := s.current(ctx, bucket, st, func(u Usage) bool { return s.stale(st, u) })
	if err != nil {
		return Usage{}, err
	}
	u.Quota = quota
	return u, nil
}

// PutObject implements object.Service.
func (s *service) PutObject(ctx context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
	if in.Size < 0 {
		max, limited, err := s.allowance(ctx, in.Bucket)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		if limited {
			// Nothing is reserved: the allowance bounds the stream.
			in.Reader = newLimitedReader(in.Reader, max, in.Bucket)
			res := &reservation{s: s, st: s.state(in.Bucket)}
			info, err := s.Service.PutObject(ctx, in)
			res.done(uint64(max0(info.Size)), err == nil)
			return info, err
		}
		return s.Service.PutObject(ctx, in)
	}
	res, err := s.reserve(ctx, in.Bucket, uint64(in.Size))
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	info, err := s.Service.PutObject(ctx, in)
	res.done(uint64(max0(info.Size)), err == nil)
	return info, err
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

func max0(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// sourceSize returns the size of the source of a copy (and of its range),
// or ok false when the source cannot be described: the layer then
// reports the failure itself.
func (s *service) sourceSize(ctx context.Context, bucket, key, versionID string, ckey domain.SSECustomerKey, rng *domain.Range) (uint64, bool) {
	info, err := s.GetObjectInfo(ctx, bucket, key, object.GetObjectOptions{VersionID: versionID, SSECustomerKey: ckey})
	if err != nil {
		return 0, false
	}
	size := info.Size
	if rng != nil {
		_, length, err := rng.Resolve(info.Size)
		if err != nil {
			return 0, false
		}
		size = length
	}
	return uint64(max0(size)), true
}

// CopyObject implements object.Service.
func (s *service) CopyObject(ctx context.Context, in object.CopyObjectInput) (domain.ObjectInfo, error) {
	size, ok := s.sourceSize(ctx, in.SrcBucket, in.SrcKey, in.SrcVersionID, in.SrcSSECustomerKey, nil)
	if !ok {
		return s.Service.CopyObject(ctx, in)
	}
	res, err := s.reserve(ctx, in.DstBucket, size)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	info, err := s.Service.CopyObject(ctx, in)
	res.done(uint64(max0(info.Size)), err == nil)
	return info, err
}

// PutObjectPart implements object.Service: a part that cannot fit in the
// quota is refused (the parts are counted when the upload completes).
func (s *service) PutObjectPart(ctx context.Context, in object.PutObjectPartInput) (domain.Part, error) {
	if in.Size < 0 {
		max, limited, err := s.allowance(ctx, in.Bucket)
		if err != nil {
			return domain.Part{}, err
		}
		if limited {
			in.Reader = newLimitedReader(in.Reader, max, in.Bucket)
		}
		return s.Service.PutObjectPart(ctx, in)
	}
	res, err := s.reserve(ctx, in.Bucket, uint64(in.Size))
	if err != nil {
		return domain.Part{}, err
	}
	p, err := s.Service.PutObjectPart(ctx, in)
	res.done(0, false)
	return p, err
}

// CopyObjectPart implements object.Service.
func (s *service) CopyObjectPart(ctx context.Context, in object.CopyObjectPartInput) (domain.Part, error) {
	size, ok := s.sourceSize(ctx, in.SrcBucket, in.SrcKey, in.SrcVersionID, in.SrcSSECustomerKey, in.SrcRange)
	if !ok {
		return s.Service.CopyObjectPart(ctx, in)
	}
	res, err := s.reserve(ctx, in.DstBucket, size)
	if err != nil {
		return domain.Part{}, err
	}
	p, err := s.Service.CopyObjectPart(ctx, in)
	res.done(0, false)
	return p, err
}

// uploadSize returns the size of the parts a completion selects.
func (s *service) uploadSize(ctx context.Context, in object.CompleteMultipartInput) (uint64, error) {
	selected := make(map[int]bool, len(in.Parts))
	for _, p := range in.Parts {
		selected[p.Number] = true
	}
	var size uint64
	marker := 0
	for {
		res, err := s.ListObjectParts(ctx, domain.ListPartsInput{Bucket: in.Bucket, Key: in.Key, UploadID: in.UploadID, PartNumberMarker: marker})
		if err != nil {
			return 0, err
		}
		for _, p := range res.Parts {
			if selected[p.Number] {
				size += uint64(max0(p.Size))
			}
		}
		if !res.IsTruncated || res.NextPartNumberMarker <= marker {
			return size, nil
		}
		marker = res.NextPartNumberMarker
	}
}

// CompleteMultipartUpload implements object.Service: the completed object
// must fit in the quota.
func (s *service) CompleteMultipartUpload(ctx context.Context, in object.CompleteMultipartInput) (domain.ObjectInfo, error) {
	quota, err := s.quotaOf(ctx, in.Bucket)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if quota == 0 {
		return s.Service.CompleteMultipartUpload(ctx, in)
	}
	size, err := s.uploadSize(ctx, in)
	if err != nil {
		// The layer reports the missing upload or bucket itself.
		return s.Service.CompleteMultipartUpload(ctx, in)
	}
	res, err := s.reserve(ctx, in.Bucket, size)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	info, err := s.Service.CompleteMultipartUpload(ctx, in)
	res.done(uint64(max0(info.Size)), err == nil)
	return info, err
}

// markDirty records a delete in a bucket with a quota: the next refusal
// walks the bucket again, so that the freed space is seen at once.
func (s *service) markDirty(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.buckets[bucket]; ok {
		st.dirty = true
	}
}

// DeleteObject implements object.Service.
func (s *service) DeleteObject(ctx context.Context, bucket, key string, opts object.DeleteObjectOptions) (object.DeletedObject, error) {
	out, err := s.Service.DeleteObject(ctx, bucket, key, opts)
	if err == nil {
		s.markDirty(bucket)
	}
	return out, err
}

// DeleteObjects implements object.Service.
func (s *service) DeleteObjects(ctx context.Context, in object.DeleteObjectsInput) (object.DeleteObjectsResult, error) {
	out, err := s.Service.DeleteObjects(ctx, in)
	if err == nil {
		s.markDirty(in.Bucket)
	}
	return out, err
}

// DeleteBucket implements object.Service: the state of the bucket goes
// with it.
func (s *service) DeleteBucket(ctx context.Context, bucket string, opts object.DeleteBucketOptions) error {
	err := s.Service.DeleteBucket(ctx, bucket, opts)
	if err == nil {
		s.mu.Lock()
		delete(s.buckets, bucket)
		s.mu.Unlock()
	}
	return err
}
