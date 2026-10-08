package object

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/dchest/siphash"
	"github.com/klauspost/reedsolomon"
	"github.com/minio/highwayhash"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Erasure layout constants recorded in xl.meta.
const (
	erasureAlgorithm = "rs-vandermonde"
	bitrotAlgorithm  = "highwayhash256S"
	bitrotHashSize   = 32
)

// bitrotKey is the fixed HighwayHash key of the per-block shard hashes.
var bitrotKey = [32]byte{
	0x4b, 0xe7, 0x34, 0xfa, 0x8e, 0x23, 0x8a, 0xcd, 0x26, 0x3e, 0x83, 0xe6, 0xbb, 0x96, 0x80, 0x52,
	0x04, 0x0f, 0x93, 0x5d, 0xa3, 0x9f, 0x44, 0x14, 0x97, 0xe0, 0x9d, 0x13, 0x22, 0xde, 0x36, 0xa0,
}

// erasureObjects is the erasure-coded object layer (SNMD / distributed): a
// pool of erasure sets, each spreading every object over its drives as
// Reed-Solomon data and parity shards protected by per-block bitrot hashes.
type erasureObjects struct {
	*layer
	stores []*erasureStore
}

var _ Service = (*erasureObjects)(nil)

// NewErasure returns the erasure object layer over deps.Sets with
// deps.Parity parity shards per set.
func NewErasure(deps Deps, opts ...Option) (Service, error) {
	if len(deps.Sets) == 0 {
		return nil, fmt.Errorf("%w: object: NewErasure requires at least one erasure set", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	stores := make([]*erasureStore, 0, len(deps.Sets))
	sets := make([]store, 0, len(deps.Sets))
	for i, drives := range deps.Sets {
		n := len(drives)
		if n < 2 || deps.Parity < 1 || deps.Parity > n/2 {
			return nil, fmt.Errorf("%w: object: set %d has %d drives and parity %d", domain.ErrInvalidArgument, i, n, deps.Parity)
		}
		rs, err := reedsolomon.New(n-deps.Parity, deps.Parity)
		if err != nil {
			return nil, fmt.Errorf("object: erasure encoder for set %d: %w", i, err)
		}
		st := &erasureStore{drives: drives, rs: rs, data: n - deps.Parity, parity: deps.Parity, blockSize: o.blockSize, logger: deps.Logger, setIdx: i}
		stores = append(stores, st)
		sets = append(sets, st)
	}
	l := newLayer(deps, opts, sets, "erasure")
	k0, k1 := sipKeys(deps.DeploymentID)
	l.pick = func(key string) store {
		if len(sets) == 1 {
			return sets[0]
		}
		return sets[siphash.Hash(k0, k1, []byte(key))%uint64(len(sets))]
	}
	return &erasureObjects{layer: l, stores: stores}, nil
}

// sipKeys derives the SipHash keys of the set selection from the deployment id.
func sipKeys(deploymentID string) (uint64, uint64) {
	sum := sha256.Sum256([]byte(deploymentID))
	return binary.LittleEndian.Uint64(sum[:8]), binary.LittleEndian.Uint64(sum[8:16])
}

// Health implements Service.
func (e *erasureObjects) Health(_ context.Context, opts HealthOptions) HealthResult {
	res := HealthResult{Healthy: true}
	for _, st := range e.stores {
		online, total := st.Online()
		res.WriteQuorum, res.ReadQuorum = st.writeQuorum(), st.readQuorum()
		need := st.writeQuorum()
		if opts.Maintenance {
			need++
		}
		if online < need {
			res.Healthy = false
			res.Message = fmt.Sprintf("set %d has %d/%d drives online, write quorum is %d", st.setIdx+1, online, total, st.writeQuorum())
		}
	}
	return res
}

// erasureStore is the store of one erasure set.
type erasureStore struct {
	drives    []storage.StorageAPI
	rs        reedsolomon.Encoder
	data      int
	parity    int
	blockSize int64
	logger    *slog.Logger
	setIdx    int
}

var _ store = (*erasureStore)(nil)

func (e *erasureStore) writeQuorum() int {
	if e.data == e.parity {
		return e.data + 1
	}
	return e.data
}

func (e *erasureStore) readQuorum() int { return e.data }

func (e *erasureStore) Online() (int, int) {
	online := 0
	for _, d := range e.drives {
		if d != nil && d.IsOnline() {
			online++
		}
	}
	return online, len(e.drives)
}

// each runs fn on every online drive and reduces the errors with quorum.
func (e *erasureStore) each(quorum int, fallback error, fn func(i int, d storage.StorageAPI) error) error {
	errs := parallel(len(e.drives), func(i int) error {
		d := e.drives[i]
		if d == nil || !d.IsOnline() {
			return fmt.Errorf("%w: set %d drive %d", domain.ErrDiskNotFound, e.setIdx, i)
		}
		return fn(i, d)
	})
	return reduceErrs(errs, quorum, fallback)
}

func (e *erasureStore) MakeVol(ctx context.Context, volume string) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(_ int, d storage.StorageAPI) error { return d.MakeVol(ctx, volume) })
}

