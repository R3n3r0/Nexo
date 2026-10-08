package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

var _ Healer = (*erasureServerPools)(nil)

// HealBucket implements Healer.
func (p *erasureServerPools) HealBucket(ctx context.Context, bucket string, opts HealOptions) (HealResult, error) {
	res := HealResult{Bucket: bucket}
	if err := validBucket(bucket); err != nil {
		return res, err
	}
	lc, err := p.lock(ctx, bucket, "", true)
	if err != nil {
		return res, err
	}
	defer lc.Unlock(ctx)
	exists := false
	var record []byte
	for _, st := range p.sets {
		if _, err := st.StatVol(ctx, bucket); err == nil {
			exists = true
			if record == nil {
				record, _ = st.ReadAll(ctx, bucketDocPath(bucket))
			}
		}
	}
	if !exists {
		return res, fmt.Errorf("%s: heal bucket %s: %w", p.name, bucket, domain.ErrBucketNotFound)
	}
	for _, pl := range p.pools {
		for _, set := range pl.sets {
			r := set.healBucket(ctx, bucket, record, opts)
			res.Drives += r.Drives
			res.Offline += r.Offline
			res.Healed += r.Healed
			res.DriveStates = append(res.DriveStates, r.DriveStates...)
		}
	}
	return res, nil
}

// healBucket creates the volume and the bucket record on the drives of
// the set that miss them.
func (e *erasureSet) healBucket(ctx context.Context, bucket string, record []byte, opts HealOptions) HealResult {
	res := HealResult{Bucket: bucket, Drives: e.n()}
	before := e.offlineStates()
	after := make([]HealDriveState, e.n())
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		before[i] = HealDriveOK
		if _, err := d.StatVol(ctx, bucket); err != nil {
			before[i] = HealDriveCorrupt
			if errors.Is(err, domain.ErrBucketNotFound) {
				before[i] = HealDriveMissing
			}
		}
		if record != nil {
			if have, err := d.ReadAll(ctx, domain.SystemBucket, bucketDocPath(bucket)); err != nil || !bytes.Equal(have, record) {
				if before[i] == HealDriveOK {
					before[i] = HealDriveCorrupt
					if isNotFound(err) {
						before[i] = HealDriveMissing
					}
				}
			}
		}
		after[i] = before[i]
		if before[i] == HealDriveOK || opts.DryRun {
			return nil
		}
		if err := d.MakeVol(ctx, bucket); err != nil && !errors.Is(err, domain.ErrBucketAlreadyExists) {
			return err
		}
		if record != nil {
			if err := d.WriteAll(ctx, domain.SystemBucket, bucketDocPath(bucket), record); err != nil {
				return err
			}
		}
		after[i] = HealDriveOK
		return nil
	})
	for i := range before {
		if opts.DryRun || after[i] == "" {
			after[i] = before[i]
		}
		if before[i] != HealDriveOK && before[i] != HealDriveOffline && (opts.DryRun || after[i] == HealDriveOK) {
			res.Healed++
		}
	}
	online, _ := e.Online()
	res.Offline = e.n() - online
	res.DriveStates = e.driveStates(before, after)
	return res
}

// offlineStates returns the initial per-drive states of an operation:
// offline until the drive answers.
func (e *erasureSet) offlineStates() []HealDriveState {
	states := make([]HealDriveState, e.n())
	for i := range states {
		states[i] = HealDriveOffline
	}
	return states
}

// driveStates pairs the states before and after an operation with the
// position of every drive of the set.
func (e *erasureSet) driveStates(before, after []HealDriveState) []HealDrive {
	out := make([]HealDrive, e.n())
	for i, d := range e.drives {
		out[i] = HealDrive{Pool: e.poolIdx, Set: e.setIdx, Disk: i, Before: before[i], After: after[i]}
		if d != nil {
			out[i].Endpoint = d.Endpoint().String()
		}
	}
	return out
}

// stateRank orders the drive states from the best to the worst.
func stateRank(s HealDriveState) int {
	switch s {
	case HealDriveOK:
		return 0
	case HealDriveMissing:
		return 1
	case HealDriveCorrupt:
		return 2
	default:
		return 3
	}
}

