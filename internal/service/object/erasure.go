package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// erasureSet is the store of one erasure set (ErasureObjects): every
// object of the set is spread over its N drives as D data and P parity
// shards (P from the storage class of the object), protected by per-block
// bitrot hashes. Reads need D consistent drives, writes D (D+1 when D ==
// P); a write that misses its quorum is rolled back, one that reaches it
// without every drive is reported to the heal notifier (MRF), and so is a
// read that met missing, outdated or corrupt data (heal-on-read).
type erasureSet struct {
	poolIdx, setIdx int
	drives          []storage.StorageAPI
	codecs          erasure.Factory
	classes         storageClasses
	blockSize       int64
	heal            HealNotifier
	logger          *slog.Logger
}

var _ store = (*erasureSet)(nil)

func (e *erasureSet) n() int { return len(e.drives) }

// quorums returns the read and write quorums of a D+P layout.
func quorums(data, parity int) (read, write int) {
	return topology.SetQuorums(data+parity, parity)
}

// readQuorum and writeQuorum are the quorums of the set's default
// (STANDARD) layout, used for volumes, system files and delete markers.
func (e *erasureSet) readQuorum() int {
	r, _ := quorums(e.n()-e.classes.standard, e.classes.standard)
	return r
}

func (e *erasureSet) writeQuorum() int {
	_, w := quorums(e.n()-e.classes.standard, e.classes.standard)
	return w
}

// writeQuorumOf returns the write quorum of a version: that of its layout,
// the default one for versions without a layout (delete markers).
func (e *erasureSet) writeQuorumOf(ei storage.ErasureInfo) int {
	if ei.DataBlocks <= 0 || ei.DataBlocks+ei.ParityBlocks != e.n() {
		return e.writeQuorum()
	}
	_, w := quorums(ei.DataBlocks, ei.ParityBlocks)
	return w
}

func (e *erasureSet) Online() (int, int) {
	online := 0
	for _, d := range e.drives {
		if isOnline(d) {
			online++
		}
	}
	return online, e.n()
}

func isOnline(d storage.StorageAPI) bool { return d != nil && d.IsOnline() }

func (e *erasureSet) offline(i int) error {
	return fmt.Errorf("%w: pool %d set %d drive %d", domain.ErrDiskNotFound, e.poolIdx+1, e.setIdx+1, i+1)
}

// onDrives runs fn on every online drive concurrently and returns the
// per-drive errors (ErrDiskNotFound for offline drives).
func (e *erasureSet) onDrives(fn func(i int, d storage.StorageAPI) error) []error {
	return parallel(e.n(), func(i int) error {
		d := e.drives[i]
		if !isOnline(d) {
			return e.offline(i)
		}
		return fn(i, d)
	})
}

// notify reports bucket/key (version versionID as a request names it: the
// empty id is the latest version) to the heal notifier. It never blocks on
// the request context.
func (e *erasureSet) notify(ctx context.Context, bucket, key, versionID string) {
	if bucket == domain.SystemBucket {
		return
	}
	e.heal.NotifyHeal(context.WithoutCancel(ctx), bucket, key, versionID)
}

// notifyStored reports a version by its stored id: the empty id is the
// null version, reported as "null", which need not be the latest one.
func (e *erasureSet) notifyStored(ctx context.Context, bucket, key, storedID string) {
	if storedID == "" {
		storedID = nullVersion
	}
	e.notify(ctx, bucket, key, storedID)
}

// partial reports whether some drive, online or offline, missed an
// operation that reached its quorum: the object needs healing (MRF). The
// healing service keeps the entry of a drive still offline queued, with
// backoff, until the drive is back and repaired.
func partial(errs []error) bool {
	for _, err := range errs {
		if err != nil {
			return true
		}
	}
	return false
}

// --- volumes -------------------------------------------------------------------

// MakeVol creates the volume on every drive; when the write quorum is
// missed, the drives where it was created are rolled back.
func (e *erasureSet) MakeVol(ctx context.Context, volume string) error {
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error { return d.MakeVol(ctx, volume) })
	err := reduceErrs(errs, e.writeQuorum(), domain.ErrWriteQuorum)
	if err != nil && !errors.Is(err, domain.ErrBucketAlreadyExists) {
		ctx := context.WithoutCancel(ctx)
		for i, derr := range errs {
			if derr == nil {
				if uerr := e.drives[i].DeleteVol(ctx, volume, false); uerr != nil {
					e.logger.WarnContext(ctx, "object: cannot roll back volume creation", "volume", volume, "drive", i+1, "err", uerr)
				}
			}
		}
	}
	return err
}