func (e *erasureStore) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	var last error
	for _, d := range e.drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		vols, err := d.ListVols(ctx)
		if err == nil {
			return vols, nil
		}
		last = err
	}
	if last == nil {
		last = domain.ErrReadQuorum
	}
	return nil, last
}

func (e *erasureStore) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	var (
		mu  sync.Mutex
		out storage.VolInfo
		got bool
	)
	err := e.each(e.readQuorum(), domain.ErrReadQuorum, func(_ int, d storage.StorageAPI) error {
		vi, err := d.StatVol(ctx, volume)
		if err != nil {
			return err
		}
		mu.Lock()
		if !got {
			out, got = vi, true
		}
		mu.Unlock()
		return nil
	})
	return out, err
}

func (e *erasureStore) DeleteVol(ctx context.Context, volume string, force bool) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(_ int, d storage.StorageAPI) error { return d.DeleteVol(ctx, volume, force) })
}

// versionKey identifies a version for quorum agreement.
func versionKey(fi storage.FileInfo) string {
	return fmt.Sprintf("%s|%d|%s|%d|%t", fi.VersionID, fi.ModTime.UnixNano(), fi.DataDir, fi.Size, fi.Deleted)
}

func (e *erasureStore) ReadVersion(ctx context.Context, volume, path, versionID string, readData bool) (storage.FileInfo, error) {
	infos := make([]storage.FileInfo, len(e.drives))
	errs := parallel(len(e.drives), func(i int) error {
		d := e.drives[i]
		if d == nil || !d.IsOnline() {
			return fmt.Errorf("%w: set %d drive %d", domain.ErrDiskNotFound, e.setIdx, i)
		}
		fi, err := d.ReadVersion(ctx, volume, path, versionID, storage.ReadOptions{ReadData: readData})
		infos[i] = fi
		return err
	})
	counts := make(map[string]int)
	best, bestCount := -1, 0
	for i, err := range errs {
		if err != nil {
			continue
		}
		k := versionKey(infos[i])
		counts[k]++
		if counts[k] > bestCount {
			best, bestCount = i, counts[k]
		}
	}
	if best >= 0 && bestCount >= e.readQuorum() {
		return infos[best], nil
	}
	if err := reduceErrs(errs, e.readQuorum(), nil); err != nil {
		return storage.FileInfo{}, err
	}
	return storage.FileInfo{}, fmt.Errorf("%w: %s/%s: drives disagree on the version", domain.ErrReadQuorum, volume, path)
}

func (e *erasureStore) withIndex(fi storage.FileInfo, i int) storage.FileInfo {
	fi.Erasure.Index = i + 1
	return fi
}

func (e *erasureStore) WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(i int, d storage.StorageAPI) error {
		return d.WriteMetadata(ctx, volume, path, e.withIndex(fi, i))
	})
}

func (e *erasureStore) UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(i int, d storage.StorageAPI) error {
		return d.UpdateMetadata(ctx, volume, path, e.withIndex(fi, i), storage.UpdateMetadataOptions{})
	})
}

func (e *erasureStore) DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(_ int, d storage.StorageAPI) error {
		return d.DeleteVersion(ctx, volume, path, fi, storage.DeleteOptions{})
	})
}

