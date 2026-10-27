package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

var _ PoolMover = (*erasureServerPools)(nil)

// PoolUsage implements PoolMover.
func (p *erasureServerPools) PoolUsage(ctx context.Context) []PoolUsage {
	out := make([]PoolUsage, len(p.pools))
	_ = parallel(len(p.pools), func(i int) error {
		pl := p.pools[i]
		// A fresh reading: the cache of the placement is refreshed too.
		s := pl.capacity(ctx, p.now(), 0)
		out[i] = PoolUsage{Index: i, Total: s.total, Free: s.free, Draining: pl.draining.Load()}
		return nil
	})
	return out
}

// SetPoolDraining implements PoolMover.
func (p *erasureServerPools) SetPoolDraining(pool int, draining bool) {
	if pool >= 0 && pool < len(p.pools) {
		p.pools[pool].draining.Store(draining)
	}
}

func (p *erasureServerPools) checkPool(pool int) error {
	if pool < 0 || pool >= len(p.pools) {
		return fmt.Errorf("%w: no pool %d (the deployment has %d)", domain.ErrInvalidArgument, pool+1, len(p.pools))
	}
	return nil
}

// PoolKeys implements PoolMover.
func (p *erasureServerPools) PoolKeys(ctx context.Context, pool int, bucket, after string, max int) ([]string, error) {
	if err := p.checkPool(pool); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = 1000
	}
	stores := make([]store, 0, len(p.pools[pool].sets))
	for _, set := range p.pools[pool].sets {
		stores = append(stores, set)
	}
	var keys []string
	err := walkStores(ctx, stores, storage.WalkDirOptions{Bucket: bucket, Recursive: true, ForwardTo: after}, func(e walkEntry) error {
		if e.IsDir || e.Name <= after || len(e.Versions) == 0 {
			return nil
		}
		keys = append(keys, e.Name)
		if len(keys) >= max {
			return errListDone
		}
		return nil
	})
	if err != nil && !errors.Is(err, errListDone) {
		return nil, fmt.Errorf("%s: keys of pool %d in %s: %w", p.name, pool+1, bucket, err)
	}
	return keys, nil
}

// PoolUploads implements PoolMover: the upload documents of the sets of
// the pool.
func (p *erasureServerPools) PoolUploads(ctx context.Context, pool int) (int, error) {
	if err := p.checkPool(pool); err != nil {
		return 0, err
	}
	sets := p.pools[pool].sets
	counts := make([]int, len(sets))
	errs := parallel(len(sets), func(i int) error {
		infos, err := sets[i].StatGlob(ctx, storage.MultipartDir+"/*/*/"+uploadFile)
		if err != nil && !isNotFound(err) {
			return err
		}
		counts[i] = len(infos)
		return nil
	})
	total := 0
	for i, err := range errs {
		if err != nil {
			return 0, fmt.Errorf("%s: uploads of pool %d set %d: %w", p.name, pool+1, i+1, err)
		}
		total += counts[i]
	}
	return total, nil
}