// ListVols returns the volumes present on a read quorum of drives.
func (e *erasureSet) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	lists := make([][]storage.VolInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		vols, err := d.ListVols(ctx)
		lists[i] = vols
		return err
	})
	if err := reduceErrs(errs, e.readQuorum(), domain.ErrReadQuorum); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	first := map[string]storage.VolInfo{}
	for _, vols := range lists {
		for _, v := range vols {
			counts[v.Name]++
			if f, ok := first[v.Name]; !ok || (v.Created.Before(f.Created) && !v.Created.IsZero()) {
				first[v.Name] = v
			}
		}
	}
	out := make([]storage.VolInfo, 0, len(counts))
	for name, c := range counts {
		if c >= e.readQuorum() {
			out = append(out, first[name])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e *erasureSet) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	infos := make([]storage.VolInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		vi, err := d.StatVol(ctx, volume)
		infos[i] = vi
		return err
	})
	if err := reduceErrs(errs, e.readQuorum(), domain.ErrReadQuorum); err != nil {
		return storage.VolInfo{}, err
	}
	for i, err := range errs {
		if err == nil {
			return infos[i], nil
		}
	}
	return storage.VolInfo{}, domain.ErrReadQuorum
}

// DeleteVol removes the volume from every drive; when the write quorum is
// missed, it is re-created where it was removed.
func (e *erasureSet) DeleteVol(ctx context.Context, volume string, force bool) error {
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error { return d.DeleteVol(ctx, volume, force) })
	err := reduceErrs(errs, e.writeQuorum(), domain.ErrWriteQuorum)
	if err != nil && !errors.Is(err, domain.ErrBucketNotFound) {
		ctx := context.WithoutCancel(ctx)
		for i, derr := range errs {
			if derr == nil {
				if uerr := e.drives[i].MakeVol(ctx, volume); uerr != nil {
					e.logger.WarnContext(ctx, "object: cannot roll back volume removal", "volume", volume, "drive", i+1, "err", uerr)
				}
			}
		}
	}
	return err
}

// --- metadata ------------------------------------------------------------------

// shardOf returns the 0-based shard index stored on drive i by a version
// with layout ei.
func shardOf(ei storage.ErasureInfo, i int) int {
	if i < len(ei.Distribution) && ei.Distribution[i] >= 1 {
		return ei.Distribution[i] - 1
	}
	return i
}

// forDrive returns the metadata of fi as stored on drive i: the erasure
// index of the drive's shard.
func forDrive(fi storage.FileInfo, i int) storage.FileInfo {
	if fi.Erasure.DataBlocks > 0 {
		fi.Erasure.Index = shardOf(fi.Erasure, i) + 1
	}
	return fi
}

func (e *erasureSet) ReadVersion(ctx context.Context, volume, path, versionID string, readData bool) (storage.FileInfo, error) {
	infos := make([]storage.FileInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		fi, err := d.ReadVersion(ctx, volume, path, versionID, storage.ReadOptions{ReadData: readData})
		infos[i] = fi
		return err
	})
	fi, agreeing, err := pickValidFileInfo(infos, errs, e.readQuorum())
	if err != nil {
		if absent(err) && slices.Contains(errs, nil) {
			// Missing on a quorum of drives but left on others: a dangling
			// leftover the healing removes.
			e.notify(ctx, volume, path, versionID)
		}
		return storage.FileInfo{}, fmt.Errorf("%w: %s/%s", err, volume, path)
	}
	degraded := partialAgreement(errs, agreeing)
	if readData && fi.Data != nil {
		data, ok := e.verifiedInline(ctx, fi, infos, agreeing)
		if !ok {
			return storage.FileInfo{}, fmt.Errorf("%w: %s/%s: no intact copy of the inline data", domain.ErrReadQuorum, volume, path)
		}
		if !bytes.Equal(data, fi.Data) {
			degraded = true
		}
		fi.Data = data
	}
	if degraded {
		e.notifyStored(ctx, volume, path, fi.VersionID)
	}
	return fi, nil
}

// partialAgreement reports whether an online drive answered differently
// from the agreed version (error other than offline, or other content).
func partialAgreement(errs []error, agreeing []bool) bool {
	for i, err := range errs {
		if err != nil {
			if !errors.Is(err, domain.ErrDiskNotFound) {
				return true
			}
			continue
		}
		if !agreeing[i] {
			return true
		}
	}
	return false
}

// verifiedInline returns an inline content whose bitrot hash matches the
// one recorded with the version (any agreeing copy when none is recorded),
// trying the agreeing drives in order.
func (e *erasureSet) verifiedInline(ctx context.Context, fi storage.FileInfo, infos []storage.FileInfo, agreeing []bool) ([]byte, bool) {
	want := inlineHash(fi)
	codec, err := e.codecFor(ctx, fi.Erasure)
	for i, ok := range agreeing {
		if !ok {
			continue
		}
		if want == nil || err != nil || bytes.Equal(codec.BitrotSum(infos[i].Data), want) {
			return infos[i].Data, true
		}
		e.logger.WarnContext(ctx, "object: bitrot detected in inline data", "bucket", fi.Volume, "key", fi.Name, "drive", i+1)
	}
	return nil, false
}