func (e *erasureStore) ErasureInfo() storage.ErasureInfo {
	n := e.data + e.parity
	dist := make([]int, n)
	for i := range dist {
		dist[i] = i + 1
	}
	return storage.ErasureInfo{
		Algorithm:    erasureAlgorithm,
		DataBlocks:   e.data,
		ParityBlocks: e.parity,
		BlockSize:    e.blockSize,
		Distribution: dist,
		Checksums:    []storage.ChecksumInfo{{PartNumber: 1, Algorithm: bitrotAlgorithm}},
	}
}

func bitrotSum(b []byte) []byte {
	sum := highwayhash.Sum(b, bitrotKey[:])
	return sum[:]
}

func shardSize(blockLen int64, data int) int64 { return (blockLen + int64(data) - 1) / int64(data) }

// WriteData erasure-codes r block by block and streams every shard, prefixed
// by its bitrot hash, to the corresponding drive.
func (e *erasureStore) WriteData(ctx context.Context, dir string, partNum int, size int64, r io.Reader) (int64, error) {
	path := dir + "/" + partName(partNum)
	n := len(e.drives)
	writers := make([]*io.PipeWriter, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, d := range e.drives {
		if d == nil || !d.IsOnline() {
			errs[i] = fmt.Errorf("%w: set %d drive %d", domain.ErrDiskNotFound, e.setIdx, i)
			continue
		}
		pr, pw := io.Pipe()
		writers[i] = pw
		wg.Add(1)
		go func(i int, d storage.StorageAPI, pr *io.PipeReader) {
			defer wg.Done()
			err := d.CreateFile(ctx, domain.SystemBucket, path, -1, pr)
			if err != nil {
				_ = pr.CloseWithError(err)
			} else {
				_ = pr.Close()
			}
			errs[i] = err
		}(i, d, pr)
	}
	fail := func(i int, err error) {
		if writers[i] != nil {
			_ = writers[i].CloseWithError(err)
			writers[i] = nil
		}
	}
	finish := func(abort error) {
		for i := range writers {
			if writers[i] == nil {
				continue
			}
			if abort != nil {
				_ = writers[i].CloseWithError(abort)
			} else {
				_ = writers[i].Close()
			}
			writers[i] = nil
		}
		wg.Wait()
	}

	buf := make([]byte, e.blockSize)
	var total int64
	for {
		want := int64(len(buf))
		if size >= 0 && size-total < want {
			want = size - total
		}
		if want == 0 {
			break
		}
		nb, err := io.ReadFull(r, buf[:want])
		if nb == 0 && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			finish(err)
			e.cleanup(ctx, dir)
			return total, err
		}
		total += int64(nb)
		shards, serr := e.rs.Split(buf[:nb])
		if serr == nil {
			serr = e.rs.Encode(shards)
		}
		if serr != nil {
			finish(serr)
			e.cleanup(ctx, dir)
			return total, fmt.Errorf("object: erasure encode: %w", serr)
		}
		for i, w := range writers {
			if w == nil {
				continue
			}
			if _, werr := w.Write(bitrotSum(shards[i])); werr != nil {
				fail(i, werr)
				continue
			}
			if _, werr := w.Write(shards[i]); werr != nil {
				fail(i, werr)
			}
		}
		if err != nil { // short final block: the stream ended
			break
		}
	}
	finish(nil)
	if err := reduceErrs(errs, e.writeQuorum(), domain.ErrWriteQuorum); err != nil {
		e.cleanup(ctx, dir)
		return total, err
	}
	return total, nil
}

// cleanup removes a temporary directory from every drive, best effort.
func (e *erasureStore) cleanup(ctx context.Context, dir string) {
	_ = e.each(1, nil, func(_ int, d storage.StorageAPI) error {
		return d.Delete(ctx, domain.SystemBucket, dir, storage.DeleteOptions{Recursive: true})
	})
}