// mergeDriveStates keeps, drive by drive, the worst states of two reports
// of the same set (two versions of an object).
func mergeDriveStates(acc, next []HealDrive) []HealDrive {
	if len(acc) != len(next) {
		return append(acc, next...)
	}
	for i := range acc {
		if stateRank(next[i].Before) > stateRank(acc[i].Before) {
			acc[i].Before = next[i].Before
		}
		if stateRank(next[i].After) > stateRank(acc[i].After) {
			acc[i].After = next[i].After
		}
	}
	return acc
}

// HealObject implements Healer.
func (p *erasureServerPools) HealObject(ctx context.Context, bucket, key, versionID string, opts HealOptions) (HealResult, error) {
	res := HealResult{Bucket: bucket, Key: key}
	if err := validObject(bucket, key); err != nil {
		return res, err
	}
	lc, err := p.lock(ctx, bucket, key, true)
	if err != nil {
		return res, err
	}
	defer lc.Unlock(ctx)
	found := false
	var notFound error
	for _, pl := range p.pools {
		r, err := pl.setFor(key).healObject(ctx, bucket, key, versionID, opts)
		if err != nil {
			if absent(err) {
				notFound = err
				continue
			}
			return res, fmt.Errorf("%s: heal %s/%s: %w", p.name, bucket, key, err)
		}
		found = true
		res.VersionIDs = append(res.VersionIDs, r.VersionIDs...)
		res.Drives += r.Drives
		res.Offline += r.Offline
		res.Healed += r.Healed
		res.Dangling += r.Dangling
		res.Removed += r.Removed
		res.Bytes += r.Bytes
		res.DriveStates = append(res.DriveStates, r.DriveStates...)
	}
	if !found {
		if notFound == nil {
			notFound = domain.ErrObjectNotFound
		}
		return res, fmt.Errorf("%s: heal %s/%s: %w", p.name, bucket, key, notFound)
	}
	return res, nil
}

// healObject heals versionID, or every version any drive holds.
func (e *erasureSet) healObject(ctx context.Context, bucket, key, versionID string, opts HealOptions) (HealResult, error) {
	versions := []string{versionID}
	if opts.AllVersions {
		var err error
		if versions, err = e.versionIDs(ctx, bucket, key); err != nil {
			return HealResult{}, err
		}
		if len(versions) == 0 {
			return HealResult{}, fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, bucket, key)
		}
	}
	res := HealResult{Bucket: bucket, Key: key, Drives: e.n()}
	examined := 0
	var last error
	for _, v := range versions {
		r, err := e.healVersion(ctx, bucket, key, v, opts)
		if err != nil {
			if absent(err) {
				last = err
				continue
			}
			return res, err
		}
		examined++
		res.VersionIDs = append(res.VersionIDs, v)
		res.Offline = r.Offline
		res.Healed += r.Healed
		res.Dangling += r.Dangling
		res.Removed += r.Removed
		res.Bytes += r.Bytes
		res.DriveStates = mergeDriveStates(res.DriveStates, r.DriveStates)
	}
	if examined == 0 {
		if last == nil {
			last = domain.ErrObjectNotFound
		}
		return res, last
	}
	return res, nil
}