// inlineHash returns the bitrot hash recorded for inline content.
func inlineHash(fi storage.FileInfo) []byte {
	for _, c := range fi.Erasure.Checksums {
		if c.PartNumber == 1 && c.Algorithm == erasure.BitrotAlgorithm && len(c.Hash) > 0 {
			return c.Hash
		}
	}
	return nil
}

func (e *erasureSet) WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		return d.WriteMetadata(ctx, volume, path, forDrive(fi, i))
	})
	return e.afterWrite(ctx, volume, path, fi.VersionID, errs, e.writeQuorumOf(fi.Erasure))
}

func (e *erasureSet) UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		return d.UpdateMetadata(ctx, volume, path, forDrive(fi, i), storage.UpdateMetadataOptions{})
	})
	return e.afterWrite(ctx, volume, path, fi.VersionID, errs, e.writeQuorumOf(fi.Erasure))
}

// DeleteVersion removes a version (or writes a delete marker) on every
// drive. The data of the removed (or replaced) version stays in place
// until the delete reached its write quorum: a delete that misses it puts
// the previous version back on the drives it reached, so that a refused
// delete never removes a version from fewer drives than its readers need.
// It fails without touching anything when too few drives are online.
func (e *erasureSet) DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	quorum := e.writeQuorum()
	if online, _ := e.Online(); online < quorum {
		return fmt.Errorf("%w: %s/%s: %d of %d drives online, %d needed", domain.ErrWriteQuorum, volume, path, online, e.n(), quorum)
	}
	// The version the delete removes, or the marker replaces, per drive.
	target := fi.VersionID
	if target == "" {
		target = nullVersion
	}
	previous := make([]*storage.FileInfo, e.n())
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		prev, err := d.ReadVersion(ctx, volume, path, target, storage.ReadOptions{ReadData: true})
		if err == nil {
			previous[i] = &prev
		}
		return nil
	})
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error {
		return d.DeleteVersion(ctx, volume, path, fi, storage.DeleteOptions{KeepData: true})
	})
	if err := reduceErrs(errs, quorum, domain.ErrWriteQuorum); err != nil {
		e.undoDelete(ctx, volume, path, fi, errs, previous)
		return err
	}
	if partial(errs) {
		e.notifyStored(ctx, volume, path, fi.VersionID)
	}
	olds := make([]string, e.n())
	for i, prev := range previous {
		if prev != nil {
			olds[i] = prev.DataDir
		}
	}
	e.purgeReplaced(ctx, volume, path, "", errs, olds)
	return nil
}

// undoDelete puts back, on the drives where a delete that missed its
// quorum succeeded, the version it removed (its data was kept), or removes
// the delete marker it added.
func (e *erasureSet) undoDelete(ctx context.Context, volume, path string, fi storage.FileInfo, errs []error, previous []*storage.FileInfo) {
	ctx = context.WithoutCancel(ctx)
	_ = parallel(e.n(), func(i int) error {
		if errs[i] != nil {
			return nil
		}
		d := e.drives[i]
		var err error
		switch prev := previous[i]; {
		case prev != nil:
			err = d.WriteMetadata(ctx, volume, path, *prev)
		case fi.MarkDeleted:
			err = d.DeleteVersion(ctx, volume, path, storage.FileInfo{VersionID: fi.VersionID}, storage.DeleteOptions{})
		default:
			err = errors.New("the removed version could not be read beforehand")
		}
		if err != nil {
			e.logger.WarnContext(ctx, "object: cannot roll back a failed delete", "bucket", volume, "key", path, "drive", i+1, "err", err)
			e.notifyStored(ctx, volume, path, fi.VersionID)
		}
		return nil
	})
}

// afterWrite reduces the errors of a metadata write of the version with
// stored id storedID and reports a write that reached its quorum but not
// every online drive.
func (e *erasureSet) afterWrite(ctx context.Context, volume, path, storedID string, errs []error, quorum int) error {
	if err := reduceErrs(errs, quorum, domain.ErrWriteQuorum); err != nil {
		return err
	}
	if partial(errs) {
		e.notifyStored(ctx, volume, path, storedID)
	}
	return nil
}

// --- data ----------------------------------------------------------------------

// ErasureInfo implements store: the parity of the storage class, the block
// size of the set and the distribution of key.
func (e *erasureSet) ErasureInfo(key, storageClass string) storage.ErasureInfo {
	p := e.classes.parity(storageClass)
	return storage.ErasureInfo{
		Algorithm:    erasure.Algorithm,
		DataBlocks:   e.n() - p,
		ParityBlocks: p,
		BlockSize:    e.blockSize,
		Distribution: hashOrder(key, e.n()),
		Checksums:    []storage.ChecksumInfo{{PartNumber: 1, Algorithm: erasure.BitrotAlgorithm}},
	}
}