// MoveObject implements PoolMover, in three steps, so that the readers
// and writers of the key only wait for renames:
//
//  1. under the read lock of the key, the versions the source set holds
//     are read (each by a read quorum);
//  2. without lock, the data of each version is copied as it is stored
//     (nothing is decrypted or decompressed) into a staging directory of
//     the target set, then read back from there and compared, through a
//     SHA-256, with the source;
//  3. under the write lock, a few versions at a time, each version still
//     the same in the source (time, data directory) is committed in the
//     target pool with its current metadata, read back, and only then
//     removed from the source.
//
// A version changed or deleted meanwhile is left where it is (the next
// pass moves it); a failure leaves the source as it was and the staged
// data is removed.
func (p *erasureServerPools) MoveObject(ctx context.Context, in MoveObjectInput) (MoveObjectResult, error) {
	res := MoveObjectResult{To: -1}
	if err := validObject(in.Bucket, in.Key); err != nil {
		return res, err
	}
	if err := p.checkPool(in.From); err != nil {
		return res, err
	}
	for _, t := range in.Targets {
		if err := p.checkPool(t); err != nil {
			return res, err
		}
	}
	if len(p.pools) < 2 {
		return res, fmt.Errorf("%w: a single pool has nowhere to move objects", domain.ErrInvalidArgument)
	}
	bucket, key := in.Bucket, in.Key
	src := p.pools[in.From].setFor(key)

	// 1. The versions, under the read lock.
	rctx, runlock, err := p.lock(ctx, bucket, key, false)
	if err != nil {
		return res, err
	}
	versions, err := src.storedVersions(rctx, bucket, key)
	runlock()
	if err != nil {
		return res, fmt.Errorf("%s: move %s/%s: %w", p.name, bucket, key, err)
	}
	if len(versions) == 0 {
		return res, nil
	}
	to, err := p.moveTarget(ctx, in, versions)
	if err != nil {
		return res, fmt.Errorf("%s: move %s/%s: %w", p.name, bucket, key, err)
	}
	dst := p.pools[to].setFor(key)

	// 2. The data, staged and verified without lock.
	staged := make([]stagedVersion, 0, len(versions))
	defer func() {
		for _, sv := range staged {
			p.cleanupTmp(ctx, dst, sv.tmpDir())
		}
	}()
	for _, fi := range versions {
		sv, err := p.stageVersion(ctx, src, dst, bucket, key, fi)
		if err != nil {
			return res, fmt.Errorf("%s: move %s/%s version %s to pool %d: %w", p.name, bucket, key, storedName(fi.VersionID), to+1, err)
		}
		staged = append(staged, sv)
	}

	// 3. Commit, verify and remove the source, under the write lock, a
	// few versions at a time so that the readers and writers of a key with
	// many versions get their turn between the batches. Oldest first: a
	// reader of the target pool sees the versions arrive in their order.
	slices.SortStableFunc(staged, func(a, b stagedVersion) int { return a.fi.ModTime.Compare(b.fi.ModTime) })
	for start := 0; start < len(staged); start += moveBatch {
		batch := staged[start:min(start+moveBatch, len(staged))]
		n, bytes, err := p.commitBatch(ctx, src, dst, bucket, key, batch)
		res.Versions += n
		res.Bytes += bytes
		if err != nil {
			return res, fmt.Errorf("%s: move %s/%s to pool %d: %w", p.name, bucket, key, to+1, err)
		}
	}
	if res.Versions > 0 {
		res.To = to
	}
	return res, nil
}

// moveBatch is the number of versions a move commits under one write
// lock of the key.
const moveBatch = 8

// commitBatch commits staged versions in dst and removes them from src,
// under the write lock of the key: each version still the same in src is
// committed with its current metadata, read back, and then removed from
// src. It returns the versions moved and their stored size.
func (p *erasureServerPools) commitBatch(ctx context.Context, src, dst *erasureSet, bucket, key string, batch []stagedVersion) (int, int64, error) {
	wctx, unlock, err := p.lockWrite(ctx, bucket, key)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()
	var (
		moved []storage.FileInfo
		bytes int64
	)
	for i := range batch {
		sv := &batch[i]
		if err := checkLocks(wctx); err != nil {
			return 0, 0, err
		}
		cur, err := src.ReadVersion(wctx, bucket, key, storedName(sv.fi.VersionID), true)
		if err != nil {
			if absent(err) {
				continue // deleted meanwhile: nothing to move
			}
			return 0, 0, err
		}
		if !sameVersion(cur, sv.fi) {
			continue // replaced meanwhile: the next pass moves it
		}
		n, err := p.commitStaged(wctx, dst, bucket, key, sv, cur)
		if err != nil {
			return 0, 0, fmt.Errorf("version %s: %w", storedName(cur.VersionID), err)
		}
		bytes += n
		moved = append(moved, cur)
	}
	// Every version of the batch is in the target pool, verified: the
	// source copies go.
	done := 0
	for _, fi := range moved {
		if err := checkLocks(wctx); err != nil {
			return done, bytes, err
		}
		err := src.DeleteVersion(wctx, bucket, key, storage.FileInfo{VersionID: fi.VersionID})
		if err != nil && !absent(err) {
			return done, bytes, fmt.Errorf("remove version %s from the source pool: %w", storedName(fi.VersionID), err)
		}
		done++
	}
	return done, bytes, nil
}

// stagedVersion is a version whose data waits, verified, in a staging
// directory of the target set (none for inline versions and delete
// markers).
type stagedVersion struct {
	fi        storage.FileInfo
	tmpID     string
	ei        storage.ErasureInfo
	committed bool
}