// versionIDs returns the ids of the versions of key held by any online
// drive (the null version as "null"), latest first on the first drive.
func (e *erasureSet) versionIDs(ctx context.Context, bucket, key string) ([]string, error) {
	perDrive := make([][]storage.FileInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		versions, err := driveVersions(ctx, d, bucket, key)
		perDrive[i] = versions
		return err
	})
	if err := reduceErrs(errs, 1, nil); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, versions := range perDrive {
		for _, fi := range versions {
			id := fi.VersionID
			if id == "" {
				id = nullVersion
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}

// driveVersions reads the versions of key recorded on one drive.
func driveVersions(ctx context.Context, d storage.StorageAPI, bucket, key string) ([]storage.FileInfo, error) {
	parent, _ := splitPrefix(strings.TrimSuffix(key, "/"))
	var out []storage.FileInfo
	err := d.WalkDir(ctx, storage.WalkDirOptions{Bucket: bucket, BaseDir: parent, FilterPrefix: key, ForwardTo: key}, func(we storage.WalkEntry) error {
		if we.IsDir || we.Name != key {
			if we.Name > key {
				return errListDone
			}
			return nil
		}
		versions, err := storage.DecodeFileInfos(bucket, key, we.Metadata)
		if err != nil {
			return nil // unreadable here: the drive is healed
		}
		out = versions
		return errListDone
	})
	if err != nil && !errors.Is(err, errListDone) {
		return nil, err
	}
	return out, nil
}

// healTarget is a drive to repair: the whole version, or its metadata only.
type healTarget struct {
	drive    int
	metaOnly bool
}

// healVersion repairs one version on the drives that need it.
func (e *erasureSet) healVersion(ctx context.Context, bucket, key, versionID string, opts HealOptions) (HealResult, error) {
	res := HealResult{Bucket: bucket, Key: key, Drives: e.n(), VersionIDs: []string{versionID}}
	infos := make([]storage.FileInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		fi, err := d.ReadVersion(ctx, bucket, key, versionID, storage.ReadOptions{ReadData: true})
		infos[i] = fi
		return err
	})
	before := make([]HealDriveState, e.n())
	for i, err := range errs {
		before[i] = readState(err)
		if before[i] == HealDriveOffline {
			res.Offline++
		}
	}
	fi, agreeing, err := pickValidFileInfo(infos, errs, e.readQuorum())
	if err != nil {
		return e.healDangling(ctx, bucket, key, infos, errs, opts, res, before, err)
	}
	if !fi.Deleted {
		res.Bytes = fi.Size
	}
	for i := range before {
		if errs[i] == nil && !agreeing[i] {
			before[i] = HealDriveMissing // another (outdated) version
		}
	}
	var codec erasure.Codec
	if fi.DataDir != "" && !fi.Deleted {
		if codec, err = e.codecFor(ctx, fi.Erasure); err != nil {
			return res, err
		}
	}
	if fi.Data != nil {
		data, ok := e.verifiedInline(ctx, fi, infos, agreeing)
		if !ok {
			return res, fmt.Errorf("%w: %s/%s: no intact copy of the inline data", domain.ErrReadQuorum, bucket, key)
		}
		fi.Data = data
	}
	want := metadataKey(fi)
	wantHash := inlineHash(fi)
	var (
		mu      sync.Mutex
		targets []healTarget
	)
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		t := healTarget{drive: i}
		shards := HealDriveOK
		if errs[i] == nil && agreeing[i] && codec != nil {
			shards = e.checkShards(ctx, d, bucket, key, forDrive(fi, i), codec, opts.Deep)
		}
		switch {
		case errs[i] != nil || !agreeing[i]:
		case fi.Data != nil && wantHash != nil && codec == nil && !bytes.Equal(e.inlineSum(ctx, fi, infos[i].Data), wantHash):
			before[i] = HealDriveCorrupt
		case shards != HealDriveOK:
			before[i] = shards
		case metadataKey(infos[i]) != want:
			t.metaOnly = true
			before[i] = HealDriveCorrupt
		default:
			return nil
		}
		mu.Lock()
		targets = append(targets, t)
		mu.Unlock()
		return nil
	})
	after := slices.Clone(before)
	res.Healed = len(targets)
	if len(targets) == 0 || opts.DryRun {
		res.DriveStates = e.driveStates(before, after)
		return res, nil
	}
	healed := e.repair(ctx, bucket, key, fi, codec, targets)
	res.Healed = len(healed)
	for i := range healed {
		after[i] = HealDriveOK
	}
	res.DriveStates = e.driveStates(before, after)
	if len(healed) < len(targets) {
		return res, fmt.Errorf("%w: %s/%s: %d of %d drives healed", domain.ErrFaultyDisk, bucket, key, len(healed), len(targets))
	}
	return res, nil
}

// readState classifies the outcome of reading a version from one drive.
func readState(err error) HealDriveState {
	switch {
	case err == nil:
		return HealDriveOK
	case errors.Is(err, domain.ErrDiskNotFound):
		return HealDriveOffline
	case isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion):
		return HealDriveMissing
	default:
		return HealDriveCorrupt
	}
}