// codecFor returns the codec of a recorded layout.
func (e *erasureSet) codecFor(ctx context.Context, ei storage.ErasureInfo) (erasure.Codec, error) {
	if ei.DataBlocks <= 0 || ei.ParityBlocks < 0 || ei.BlockSize <= 0 || ei.DataBlocks+ei.ParityBlocks != e.n() {
		return nil, fmt.Errorf("%w: erasure layout %d+%d/%d does not match the set of %d drives",
			domain.ErrInvalidFormat, ei.DataBlocks, ei.ParityBlocks, ei.BlockSize, e.n())
	}
	return e.codecs.Codec(ctx, erasure.Layout{DataShards: ei.DataBlocks, ParityShards: ei.ParityBlocks, BlockSize: ei.BlockSize})
}

// WriteData erasure-codes r block by block and streams every shard,
// prefixed by its bitrot hash, to its drive (drive i receives the shard of
// index ei.Distribution[i]). Drives that fail are left aside while the
// write quorum holds; otherwise the partial data is removed.
func (e *erasureSet) WriteData(ctx context.Context, dir string, partNum int, size int64, r io.Reader, ei storage.ErasureInfo) (int64, error) {
	codec, err := e.codecFor(ctx, ei)
	if err != nil {
		return 0, err
	}
	quorum := e.writeQuorumOf(ei)
	path := dir + "/" + partName(partNum)
	n := e.n()
	writers := make([]io.Writer, n)
	pipes := make([]*io.PipeWriter, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, d := range e.drives {
		if !isOnline(d) {
			errs[i] = e.offline(i)
			continue
		}
		pr, pw := io.Pipe()
		pipes[i] = pw
		writers[shardOf(ei, i)] = codec.BitrotWriter(ctx, pw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := d.CreateFile(ctx, domain.SystemBucket, path, -1, &shardStream{PipeReader: pr})
			if err != nil {
				_ = pr.CloseWithError(err)
			} else {
				_ = pr.Close()
			}
			errs[i] = err
		}()
	}
	src := r
	if size >= 0 {
		src = io.LimitReader(r, size)
	}
	total, encErr := codec.Encode(ctx, src, writers, quorum)
	for _, pw := range pipes {
		if pw == nil {
			continue
		}
		if encErr != nil {
			_ = pw.CloseWithError(encErr)
		} else {
			_ = pw.Close()
		}
	}
	wg.Wait()
	if encErr != nil {
		e.cleanup(ctx, dir)
		if errors.Is(encErr, erasure.ErrWriteQuorum) {
			return total, reduceErrs(errs, quorum, encErr)
		}
		return total, encErr
	}
	if err := reduceErrs(errs, quorum, domain.ErrWriteQuorum); err != nil {
		e.cleanup(ctx, dir)
		return total, err
	}
	return total, nil
}

// shardStream is the stream of one shard file handed to a drive. Its
// String method describes it without reading the pipe state, which a
// writer is changing concurrently (drive decorators may log their
// arguments).
type shardStream struct {
	*io.PipeReader
}

// String implements fmt.Stringer.
func (*shardStream) String() string { return "erasure shard stream" }

// cleanup removes a temporary directory from every drive, best effort,
// even when the client went away.
func (e *erasureSet) cleanup(ctx context.Context, dir string) {
	ctx = context.WithoutCancel(ctx)
	_ = e.onDrives(func(_ int, d storage.StorageAPI) error {
		return d.Delete(ctx, domain.SystemBucket, dir, storage.DeleteOptions{Recursive: true})
	})
}

// CommitData renames the staged data and the new metadata into place on
// every drive that holds the whole staged data. When the write quorum is
// missed, the drives that committed are rolled back: the data directory
// goes back to tmpDir and the metadata to its previous state, so that no
// partial version is visible. Once the quorum is met, every drive that
// committed purges the data directory of the version it replaced.
func (e *erasureSet) CommitData(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string) error {
	if fi.Data != nil {
		if codec, err := e.codecFor(ctx, fi.Erasure); err == nil {
			fi.Erasure.Checksums = []storage.ChecksumInfo{{PartNumber: 1, Algorithm: erasure.BitrotAlgorithm, Hash: codec.BitrotSum(fi.Data)}}
		}
	} else if fi.DataDir != "" && fi.Erasure.DataBlocks > 0 {
		sums := make([]storage.ChecksumInfo, 0, len(fi.Parts))
		for _, p := range fi.Parts {
			sums = append(sums, storage.ChecksumInfo{PartNumber: p.Number, Algorithm: erasure.BitrotAlgorithm})
		}
		fi.Erasure.Checksums = sums
	}
	quorum := e.writeQuorumOf(fi.Erasure)
	staged := e.stagedParts(ctx, tmpDir, fi)
	if ready := e.n() - countErrs(staged); ready < quorum {
		return fmt.Errorf("%w: %s/%s: the staged data is complete on %d of %d drives, %d needed",
			domain.ErrWriteQuorum, volume, path, ready, e.n(), quorum)
	}
	// The version this commit may replace (the null version, or a forced
	// id), per drive, to restore it if the commit has to be undone.
	previous := make([]*storage.FileInfo, e.n())
	replaced := fi.VersionID
	if replaced == "" {
		replaced = nullVersion
	}
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		if staged[i] != nil {
			return nil
		}
		prev, err := d.ReadVersion(ctx, volume, path, replaced, storage.ReadOptions{ReadData: true})
		if err == nil {
			previous[i] = &prev
		}
		return nil
	})
	olds := make([]string, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		if staged[i] != nil {
			return staged[i]
		}
		res, err := d.RenameData(ctx, domain.SystemBucket, tmpDir, forDrive(fi, i), volume, path, storage.RenameOptions{})
		olds[i] = res.OldDataDir
		return err
	})
	if err := reduceErrs(errs, quorum, domain.ErrWriteQuorum); err != nil {
		e.undoCommit(ctx, tmpDir, fi, volume, path, errs, previous)
		return err
	}
	if partial(errs) {
		e.notifyStored(ctx, volume, path, fi.VersionID)
	}
	e.purgeReplaced(ctx, volume, path, fi.DataDir, errs, olds)
	return nil
}