// tmpDir is the staging directory ("" when nothing is staged or the data
// was committed).
func (sv stagedVersion) tmpDir() string {
	if sv.tmpID == "" || sv.committed {
		return ""
	}
	return storage.TmpDir + "/" + sv.tmpID
}

// sameVersion reports whether cur is still the version snapshot was read
// as: same time, kind, size and data directory (the data of a version
// never changes in place; its metadata may, and the current one is used).
func sameVersion(cur, snapshot storage.FileInfo) bool {
	return cur.ModTime.Equal(snapshot.ModTime) && cur.Deleted == snapshot.Deleted &&
		cur.Size == snapshot.Size && cur.DataDir == snapshot.DataDir && len(cur.Parts) == len(snapshot.Parts) &&
		bytes.Equal(cur.Data, snapshot.Data)
}

// stageVersion copies the data of a version (when it has a data
// directory) into a staging directory of dst, laid out for dst, and reads
// it back to compare it with the source.
func (p *erasureServerPools) stageVersion(ctx context.Context, src, dst *erasureSet, bucket, key string, fi storage.FileInfo) (stagedVersion, error) {
	sv := stagedVersion{fi: fi}
	if fi.Deleted || fi.DataDir == "" {
		return sv, nil
	}
	sv.ei = dst.ErasureInfo(key, fi.Metadata[metaStorageClass])
	// One bitrot entry per part, as the commit records them.
	sv.ei.Checksums = make([]storage.ChecksumInfo, 0, len(fi.Parts))
	for _, part := range fi.Parts {
		sv.ei.Checksums = append(sv.ei.Checksums, storage.ChecksumInfo{PartNumber: part.Number, Algorithm: erasure.BitrotAlgorithm})
	}
	sv.tmpID = p.newID()
	sums := sha256.New()
	for _, part := range fi.Parts {
		if err := copyPart(ctx, src, dst, bucket, key, fi, part, sv.tmpDir(), sv.ei, sums); err != nil {
			return sv, err
		}
	}
	// Read the staged data back as the target set will serve it.
	staged := storage.FileInfo{DataDir: sv.tmpID, Erasure: sv.ei, Parts: fi.Parts}
	got := sha256.New()
	for _, part := range fi.Parts {
		rc, err := dst.ReadData(ctx, domain.SystemBucket, storage.TmpDir, staged, part, 0, part.Size)
		if err != nil {
			return sv, fmt.Errorf("read back part %d: %w", part.Number, err)
		}
		_, err = io.Copy(got, rc)
		_ = rc.Close()
		if err != nil {
			return sv, fmt.Errorf("read back part %d: %w", part.Number, err)
		}
	}
	if !bytes.Equal(got.Sum(nil), sums.Sum(nil)) {
		return sv, fmt.Errorf("%w: the copied data differs from the source version", domain.ErrInvalidFormat)
	}
	return sv, nil
}

// commitStaged makes the version cur (the current state of the staged
// version) visible in dst and checks it there. It returns the stored size
// committed. A null version already in dst and newer than cur (written
// there after it) is kept: cur is superseded and nothing is committed.
func (p *erasureServerPools) commitStaged(ctx context.Context, dst *erasureSet, bucket, key string, sv *stagedVersion, cur storage.FileInfo) (int64, error) {
	name := storedName(cur.VersionID)
	if cur.VersionID == "" {
		existing, err := dst.ReadVersion(ctx, bucket, key, nullVersion, false)
		switch {
		case err == nil && existing.ModTime.After(cur.ModTime):
			return 0, nil
		case err != nil && !absent(err):
			return 0, err
		}
	}
	if cur.Deleted {
		marker := storage.FileInfo{
			VersionID: cur.VersionID, ModTime: cur.ModTime, MarkDeleted: true, Versioned: cur.Versioned,
			Metadata: maps.Clone(cur.Metadata),
		}
		if err := dst.DeleteVersion(ctx, bucket, key, marker); err != nil {
			return 0, err
		}
		got, err := dst.ReadVersion(ctx, bucket, key, name, false)
		if err != nil {
			return 0, fmt.Errorf("read back the delete marker: %w", err)
		}
		if !got.Deleted || !got.ModTime.Equal(cur.ModTime) {
			return 0, fmt.Errorf("%w: the copied delete marker differs from the source", domain.ErrInvalidFormat)
		}
		return 0, nil
	}
	out := cur
	out.Metadata = maps.Clone(cur.Metadata)
	out.Parts = slices.Clone(cur.Parts)
	out.IsLatest, out.Idx, out.NumVersions = false, 0, 0
	if cur.DataDir == "" {
		// Inline: the data travels in the metadata.
		out.Erasure = dst.ErasureInfo(key, cur.Metadata[metaStorageClass])
		out.Data = bytes.Clone(cur.Data)
	} else {
		out.Erasure = sv.ei
		out.DataDir = uuid.NewString()
		out.Data = nil
	}
	tmpDir := sv.tmpDir()
	if err := dst.CommitData(ctx, tmpDir, out, bucket, key); err != nil {
		return 0, err
	}
	sv.committed = true
	got, err := dst.ReadVersion(ctx, bucket, key, name, true)
	if err != nil {
		return 0, fmt.Errorf("read back the copy: %w", err)
	}
	if got.Deleted || !got.ModTime.Equal(cur.ModTime) || got.Size != cur.Size || len(got.Parts) != len(cur.Parts) ||
		!maps.Equal(got.Metadata, cur.Metadata) || !bytes.Equal(got.Data, cur.Data) {
		return 0, fmt.Errorf("%w: the copy differs from the source version", domain.ErrInvalidFormat)
	}
	return cur.Size, nil
}

