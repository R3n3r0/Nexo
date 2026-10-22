package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// The data of a version transitioned to a remote tier (S-STO-12) is one
// object of the tier: the stored bytes of its parts (encrypted and
// compressed as on the drives), one after the other. The version keeps its
// metadata, parts, sizes and checksums on the drives, without data
// directory, and names the tier and the object; reads fetch the ranges they
// need from the tier.
//
// A transition never loses data: the object is written to the tier and
// checked there, then the metadata of the version is updated under its
// write lock (still the same version) in two steps: the tier is recorded
// first, with the local data kept, then the data directory is dropped. A
// version that records its tier is read from the tier whatever the state
// of its local data; the drives that dropped the data and those that did
// not yet agree on it (versionKey), and the healing completes the second
// step on the latter.
const (
	// metaTier is the tier holding the data of a transitioned version,
	// metaTierObject the name of its object there. They are internal:
	// never exposed, never copied.
	metaTier       = "x-nexo-internal-tier"
	metaTierObject = "x-nexo-internal-tier-object"
)

// tiered reports a version whose data is in a remote tier.
func tiered(fi storage.FileInfo) bool {
	return !fi.Deleted && fi.Metadata[metaTierObject] != ""
}

// inlineData reports a version whose content is in its metadata (no data
// directory, not transitioned).
func inlineData(fi storage.FileInfo) bool {
	return fi.DataDir == "" && !tiered(fi)
}

// storedSize is the size of the stored bytes of a version (the size of its
// object in a tier).
func storedSize(fi storage.FileInfo) int64 {
	var n int64
	for _, p := range fi.Parts {
		n += p.Size
	}
	return n
}

// partStart is the offset of part number n in the stored bytes of fi.
func partStart(fi storage.FileInfo, n int) (int64, bool) {
	var off int64
	for _, p := range fi.Parts {
		if p.Number == n {
			return off, true
		}
		off += p.Size
	}
	return 0, false
}

// tierReader reads the data of the transitioned versions from their tier
// and the others from the drives.
type tierReader struct {
	store
	tiers TierStore
}

// ReadData implements store.
func (t tierReader) ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error) {
	if !tiered(fi) {
		return t.store.ReadData(ctx, volume, path, fi, part, offset, length)
	}
	start, ok := partStart(fi, part.Number)
	if !ok || offset < 0 || length < 0 || offset+length > part.Size {
		return nil, fmt.Errorf("%w: %s/%s: range [%d,+%d) of part %d", domain.ErrInvalidRange, volume, path, offset, length, part.Number)
	}
	return t.tiers.OpenTiered(ctx, fi.Metadata[metaTier], fi.Metadata[metaTierObject], start+offset, length)
}

// OpenData implements store. A stream of a tier is never pinned: the read
// lock is held until the body is closed, so that the version read is not
// replaced, and its object in the tier removed, under it.
func (t tierReader) OpenData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, bool, error) {
	if !tiered(fi) {
		return t.store.OpenData(ctx, volume, path, fi, part, offset, length)
	}
	rc, err := t.ReadData(ctx, volume, path, fi, part, offset, length)
	return rc, false, err
}

// readerFor returns the store reading the data of fi: the drives, or its
// tier.
func (l *layer) readerFor(st store, fi storage.FileInfo) (store, error) {
	if !tiered(fi) {
		return st, nil
	}
	if l.deps.Tiers == nil {
		return nil, fmt.Errorf("%w: the data of %s/%s is in the tier %s and no tier is configured", domain.ErrNotImplemented, fi.Volume, fi.Name, fi.Metadata[metaTier])
	}
	return tierReader{store: st, tiers: l.deps.Tiers}, nil
}