// countErrs returns the number of non-nil errors.
func countErrs(errs []error) int {
	n := 0
	for _, err := range errs {
		if err != nil {
			n++
		}
	}
	return n
}

// stagedParts checks, drive by drive, that tmpDir holds every part of fi
// with the size of its shard file. A drive that missed a part (a failed
// shard write, a failed move of a multipart part) must neither count
// towards the quorum of the commit nor receive metadata naming data it
// does not have: it is left to the healing.
func (e *erasureSet) stagedParts(ctx context.Context, tmpDir string, fi storage.FileInfo) []error {
	out := make([]error, e.n())
	if fi.DataDir == "" || len(fi.Parts) == 0 {
		return out
	}
	codec, err := e.codecFor(ctx, fi.Erasure)
	if err != nil {
		return out // the commit refuses the layout on every drive
	}
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, tmpDir+"/"+partPrefix+"*", true)
		if err != nil {
			return err
		}
		sizes := make(map[string]int64, len(infos))
		for _, info := range infos {
			sizes[info.Name] = info.Size
		}
		for _, p := range fi.Parts {
			want := codec.BitrotFileSize(p.Size)
			if want == 0 {
				continue // an empty part has no shard data to read
			}
			if got, ok := sizes[tmpDir+"/"+partName(p.Number)]; !ok || got != want {
				return fmt.Errorf("%w: pool %d set %d drive %d: staged part %d is missing or truncated",
					domain.ErrFaultyDisk, e.poolIdx+1, e.setIdx+1, i+1, p.Number)
			}
		}
		return nil
	})
	copy(out, errs)
	return out
}

// purgeReplaced removes, on every drive that committed, the data directory
// of the version the commit replaced there (drives may disagree on it).
// The commit is done: the replaced data goes even when the client went
// away, but not when the namespace lock was lost, since another writer may
// then be committing the same key.
func (e *erasureSet) purgeReplaced(ctx context.Context, volume, path, current string, errs []error, olds []string) {
	if lockLost(ctx) {
		e.logger.WarnContext(ctx, "object: lock lost after the commit, replaced data left in place", "bucket", volume, "key", path)
		return
	}
	ctx = context.WithoutCancel(ctx)
	_ = parallel(e.n(), func(i int) error {
		if errs[i] != nil || olds[i] == "" || olds[i] == current {
			return nil
		}
		err := e.drives[i].Delete(ctx, volume, storage.ObjectDataDir(path, olds[i]), storage.DeleteOptions{Recursive: true})
		if err != nil && !isNotFound(err) {
			e.logger.WarnContext(ctx, "object: cannot purge replaced data", "bucket", volume, "key", path,
				"data_dir", olds[i], "drive", i+1, "err", err)
		}
		return nil
	})
}