// inlineSum hashes inline content with the codec of fi's layout.
func (e *erasureSet) inlineSum(ctx context.Context, fi storage.FileInfo, data []byte) []byte {
	codec, err := e.codecFor(ctx, fi.Erasure)
	if err != nil {
		return nil
	}
	return codec.BitrotSum(data)
}

// healDangling handles a version without read quorum: when more drives
// than its parity miss it, it can never be read again (dangling) and is
// removed with opts.Remove; otherwise the error stands.
func (e *erasureSet) healDangling(ctx context.Context, bucket, key string, infos []storage.FileInfo, errs []error, opts HealOptions,
	res HealResult, before []HealDriveState, cause error,
) (HealResult, error) {
	var holders []int
	missing := 0
	parity := e.classes.standard
	for i, err := range errs {
		switch {
		case err == nil:
			holders = append(holders, i)
			if infos[i].Erasure.DataBlocks > 0 {
				parity = infos[i].Erasure.ParityBlocks
			}
		case isNotFound(err) || errors.Is(err, domain.ErrNoSuchVersion):
			missing++
		}
	}
	after := slices.Clone(before)
	res.DriveStates = e.driveStates(before, after)
	if len(holders) == 0 || missing <= parity {
		return res, cause
	}
	res.Dangling = 1
	if !opts.Remove || opts.DryRun {
		return res, nil
	}
	for _, i := range holders {
		err := e.drives[i].DeleteVersion(ctx, bucket, key, storage.FileInfo{VersionID: infos[i].VersionID}, storage.DeleteOptions{})
		if err != nil && !absent(err) {
			return res, fmt.Errorf("%s/%s: remove dangling version from drive %d: %w", bucket, key, i+1, err)
		}
		after[i] = HealDriveMissing
	}
	res.DriveStates = e.driveStates(before, after)
	res.Removed = 1
	return res, nil
}

// checkShards checks the shard files of a version on one drive: their
// presence (missing) and size (corrupt), and with deep the bitrot hash
// of every block (corrupt).
func (e *erasureSet) checkShards(ctx context.Context, d storage.StorageAPI, bucket, key string, fi storage.FileInfo, codec erasure.Codec, deep bool) HealDriveState {
	check := fi
	check.Parts = make([]storage.PartInfo, len(fi.Parts))
	for j, p := range fi.Parts {
		check.Parts[j] = p
		check.Parts[j].Size = codec.BitrotFileSize(p.Size)
	}
	res, err := d.CheckParts(ctx, bucket, key, check)
	if err != nil {
		return readState(err)
	}
	state := HealDriveOK
	for _, r := range res.Results {
		switch r {
		case storage.CheckPartSuccess:
		case storage.CheckPartFileNotFound, storage.CheckPartVolumeNotFound:
			if state == HealDriveOK {
				state = HealDriveMissing
			}
		default:
			state = HealDriveCorrupt
		}
	}
	if state != HealDriveOK || !deep {
		return state
	}
	for _, p := range fi.Parts {
		path := storage.ObjectDataPath(key, fi.DataDir, p.Number)
		r := codec.BitrotReader(ctx, func(ctx context.Context, off int64) (io.ReadCloser, error) {
			return d.ReadFileStream(ctx, bucket, path, off, -1)
		})
		ok := verifyShard(r, codec, p.Size)
		_ = r.Close()
		if !ok {
			return HealDriveCorrupt
		}
	}
	return HealDriveOK
}

// verifyShard reads every block of a shard file through its bitrot reader.
func verifyShard(r erasure.ShardReader, codec erasure.Codec, size int64) bool {
	bs, ss, data := codec.BlockSize(), codec.ShardSize(), int64(codec.DataShards())
	buf := make([]byte, ss)
	for b := int64(0); b*bs < size; b++ {
		blockLen := min(bs, size-b*bs)
		shardLen := (blockLen + data - 1) / data
		if _, err := r.ReadAt(buf[:shardLen], b*ss); err != nil {
			return false
		}
	}
	return true
}