// storedName is the S3 name of a stored version id.
func storedName(id string) string {
	if id == "" {
		return nullVersion
	}
	return id
}

// storedVersions reads every version of key the set holds, each agreed by
// a read quorum and with its inline data. A version that no read quorum
// holds (a dangling leftover the healing removes) is left out; any other
// failure fails the read.
func (e *erasureSet) storedVersions(ctx context.Context, bucket, key string) ([]storage.FileInfo, error) {
	ids, err := e.versionIDs(ctx, bucket, key)
	if err != nil {
		if absent(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]storage.FileInfo, 0, len(ids))
	for _, id := range ids {
		fi, err := e.ReadVersion(ctx, bucket, key, id, true)
		if err != nil {
			if absent(err) {
				continue
			}
			return nil, err
		}
		out = append(out, fi)
	}
	return out, nil
}

// moveTarget chooses the pool of a key leaving in.From: the pool already
// holding versions of it (a key stays in one pool), preferring one that
// is not draining; otherwise a pool of in.Targets (or any pool that is
// not draining) picked by free space.
func (p *erasureServerPools) moveTarget(ctx context.Context, in MoveObjectInput, versions []storage.FileInfo) (int, error) {
	holder := -1
	for i, pl := range p.pools {
		if i == in.From {
			continue
		}
		ids, err := pl.setFor(in.Key).versionIDs(ctx, in.Bucket, in.Key)
		if err != nil && !absent(err) {
			return -1, fmt.Errorf("pool %d: %w", i+1, err)
		}
		if len(ids) == 0 {
			continue
		}
		if !pl.draining.Load() {
			return i, nil
		}
		if holder < 0 {
			holder = i
		}
	}
	var size int64
	for _, fi := range versions {
		size += fi.Size
	}
	allowed := func(i int) bool {
		if i == in.From || p.pools[i].draining.Load() {
			return false
		}
		return in.Targets == nil || slices.Contains(in.Targets, i)
	}
	if idx, err := p.place.(*poolPlacement).pickPoolAmong(ctx, size, allowed); err == nil {
		return idx, nil
	} else if holder < 0 {
		return -1, err
	}
	return holder, nil
}

// copyPart streams the stored bytes of one part from src into tmpDir of
// dst, laid out for dst, hashing them into sums.
func copyPart(ctx context.Context, src, dst *erasureSet, bucket, key string, fi storage.FileInfo, part storage.PartInfo, tmpDir string, ei storage.ErasureInfo, sums hash.Hash) error {
	rc, err := src.ReadData(ctx, bucket, key, fi, part, 0, part.Size)
	if err != nil {
		return fmt.Errorf("read part %d: %w", part.Number, err)
	}
	defer func() { _ = rc.Close() }()
	n, err := dst.WriteData(ctx, tmpDir, part.Number, part.Size, io.TeeReader(rc, sums), ei)
	if err != nil {
		return fmt.Errorf("write part %d: %w", part.Number, err)
	}
	if n != part.Size {
		return fmt.Errorf("%w: part %d: copied %d bytes of %d", domain.ErrInvalidFormat, part.Number, n, part.Size)
	}
	return nil
}