// undoCommit reverts the drives where a commit that missed its quorum
// succeeded.
func (e *erasureSet) undoCommit(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string, errs []error, previous []*storage.FileInfo) {
	ctx = context.WithoutCancel(ctx)
	_ = parallel(e.n(), func(i int) error {
		if errs[i] != nil {
			return nil
		}
		d := e.drives[i]
		if fi.DataDir != "" {
			if err := d.RenameFile(ctx, volume, storage.ObjectDataDir(path, fi.DataDir), domain.SystemBucket, tmpDir); err != nil {
				e.logger.WarnContext(ctx, "object: cannot move data back after a failed commit", "bucket", volume, "key", path, "drive", i+1, "err", err)
			}
		}
		var err error
		if prev := previous[i]; prev != nil {
			err = d.WriteMetadata(ctx, volume, path, *prev)
		} else {
			err = d.DeleteVersion(ctx, volume, path, storage.FileInfo{VersionID: fi.VersionID}, storage.DeleteOptions{})
		}
		if err != nil {
			e.logger.WarnContext(ctx, "object: cannot roll back a failed commit", "bucket", volume, "key", path, "drive", i+1, "err", err)
			e.notifyStored(ctx, volume, path, fi.VersionID)
		}
		return nil
	})
}

// ReadData decodes [offset, offset+length) of a part from the shards of
// the online drives: D shards are read (data shards first), others are
// added when a shard fails, is corrupt or is slow. Damaged shards are
// reported to the heal notifier once the read is over.
func (e *erasureSet) ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	codec, err := e.codecFor(ctx, fi.Erasure)
	if err != nil {
		return nil, fmt.Errorf("%s/%s: %w", volume, path, err)
	}
	if offset < 0 || offset+length > part.Size {
		return nil, fmt.Errorf("%w: [%d,%d) beyond part of %d bytes", domain.ErrInvalidRange, offset, offset+length, part.Size)
	}
	dataPath := storage.ObjectDataPath(path, fi.DataDir, part.Number)
	online := 0
	for _, d := range e.drives {
		if isOnline(d) {
			online++
		}
	}
	if online < codec.DataShards() {
		return nil, fmt.Errorf("%w: %s/%s: %d of %d drives online, %d needed", domain.ErrReadQuorum, volume, path, online, e.n(), codec.DataShards())
	}
	dctx, cancel := context.WithCancel(ctx)
	readers := make([]erasure.ShardReader, e.n())
	for i, d := range e.drives {
		if !isOnline(d) {
			continue
		}
		readers[shardOf(fi.Erasure, i)] = codec.BitrotReader(dctx, func(ctx context.Context, off int64) (io.ReadCloser, error) {
			return d.ReadFileStream(ctx, volume, dataPath, off, -1)
		})
	}
	pr, pw := io.Pipe()
	dr := &decodeReader{PipeReader: pr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(dr.done)
		res, err := codec.Decode(dctx, pw, readers, offset, length, part.Size)
		for _, r := range readers {
			if r != nil {
				_ = r.Close()
			}
		}
		// A decoding stopped by Close says nothing about the shards it
		// did not finish reading.
		if err == nil || dctx.Err() == nil {
			e.afterRead(ctx, volume, path, fi.VersionID, res.ShardErrs)
		}
		_ = pw.CloseWithError(err)
	}()
	return dr, nil
}

// decodeReader is the body of an erasure-coded part: the output of the
// decoding goroutine. Close stops the decoding and waits for it, so that
// no shard is read once the body is closed (and the read lock released).
type decodeReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

// Close implements io.Closer.
func (d *decodeReader) Close() error {
	err := d.PipeReader.Close()
	d.cancel()
	<-d.done
	return err
}

// afterRead reports the shards a read of the version with stored id
// storedID found damaged (missing file, bitrot, truncated): the response
// was served from the others, the object needs healing.
func (e *erasureSet) afterRead(ctx context.Context, volume, path, storedID string, shardErrs []error) {
	damaged := false
	for i, err := range shardErrs {
		if err == nil || errors.Is(err, domain.ErrDiskNotFound) || errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrClosedPipe) {
			continue
		}
		damaged = true
		if errors.Is(err, erasure.ErrBitrot) {
			e.logger.WarnContext(ctx, "object: bitrot detected", "bucket", volume, "key", path, "shard", i+1, "err", err)
		}
	}
	if damaged {
		e.notifyStored(ctx, volume, path, storedID)
	}
}

// --- system files ----------------------------------------------------------------

// ReadAll returns the content of a system file that a read quorum of
// drives agree on.
func (e *erasureSet) ReadAll(ctx context.Context, path string) ([]byte, error) {
	contents := make([][]byte, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		data, err := d.ReadAll(ctx, domain.SystemBucket, path)
		contents[i] = data
		return err
	})
	best, bestCount, withQuorum := -1, 0, 0
	counts := make([]int, e.n())
	for i, err := range errs {
		if err != nil {
			continue
		}
		for j := 0; j <= i; j++ {
			if errs[j] == nil && bytes.Equal(contents[i], contents[j]) {
				counts[j]++
				if counts[j] == e.readQuorum() {
					withQuorum++
				}
				if counts[j] > bestCount {
					best, bestCount = j, counts[j]
				}
				break
			}
		}
	}
	if withQuorum > 1 {
		// Two contents each on a read quorum (D == P): which one was
		// acknowledged cannot be told, the drive order must not decide.
		return nil, fmt.Errorf("%w: %s: two contents each on %d drives", domain.ErrReadQuorum, path, e.readQuorum())
	}
	if best >= 0 && bestCount >= e.readQuorum() {
		return contents[best], nil
	}
	if err := reduceErrs(errs, e.readQuorum(), nil); err != nil && (isNotFound(err) || best < 0) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s: drives disagree", domain.ErrReadQuorum, path)
}