// TransitionObject implements Service.
func (l *layer) TransitionObject(ctx context.Context, in TransitionInput) (domain.ObjectInfo, error) {
	if err := validObject(in.Bucket, in.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if in.Tier == "" {
		return domain.ObjectInfo{}, fmt.Errorf("%w: a transition needs a tier", domain.ErrInvalidArgument)
	}
	if l.deps.Tiers == nil {
		return domain.ObjectInfo{}, fmt.Errorf("%w: no tier is configured", domain.ErrNotImplemented)
	}
	vid := in.VersionID
	if vid == domain.NullVersionID {
		vid = ""
	}
	// The data is copied under the read lock: the version cannot be
	// replaced meanwhile, readers go on.
	fi, name, err := l.copyToTier(ctx, in, vid)
	if err != nil || name == "" {
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		return l.commitTransition(ctx, in, vid, fi, "")
	}
	info, err := l.commitTransition(ctx, in, vid, fi, name)
	if err != nil && !errors.Is(err, errTransitionCommitted) {
		// The version did not record the object: nothing refers to it.
		if derr := l.deps.Tiers.DeleteTiered(context.WithoutCancel(ctx), in.Tier, name); derr != nil {
			l.deps.Logger.WarnContext(ctx, "object: cannot remove the object of a failed transition", "tier", in.Tier, "object", name, "err", derr)
		}
	}
	if errors.Is(err, errTransitionCommitted) {
		// The tier is recorded: the version is read from it, the local
		// data goes at the next transition or healing.
		l.deps.Logger.WarnContext(ctx, "object: transition recorded, local data not released yet", "bucket", in.Bucket, "key", in.Key, "err", err)
		return info, nil
	}
	return info, err
}

// errTransitionCommitted marks a transition whose tier is recorded but
// whose local data could not be released yet.
var errTransitionCommitted = errors.New("transition recorded")

// sameTransitionSource reports whether cur is still the version the data
// was copied from.
func sameTransitionSource(cur, src storage.FileInfo) bool {
	return cur.ModTime.Equal(src.ModTime) && !cur.Deleted && cur.Size == src.Size && cur.DataDir == src.DataDir &&
		cur.Metadata[metaETag] == src.Metadata[metaETag] && len(cur.Parts) == len(src.Parts)
}

// copyToTier copies the stored data of the version to the tier under its
// read lock. A version already recorded in the tier returns no name (the
// commit releases its local data if needed).
func (l *layer) copyToTier(ctx context.Context, in TransitionInput, vid string) (storage.FileInfo, string, error) {
	lctx, unlock, err := l.lock(ctx, in.Bucket, in.Key, false)
	if err != nil {
		return storage.FileInfo{}, "", err
	}
	defer unlock()
	st, err := l.storeFor(lctx, in.Bucket, in.Key, vid)
	if err != nil {
		return storage.FileInfo{}, "", fmt.Errorf("%s: transition %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	fi, err := l.readVersion(lctx, st, in.Bucket, in.Key, vid, false)
	if err != nil {
		return storage.FileInfo{}, "", fmt.Errorf("%s: transition %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	if !in.ModTime.IsZero() && !fi.ModTime.Equal(in.ModTime) {
		return storage.FileInfo{}, "", fmt.Errorf("%w: %s/%s changed since its evaluation", domain.ErrPreconditionFailed, in.Bucket, in.Key)
	}
	switch {
	case tiered(fi):
		if fi.Metadata[metaTier] != in.Tier {
			return storage.FileInfo{}, "", fmt.Errorf("%w: %s/%s is already in the tier %s", domain.ErrInvalidArgument, in.Bucket, in.Key, fi.Metadata[metaTier])
		}
		return fi, "", nil
	case inlineData(fi) || storedSize(fi) == 0:
		return storage.FileInfo{}, "", fmt.Errorf("%w: %s/%s is stored inline, it is not transitioned", domain.ErrInvalidArgument, in.Bucket, in.Key)
	}
	data := &storedData{ctx: lctx, st: st, bucket: in.Bucket, key: in.Key, fi: fi}
	defer func() { _ = data.Close() }()
	name, err := l.deps.Tiers.PutTiered(lctx, in.Tier, data, storedSize(fi), TierRef{Bucket: in.Bucket, Key: in.Key, VersionID: fi.VersionID})
	if err != nil {
		return storage.FileInfo{}, "", fmt.Errorf("%s: transition %s/%s to %s: %w", l.name, in.Bucket, in.Key, in.Tier, lockErr(lctx, err))
	}
	if err := checkLocks(lctx); err != nil {
		if derr := l.deps.Tiers.DeleteTiered(context.WithoutCancel(ctx), in.Tier, name); derr != nil {
			l.deps.Logger.WarnContext(ctx, "object: cannot remove the object of a failed transition", "tier", in.Tier, "object", name, "err", derr)
		}
		return storage.FileInfo{}, "", err
	}
	return fi, name, nil
}

// commitTransition records the tier object name (empty: already recorded)
// in the version under its write lock, then drops its local data.
func (l *layer) commitTransition(ctx context.Context, in TransitionInput, vid string, src storage.FileInfo, name string) (domain.ObjectInfo, error) {
	ctx, unlock, err := l.lock(ctx, in.Bucket, in.Key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	st, err := l.storeFor(ctx, in.Bucket, in.Key, vid)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: transition %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	cur, err := l.readVersion(ctx, st, in.Bucket, in.Key, vid, false)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: transition %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	if name != "" {
		if !sameTransitionSource(cur, src) || tiered(cur) {
			return domain.ObjectInfo{}, fmt.Errorf("%w: %s/%s changed during its transition", domain.ErrPreconditionFailed, in.Bucket, in.Key)
		}
		meta := maps.Clone(cur.Metadata)
		meta[metaTier] = in.Tier
		meta[metaTierObject] = name
		stamp := l.commitTime(ctx, in.Bucket, in.Key).UnixNano()
		if min := cur.ModTime.UnixNano() + 1; stamp < min {
			stamp = min
		}
		if prev := metaStamp(cur.Metadata); stamp <= prev {
			stamp = prev + 1
		}
		meta[metaUpdated] = strconv.FormatInt(stamp, 10)
		cur.Metadata = meta
		cur.Data = nil
		if err := st.UpdateMetadata(ctx, in.Bucket, in.Key, cur); err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%s: transition %s/%s: %w", l.name, in.Bucket, in.Key, err)
		}
	} else if !tiered(cur) || cur.Metadata[metaTier] != in.Tier {
		return domain.ObjectInfo{}, fmt.Errorf("%w: %s/%s changed during its transition", domain.ErrPreconditionFailed, in.Bucket, in.Key)
	}
	info := objectInfo(in.Bucket, in.Key, cur)
	l.labelNull(ctx, in.Bucket, &info)
	if cur.DataDir == "" {
		return info, nil
	}
	if err := checkLocks(ctx); err != nil {
		return info, fmt.Errorf("%w: %w", errTransitionCommitted, err)
	}
	released := cur
	released.DataDir = ""
	released.Data = nil
	if err := st.WriteMetadata(ctx, in.Bucket, in.Key, released); err != nil {
		return info, fmt.Errorf("%w: %s/%s: %w", errTransitionCommitted, in.Bucket, in.Key, err)
	}
	return info, nil
}

// storedData streams the stored bytes of the parts of a version, one part
// at a time.
type storedData struct {
	ctx         context.Context
	st          store
	bucket, key string
	fi          storage.FileInfo
	next        int
	cur         io.ReadCloser
}

func (r *storedData) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if r.next >= len(r.fi.Parts) {
				return 0, io.EOF
			}
			part := r.fi.Parts[r.next]
			r.next++
			if part.Size == 0 {
				continue
			}
			rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, part, 0, part.Size)
			if err != nil {
				return 0, fmt.Errorf("object: read part %d of %s/%s: %w", part.Number, r.bucket, r.key, lockErr(r.ctx, err))
			}
			r.cur = rc
		}
		n, err := r.cur.Read(p)
		if errors.Is(err, io.EOF) {
			_ = r.cur.Close()
			r.cur = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, lockErr(r.ctx, err))
		}
		return n, nil
	}
}