func (e *erasureStore) CommitData(ctx context.Context, tmpDir string, fi storage.FileInfo, volume, path string) (string, error) {
	var (
		mu  sync.Mutex
		old string
	)
	err := e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(i int, d storage.StorageAPI) error {
		res, err := d.RenameData(ctx, domain.SystemBucket, tmpDir, e.withIndex(fi, i), volume, path, storage.RenameOptions{})
		if err != nil {
			return err
		}
		mu.Lock()
		if res.OldDataDir != "" {
			old = res.OldDataDir
		}
		mu.Unlock()
		return nil
	})
	if err != nil {
		if tmpDir != "" {
			e.cleanup(ctx, tmpDir)
		}
		return "", err
	}
	return old, nil
}

// ReadData opens the shard streams of a part and decodes the requested range.
func (e *erasureStore) ReadData(ctx context.Context, volume, path string, fi storage.FileInfo, part storage.PartInfo, offset, length int64) (io.ReadCloser, error) {
	if length <= 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	data, parity, blockSize := fi.Erasure.DataBlocks, fi.Erasure.ParityBlocks, fi.Erasure.BlockSize
	if data <= 0 || parity < 0 || blockSize <= 0 || data+parity != len(e.drives) {
		return nil, fmt.Errorf("%w: %s/%s: erasure layout %d+%d/%d does not match the set of %d drives", domain.ErrInvalidFormat, volume, path, data, parity, blockSize, len(e.drives))
	}
	rs := e.rs
	if data != e.data || parity != e.parity {
		var err error
		if rs, err = reedsolomon.New(data, parity); err != nil {
			return nil, fmt.Errorf("object: erasure decoder: %w", err)
		}
	}
	if offset+length > part.Size {
		return nil, fmt.Errorf("%w: [%d,%d) beyond part of %d bytes", domain.ErrInvalidRange, offset, offset+length, part.Size)
	}
	fullShard := shardSize(blockSize, data)
	startBlock := offset / blockSize
	fileOffset := startBlock * (bitrotHashSize + fullShard)
	dataPath := storage.ObjectDataPath(path, fi.DataDir, part.Number)

	readers := make([]io.ReadCloser, len(e.drives))
	errs := parallel(len(e.drives), func(i int) error {
		d := e.drives[i]
		if d == nil || !d.IsOnline() {
			return fmt.Errorf("%w: set %d drive %d", domain.ErrDiskNotFound, e.setIdx, i)
		}
		rc, err := d.ReadFileStream(ctx, volume, dataPath, fileOffset, -1)
		if err != nil {
			return err
		}
		readers[i] = rc
		return nil
	})
	available := 0
	for _, rc := range readers {
		if rc != nil {
			available++
		}
	}
	if available < data {
		for _, rc := range readers {
			if rc != nil {
				_ = rc.Close()
			}
		}
		if err := reduceErrs(errs, data, nil); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s/%s: %d of %d shards readable", domain.ErrReadQuorum, volume, path, available, data+parity)
	}
	return &erasureReader{
		rs: rs, readers: readers, data: data, blockSize: blockSize, partSize: part.Size,
		block: startBlock, skip: offset - startBlock*blockSize, remaining: length,
		logger: e.logger, volume: volume, path: path,
	}, nil
}

// erasureReader decodes blocks from the shard streams on demand.
type erasureReader struct {
	rs        reedsolomon.Encoder
	readers   []io.ReadCloser
	data      int
	blockSize int64
	partSize  int64
	block     int64
	skip      int64
	remaining int64
	buf       []byte
	logger    *slog.Logger
	volume    string
	path      string
}