// WriteAll writes a system file on every drive. A write that misses its
// quorum is rolled back: the drives it reached get their previous content
// back (or lose the file when they had none), so that a refused write
// never becomes visible, nor ties with the acknowledged content (D == P).
func (e *erasureSet) WriteAll(ctx context.Context, path string, data []byte) error {
	type prior struct {
		data   []byte
		absent bool
	}
	previous := make([]*prior, e.n())
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		old, err := d.ReadAll(ctx, domain.SystemBucket, path)
		switch {
		case err == nil:
			previous[i] = &prior{data: old}
		case isNotFound(err):
			previous[i] = &prior{absent: true}
		}
		return nil
	})
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error {
		return d.WriteAll(ctx, domain.SystemBucket, path, data)
	})
	err := reduceErrs(errs, e.writeQuorum(), domain.ErrWriteQuorum)
	if err == nil {
		return nil
	}
	rctx := context.WithoutCancel(ctx)
	_ = parallel(e.n(), func(i int) error {
		if errs[i] != nil {
			return nil
		}
		d, prev := e.drives[i], previous[i]
		var rerr error
		switch {
		case prev == nil:
			rerr = errors.New("previous content unknown")
		case prev.absent:
			rerr = d.Delete(rctx, domain.SystemBucket, path, storage.DeleteOptions{})
			if isNotFound(rerr) {
				rerr = nil
			}
		default:
			rerr = d.WriteAll(rctx, domain.SystemBucket, path, prev.data)
		}
		if rerr != nil {
			e.logger.WarnContext(rctx, "object: cannot roll back a refused system write", "path", path, "drive", i+1, "err", rerr)
		}
		return nil
	})
	return err
}

func (e *erasureSet) DeletePath(ctx context.Context, volume, path string, recursive bool) error {
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error {
		err := d.Delete(ctx, volume, path, storage.DeleteOptions{Recursive: recursive})
		if isNotFound(err) {
			return errNotFoundOK
		}
		return err
	})
	notFound := 0
	for i, err := range errs {
		if errors.Is(err, errNotFoundOK) {
			errs[i] = nil
			notFound++
		}
	}
	if err := reduceErrs(errs, e.writeQuorum(), domain.ErrWriteQuorum); err != nil {
		return err
	}
	if notFound == e.n() {
		return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, path)
	}
	return nil
}

// errNotFoundOK marks a delete that found nothing to remove.
var errNotFoundOK = errors.New("object: nothing to delete")

// MoveFiles implements store. The quorum is the one of the layout the
// moved files belong to (the parts of an upload are written with the
// layout of its storage class), so that a part is never acknowledged on
// fewer drives than its decoding needs.
func (e *erasureSet) MoveFiles(ctx context.Context, moves []fileMove, ei storage.ErasureInfo) error {
	errs := e.onDrives(func(_ int, d storage.StorageAPI) error {
		return moveAll(ctx, d, moves, e.logger)
	})
	if err := reduceErrs(errs, e.writeQuorumOf(ei), domain.ErrWriteQuorum); err != nil {
		e.UndoMoves(ctx, moves)
		return err
	}
	return nil
}