// Close releases the part being read.
func (r *storedData) Close() error {
	if r.cur != nil {
		err := r.cur.Close()
		r.cur = nil
		return err
	}
	return nil
}

// TierLocation implements Service.
func (l *layer) TierLocation(ctx context.Context, bucket, key, versionID string) (TierLocation, error) {
	if err := validObject(bucket, key); err != nil {
		return TierLocation{}, err
	}
	if versionID == domain.NullVersionID {
		versionID = ""
	}
	ctx, unlock, err := l.lock(ctx, bucket, key, false)
	if err != nil {
		return TierLocation{}, err
	}
	defer unlock()
	st, err := l.storeFor(ctx, bucket, key, versionID)
	if err != nil {
		return TierLocation{}, err
	}
	fi, err := st.ReadVersion(ctx, bucket, key, versionID, false)
	if err != nil {
		if isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion) {
			if serr := l.statBucket(ctx, st, bucket); serr != nil && !errors.Is(serr, domain.ErrBucketNotFound) {
				return TierLocation{}, serr
			}
			return TierLocation{}, nil
		}
		return TierLocation{}, err
	}
	if fi.Deleted {
		return TierLocation{Exists: true}, nil
	}
	return TierLocation{Exists: true, Tier: fi.Metadata[metaTier], Object: fi.Metadata[metaTierObject]}, nil
}