func (r *erasureReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		if err := r.decodeNext(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *erasureReader) decodeNext() error {
	blockLen := r.partSize - r.block*r.blockSize
	if blockLen > r.blockSize {
		blockLen = r.blockSize
	}
	if blockLen <= 0 {
		return fmt.Errorf("%w: %s/%s: read beyond the last block", domain.ErrInvalidRange, r.volume, r.path)
	}
	shardLen := shardSize(blockLen, r.data)
	shards := make([][]byte, len(r.readers))
	available := 0
	for i, rc := range r.readers {
		if rc == nil {
			continue
		}
		buf := make([]byte, bitrotHashSize+shardLen)
		if _, err := io.ReadFull(rc, buf); err != nil {
			r.drop(i, "short shard read", err)
			continue
		}
		if !bytes.Equal(bitrotSum(buf[bitrotHashSize:]), buf[:bitrotHashSize]) {
			r.drop(i, "bitrot detected", nil)
			continue
		}
		shards[i] = buf[bitrotHashSize:]
		available++
	}
	if available < r.data {
		return fmt.Errorf("%w: %s/%s: block %d has %d of %d shards", domain.ErrReadQuorum, r.volume, r.path, r.block, available, r.data)
	}
	if available < len(shards) {
		if err := r.rs.ReconstructData(shards); err != nil {
			return fmt.Errorf("%w: %s/%s: reconstruct block %d: %v", domain.ErrReadQuorum, r.volume, r.path, r.block, err)
		}
	}
	out := make([]byte, 0, blockLen)
	for i := 0; i < r.data; i++ {
		out = append(out, shards[i]...)
	}
	out = out[:blockLen]
	if r.skip > 0 {
		if r.skip >= int64(len(out)) {
			out = out[:0]
		} else {
			out = out[r.skip:]
		}
		r.skip = 0
	}
	if int64(len(out)) > r.remaining {
		out = out[:r.remaining]
	}
	r.remaining -= int64(len(out))
	r.buf = out
	r.block++
	return nil
}

func (r *erasureReader) drop(i int, reason string, err error) {
	if r.readers[i] != nil {
		_ = r.readers[i].Close()
		r.readers[i] = nil
	}
	r.logger.Warn("object: shard unavailable", "object", r.volume+"/"+r.path, "drive", i, "reason", reason, "err", err)
}

func (r *erasureReader) Close() error {
	var errs []error
	for i, rc := range r.readers {
		if rc != nil {
			if err := rc.Close(); err != nil {
				errs = append(errs, err)
			}
			r.readers[i] = nil
		}
	}
	return errors.Join(errs...)
}

func (e *erasureStore) ReadAll(ctx context.Context, path string) ([]byte, error) {
	var last error
	for _, d := range e.drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, path)
		if err == nil {
			return data, nil
		}
		if last == nil || !errors.Is(err, domain.ErrObjectNotFound) {
			last = err
		}
	}
	if last == nil {
		last = fmt.Errorf("%w: no drive online", domain.ErrReadQuorum)
	}
	return nil, last
}

func (e *erasureStore) WriteAll(ctx context.Context, path string, data []byte) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(_ int, d storage.StorageAPI) error {
		return d.WriteAll(ctx, domain.SystemBucket, path, data)
	})
}

func (e *erasureStore) DeletePath(ctx context.Context, volume, path string, recursive bool) error {
	errs := parallel(len(e.drives), func(i int) error {
		d := e.drives[i]
		if d == nil || !d.IsOnline() {
			return fmt.Errorf("%w: set %d drive %d", domain.ErrDiskNotFound, e.setIdx, i)
		}
		err := d.Delete(ctx, volume, path, storage.DeleteOptions{Recursive: recursive})
		if errors.Is(err, domain.ErrObjectNotFound) {
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
	if notFound == len(e.drives) {
		return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, volume, path)
	}
	return nil
}

// errNotFoundOK marks a delete that found nothing to remove.
var errNotFoundOK = errors.New("object: nothing to delete")

func (e *erasureStore) RenameFile(ctx context.Context, srcPath, dstPath string) error {
	return e.each(e.writeQuorum(), domain.ErrWriteQuorum, func(_ int, d storage.StorageAPI) error {
		return d.RenameFile(ctx, domain.SystemBucket, srcPath, domain.SystemBucket, dstPath)
	})
}

func (e *erasureStore) StatGlob(ctx context.Context, pattern string) ([]storage.StatInfo, error) {
	var last error
	for _, d := range e.drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, pattern, true)
		if err == nil {
			return infos, nil
		}
		last = err
	}
	if last == nil {
		last = fmt.Errorf("%w: no drive online", domain.ErrReadQuorum)
	}
	return nil, last
}

func (e *erasureStore) Walk(ctx context.Context, opts storage.WalkDirOptions, fn storage.WalkDirFunc) error {
	for _, d := range e.drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		return d.WalkDir(ctx, opts, fn)
	}
	return fmt.Errorf("%w: set %d has no drive online", domain.ErrReadQuorum, e.setIdx)
}