// repair rewrites the version on the target drives and returns the
// drives healed. Metadata-only targets get the agreed metadata, the others
// the whole version: its metadata (with the inline content) or its shards,
// rebuilt from the healthy drives.
func (e *erasureSet) repair(ctx context.Context, bucket, key string, fi storage.FileInfo, codec erasure.Codec, targets []healTarget) map[int]bool {
	healed := make(map[int]bool, len(targets))
	var data []int
	for _, t := range targets {
		d := e.drives[t.drive]
		var err error
		switch {
		case t.metaOnly && codec != nil:
			err = d.UpdateMetadata(ctx, bucket, key, forDrive(fi, t.drive), storage.UpdateMetadataOptions{})
		case codec == nil:
			err = d.WriteMetadata(ctx, bucket, key, forDrive(fi, t.drive))
		default:
			data = append(data, t.drive)
			continue
		}
		if err != nil {
			e.logger.WarnContext(ctx, "object: heal failed", "bucket", bucket, "key", key, "drive", t.drive+1, "err", err)
			continue
		}
		healed[t.drive] = true
	}
	if len(data) > 0 {
		for _, i := range e.rebuildShards(ctx, bucket, key, fi, codec, data) {
			healed[i] = true
		}
	}
	return healed
}

// rebuildShards reconstructs the shards of the target drives from the
// healthy ones, part by part, in a temporary directory of each target,
// then renames them into place with the agreed metadata. It returns the
// drives healed.
func (e *erasureSet) rebuildShards(ctx context.Context, bucket, key string, fi storage.FileInfo, codec erasure.Codec, targets []int) []int {
	isTarget := make(map[int]bool, len(targets))
	for _, i := range targets {
		isTarget[i] = true
	}
	tmpDir := storage.TmpDir + "/" + uuid.NewString()
	defer e.cleanup(ctx, tmpDir)
	failed := make([]bool, e.n())
	for _, part := range fi.Parts {
		dataPath := storage.ObjectDataPath(key, fi.DataDir, part.Number)
		readers := make([]erasure.ShardReader, e.n())
		for i, d := range e.drives {
			if isTarget[i] || !isOnline(d) {
				continue
			}
			readers[shardOf(fi.Erasure, i)] = codec.BitrotReader(ctx, func(ctx context.Context, off int64) (io.ReadCloser, error) {
				return d.ReadFileStream(ctx, bucket, dataPath, off, -1)
			})
		}
		writers := make([]io.Writer, e.n())
		pipes := make([]*io.PipeWriter, e.n())
		errs := make([]error, e.n())
		var wg sync.WaitGroup
		for _, i := range targets {
			if failed[i] {
				continue
			}
			pr, pw := io.Pipe()
			pipes[i] = pw
			writers[shardOf(fi.Erasure, i)] = codec.BitrotWriter(ctx, pw)
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := e.drives[i].CreateFile(ctx, domain.SystemBucket, tmpDir+"/"+partName(part.Number), -1, &shardStream{PipeReader: pr})
				if err != nil {
					_ = pr.CloseWithError(err)
				} else {
					_ = pr.Close()
				}
				errs[i] = err
			}()
		}
		_, herr := codec.Heal(ctx, writers, readers, part.Size)
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
		for _, r := range readers {
			if r != nil {
				_ = r.Close()
			}
		}
		if herr != nil {
			e.logger.WarnContext(ctx, "object: cannot rebuild shards", "bucket", bucket, "key", key, "part", part.Number, "err", herr)
			return nil
		}
		for _, i := range targets {
			if errs[i] != nil {
				failed[i] = true
			}
		}
	}
	var healed []int
	for _, i := range targets {
		if failed[i] {
			continue
		}
		res, err := e.drives[i].RenameData(ctx, domain.SystemBucket, tmpDir, forDrive(fi, i), bucket, key, storage.RenameOptions{})
		if err != nil {
			e.logger.WarnContext(ctx, "object: cannot commit healed shards", "bucket", bucket, "key", key, "drive", i+1, "err", err)
			continue
		}
		if res.OldDataDir != "" && res.OldDataDir != fi.DataDir {
			_ = e.drives[i].Delete(ctx, bucket, storage.ObjectDataDir(key, res.OldDataDir), storage.DeleteOptions{Recursive: true})
		}
		healed = append(healed, i)
	}
	return healed
}