// StageParts implements store.
func (e *erasureSet) StageParts(ctx context.Context, moves []fileMove, stagingDir string, fi storage.FileInfo) error {
	if len(moves) != len(fi.Parts) {
		return fmt.Errorf("%w: %d moves for %d parts", domain.ErrInvalidArgument, len(moves), len(fi.Parts))
	}
	codec, err := e.codecFor(ctx, fi.Erasure)
	if err != nil {
		return err
	}
	n := e.n()
	moveErrs := make([][]error, n)
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		errs := make([]error, len(moves))
		for j, m := range moves {
			errs[j] = d.RenameFile(ctx, domain.SystemBucket, m.src, domain.SystemBucket, m.dst)
		}
		moveErrs[i] = errs
		return nil
	})
	// The size of every staged shard file, per drive.
	sizes := make([]map[string]int64, n)
	statErrs := e.onDrives(func(i int, d storage.StorageAPI) error {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, stagingDir+"/"+partPrefix+"*", true)
		if err != nil && !isNotFound(err) {
			return err
		}
		sizes[i] = make(map[string]int64, len(infos))
		for _, info := range infos {
			sizes[i][info.Name] = info.Size
		}
		return nil
	})
	data := codec.DataShards()
	for j, p := range fi.Parts {
		dst := moves[j].dst
		want := codec.BitrotFileSize(p.Size)
		holders := make([]bool, n)
		count, unknown := 0, 0
		var targets []int
		for i := range n {
			if statErrs[i] != nil {
				unknown++ // offline or failing: it may hold the part
				continue
			}
			if got, ok := sizes[i][dst]; ok && got == want {
				holders[i] = true
				count++
				continue
			}
			targets = append(targets, i)
			if merr := moveErrs[i]; merr != nil && merr[j] != nil && !isNotFound(merr[j]) {
				unknown++ // the move failed: the part may be at its source
			}
		}
		if count < data {
			if count+unknown < data {
				return fmt.Errorf("%w: part %d is held by %d of %d drives, %d needed to decode it",
					domain.ErrObjectNotFound, p.Number, count, n, data)
			}
			return fmt.Errorf("%w: part %d is held by %d of %d reachable drives, %d needed to decode it",
				domain.ErrWriteQuorum, p.Number, count, n-unknown, data)
		}
		if len(targets) == 0 {
			continue
		}
		errs, err := e.healShards(ctx, codec, fi.Erasure, p.Size, holders, domain.SystemBucket, dst, targets, dst)
		if err != nil {
			e.logger.WarnContext(ctx, "object: cannot rebuild a part of a multipart completion", "part", p.Number, "err", err)
			continue
		}
		for _, i := range targets {
			if errs[i] != nil {
				e.logger.WarnContext(ctx, "object: cannot rebuild a part of a multipart completion on a drive",
					"part", p.Number, "drive", i+1, "err", errs[i])
			}
		}
	}
	return nil
}

// healShards rebuilds the shard file of one part (layout ei, size logical
// bytes) on the target drives, at dstPath of the system volume, from the
// shard files srcVolume/srcPath of the source drives. It returns the
// error of every drive (nil for the targets written), or an error when the
// part could not be decoded.
func (e *erasureSet) healShards(ctx context.Context, codec erasure.Codec, ei storage.ErasureInfo, size int64, sources []bool,
	srcVolume, srcPath string, targets []int, dstPath string,
) ([]error, error) {
	readers := make([]erasure.ShardReader, e.n())
	for i, d := range e.drives {
		if !sources[i] || !isOnline(d) {
			continue
		}
		readers[shardOf(ei, i)] = codec.BitrotReader(ctx, func(ctx context.Context, off int64) (io.ReadCloser, error) {
			return d.ReadFileStream(ctx, srcVolume, srcPath, off, -1)
		})
	}
	defer func() {
		for _, r := range readers {
			if r != nil {
				_ = r.Close()
			}
		}
	}()
	writers := make([]io.Writer, e.n())
	pipes := make([]*io.PipeWriter, e.n())
	errs := make([]error, e.n())
	var wg sync.WaitGroup
	for _, i := range targets {
		pr, pw := io.Pipe()
		pipes[i] = pw
		writers[shardOf(ei, i)] = codec.BitrotWriter(ctx, pw)
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.drives[i].CreateFile(ctx, domain.SystemBucket, dstPath, -1, &shardStream{PipeReader: pr})
			if err != nil {
				_ = pr.CloseWithError(err)
			} else {
				_ = pr.Close()
			}
			errs[i] = err
		}()
	}
	_, herr := codec.Heal(ctx, writers, readers, size)
	for _, pw := range pipes {
		if pw == nil {
			continue
		}
		if herr != nil {
			_ = pw.CloseWithError(herr)
		} else {
			_ = pw.Close()
		}
	}
	wg.Wait()
	if herr != nil {
		return nil, herr
	}
	return errs, nil
}

// UndoMoves implements store.
func (e *erasureSet) UndoMoves(ctx context.Context, moves []fileMove) {
	_ = e.onDrives(func(_ int, d storage.StorageAPI) error {
		undoMoves(ctx, d, moves, e.logger)
		return nil
	})
}

// StatGlob returns the union of the matches of the online drives: callers
// read every match with quorum (ReadAll), which filters out the leftovers
// of a single drive.
func (e *erasureSet) StatGlob(ctx context.Context, pattern string) ([]storage.StatInfo, error) {
	lists := make([][]storage.StatInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, pattern, true)
		lists[i] = infos
		return err
	})
	if err := reduceErrs(errs, e.readQuorum(), domain.ErrReadQuorum); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []storage.StatInfo
	for _, infos := range lists {
		for _, info := range infos {
			if !seen[info.Name] {
				seen[info.Name] = true
				out = append(out, info)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
