package storage

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/btree"

	"github.com/r3n3r0/nexo/internal/domain"
)

// The metadata log of a drive keeps the xl.meta documents of its user
// volumes in append-only segments instead of one file per object
// (docs/proposals/small-object-log.md). A writer goroutine owns the active
// segment: the records waiting when it starts a round are written at once
// and flushed together (group commit), so concurrent writes share one
// flush. An in-memory index per volume (a B-tree ordered by key) locates
// the latest record of every object; it is rebuilt at open by replaying
// the segments. Overwritten and deleted records are garbage that the
// compactor reclaims by copying the live records of a mostly dead segment
// forward, then removing it.
//
// Record layout (little endian):
//
//	0  magic   "NXL1"
//	4  crc32c  over bytes [8, total)
//	8  total   uint32, the record length
//	12 type    put, delete or drop (the whole volume)
//	13 -       reserved
//	14 volLen  uint16
//	16 keyLen  uint16
//	18 -       reserved
//	20 seq     uint64, the sequence number of the change
//	28 volume, key, payload (the encoded xl.meta of a put)
//
// A record keeps its sequence number when the compactor copies it, so
// replay picks, for every key, the record with the highest sequence, and a
// delete or drop hides the older records whatever segment they are in.
const (
	// MetaLogDir is the directory of the metadata log in the system volume.
	MetaLogDir = "metalog"
	// metaLogMarker marks a drive whose metadata is in the log: written
	// once the conversion of its xl.meta files is complete.
	metaLogMarker = "ENABLED"
	// metaLogPrune marks a drive whose user volumes may hold xl.meta files
	// left by an interrupted conversion, stale in log mode.
	metaLogPrune = "PRUNE"

	metaLogSegPrefix = "seg-"
	metaLogSegSuffix = ".log"

	recMagic      = "NXL1"
	recHeaderSize = 28

	recPut    byte = 1
	recDelete byte = 2
	recDrop   byte = 3

	// DefaultMetaLogSegmentSize is the default size of a segment.
	DefaultMetaLogSegmentSize = 64 << 20
	// MinMetaLogSegmentSize and MaxMetaLogSegmentSize bound the segment
	// size (offsets in a segment are 32-bit).
	MinMetaLogSegmentSize = 1 << 20
	MaxMetaLogSegmentSize = 1 << 30

	// maxBatchRecords and maxBatchBytes bound a group commit.
	maxBatchRecords = 1024
	maxBatchBytes   = 32 << 20
	// compactRatio is the live fraction under which a sealed segment is
	// compacted.
	compactRatio = 0.5
	// compactInterval is the pause between two compaction passes.
	compactInterval = 30 * time.Second
	// indexEntryOverhead estimates the memory of an index entry besides
	// its key (the entry, the B-tree node slot, the string header).
	indexEntryOverhead = 72
)

// crcTable is the Castagnoli table of the record checksums.
func crcTable() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

// logEntry is the index entry of an object: where the payload of its
// latest record is.
type logEntry struct {
	key  string
	seg  uint32
	off  uint32 // offset of the payload in the segment
	size uint32 // length of the payload
	meta uint32 // length of the metadata section of the payload
	seq  uint64
}

func entryLess(a, b logEntry) bool { return a.key < b.key }

// recordLen returns the length of the record of an entry of volume.
func (e logEntry) recordLen(volume string) int64 {
	return int64(recHeaderSize + len(volume) + len(e.key) + int(e.size))
}

// logSegment is an open segment file.
type logSegment struct {
	id   uint32
	path string
	f    *os.File
	// size is the length of the valid records (the writer appends there).
	size atomic.Int64
	// live is the length of the records the index points to.
	live atomic.Int64
	// refs counts the readers using f; a retired segment is closed and
	// removed by its last reader.
	refs    atomic.Int32
	retired atomic.Bool
	once    sync.Once
}

// dispose closes and removes a retired segment. The segment counts as on
// disk (hasOlder) until its removal is durable: the deletes it hides older
// records with are dropped by the compaction only once no older segment
// can come back at replay.
func (l *metaLog) dispose(g *logSegment) {
	g.once.Do(func() {
		_ = g.f.Close()
		if err := os.Remove(g.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			l.logger.Warn("storage: cannot remove a compacted metadata log segment", "path", g.path, "err", err)
			return
		}
		if l.fsync {
			syncDirPath(l.dir)
		}
		l.mu.Lock()
		delete(l.files, g.id)
		l.mu.Unlock()
	})
}

// logReq is a change handed to the writer.
type logReq struct {
	typ     byte
	volume  string
	key     string
	payload []byte
	// seq is the sequence of a record copied by the compactor (0: a new
	// change, numbered by the writer).
	seq uint64
	// from is, for a copied put, the entry it must still be the index
	// entry of for the copy to replace it.
	from *logEntry
	done chan error

	entry  logEntry
	recLen int64
}

// metaLog is the metadata log of a drive.
type metaLog struct {
	dir     string
	marker  string
	logger  *slog.Logger
	fsync   bool
	segSize atomic.Int64
	memMax  atomic.Uint64
	table   *crc32.Table

	// mu guards vols, segs and files; the writer holds it to apply a
	// commit.
	mu   sync.RWMutex
	vols map[string]*btree.BTreeG[logEntry]
	segs map[uint32]*logSegment
	// files are the segments whose file may be on disk: the open ones and
	// the retired ones not yet durably removed.
	files map[uint32]struct{}
	// keyBytes sums the keys of the index (memory estimate).
	keyBytes atomic.Int64
	keys     atomic.Int64

	// Writer state: owned by the writer goroutine once started.
	active  *logSegment
	nextSeq uint64
	nextSeg uint32
	broken  error

	reqs    chan *logReq
	stop    chan struct{}
	stopped chan struct{}
	closing sync.Once
	started bool
	warned  atomic.Bool
}

// openMetaLog opens (creating it when missing) the log of dir and replays
// its segments. The writer is not started: convert may commit batches
// first, then start runs it.
func openMetaLog(dir string, segSize int64, fsync bool, logger *slog.Logger) (*metaLog, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create %s: %w", dir, mapErr(err))
	}
	if fsync {
		syncDirPath(filepath.Dir(dir))
	}
	l := &metaLog{
		dir: dir, marker: filepath.Join(dir, metaLogMarker), logger: logger, fsync: fsync, table: crcTable(),
		vols: map[string]*btree.BTreeG[logEntry]{}, segs: map[uint32]*logSegment{}, files: map[uint32]struct{}{},
		reqs: make(chan *logReq, maxBatchRecords), stop: make(chan struct{}), stopped: make(chan struct{}),
		nextSeq: 1, nextSeg: 1,
	}
	l.setSegmentSize(segSize)
	if err := l.replay(); err != nil {
		l.closeFiles()
		return nil, err
	}
	if err := l.roll(); err != nil {
		l.closeFiles()
		return nil, err
	}
	return l, nil
}

// setSegmentSize changes the size of the next segments.
func (l *metaLog) setSegmentSize(n int64) {
	if n <= 0 {
		n = DefaultMetaLogSegmentSize
	}
	l.segSize.Store(min(max(n, MinMetaLogSegmentSize), MaxMetaLogSegmentSize))
}

// start runs the writer and the compactor.
func (l *metaLog) start() {
	l.started = true
	go l.run()
	go l.compactLoop()
}

// close stops the writer and the compactor and closes the segments.
func (l *metaLog) close() {
	l.closing.Do(func() {
		close(l.stop)
		if l.started {
			<-l.stopped
		}
		l.closeFiles()
	})
}

func (l *metaLog) closeFiles() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, g := range l.segs {
		_ = g.f.Close()
	}
}

// --- segments ---------------------------------------------------------------

func segName(id uint32) string {
	return fmt.Sprintf("%s%012d%s", metaLogSegPrefix, id, metaLogSegSuffix)
}

// segmentIDs lists the segment files of the log in id order.
func (l *metaLog) segmentIDs() ([]uint32, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("storage: read %s: %w", l.dir, mapErr(err))
	}
	var ids []uint32
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, metaLogSegPrefix) || !strings.HasSuffix(name, metaLogSegSuffix) {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, metaLogSegPrefix), metaLogSegSuffix), 10, 32)
		if err != nil {
			continue
		}
		ids = append(ids, uint32(n))
	}
	slices.Sort(ids)
	return ids, nil
}

// roll seals the active segment and starts a new one.
func (l *metaLog) roll() error {
	id := l.nextSeg
	path := filepath.Join(l.dir, segName(id))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o640) // #nosec G304 -- a file of the log directory of the drive
	if err != nil {
		return fmt.Errorf("%w: storage: create metadata log segment %s: %w", domain.ErrFaultyDisk, path, err)
	}
	if l.fsync {
		syncDirPath(l.dir)
	}
	g := &logSegment{id: id, path: path, f: f}
	l.mu.Lock()
	l.segs[id] = g
	l.files[id] = struct{}{}
	l.mu.Unlock()
	l.nextSeg++
	l.active = g
	return nil
}

// --- replay -----------------------------------------------------------------

// replay rebuilds the index from the segments: for every key the record
// with the highest sequence wins, a delete hides the older puts and a drop
// the older records of its volume. A damaged area (see scanRecords) loses
// only the records it covers: the reading resumes at the next valid
// record. Damage reaching the end of the last segment is a write torn by a
// crash, never acknowledged, and is cut off; damage elsewhere is logged
// and left in place (the healing of the erasure set restores what it
// held).
func (l *metaLog) replay() error {
	ids, err := l.segmentIDs()
	if err != nil {
		return err
	}
	tomb := map[string]map[string]uint64{}
	drops := map[string]uint64{}
	for i, id := range ids {
		path := filepath.Join(l.dir, segName(id))
		f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- a file of the log directory of the drive
		if err != nil {
			return fmt.Errorf("%w: storage: open metadata log segment %s: %w", domain.ErrFaultyDisk, path, err)
		}
		g := &logSegment{id: id, path: path, f: f}
		l.segs[id] = g
		l.files[id] = struct{}{}
		if err := l.replaySegment(g, i == len(ids)-1, tomb, drops); err != nil {
			return err
		}
		l.nextSeg = id + 1
	}
	// The drops hide the older records of their volume.
	for volume, seq := range drops {
		if t := l.vols[volume]; t != nil {
			var stale []logEntry
			t.Ascend(func(e logEntry) bool {
				if e.seq < seq {
					stale = append(stale, e)
				}
				return true
			})
			for _, e := range stale {
				t.Delete(e)
			}
			if t.Len() == 0 {
				delete(l.vols, volume)
			}
		}
	}
	for volume, t := range l.vols {
		t.Ascend(func(e logEntry) bool {
			if g := l.segs[e.seg]; g != nil {
				g.live.Add(e.recordLen(volume))
			}
			l.keys.Add(1)
			l.keyBytes.Add(int64(len(e.key)))
			return true
		})
	}
	return nil
}

// replaySegment applies the records of a segment; last tells the segment
// written when the drive stopped.
func (l *metaLog) replaySegment(g *logSegment, last bool, tomb map[string]map[string]uint64, drops map[string]uint64) error {
	st, err := g.f.Stat()
	if err != nil {
		return fmt.Errorf("%w: storage: stat %s: %w", domain.ErrFaultyDisk, g.path, err)
	}
	var valid int64
	damage, err := l.scanRecords(g.f, st.Size(), func(off int64, hdr, body []byte) error {
		l.replayRecord(g.id, off, hdr, body, tomb, drops)
		valid = off + int64(len(hdr)+len(body))
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: storage: read %s: %w", domain.ErrFaultyDisk, g.path, err)
	}
	g.size.Store(valid)
	for i, d := range damage {
		if last && i == len(damage)-1 && d.end == st.Size() {
			l.logger.Warn("storage: metadata log: torn write cut off at the end of the log", "segment", g.path, "offset", d.off, "bytes", d.end-d.off, "err", d.err)
			if err := g.f.Truncate(d.off); err != nil {
				return fmt.Errorf("%w: storage: truncate %s: %w", domain.ErrFaultyDisk, g.path, err)
			}
			continue
		}
		l.logger.Error("storage: metadata log: damaged area skipped, the records it held are lost on this drive",
			"segment", g.path, "offset", d.off, "bytes", d.end-d.off, "err", d.err)
	}
	return nil
}

// replayRecord applies a valid record to the index being rebuilt.
func (l *metaLog) replayRecord(seg uint32, off int64, hdr, body []byte, tomb map[string]map[string]uint64, drops map[string]uint64) {
	typ, seq := hdr[12], binary.LittleEndian.Uint64(hdr[20:28])
	volLen, keyLen := int(binary.LittleEndian.Uint16(hdr[14:16])), int(binary.LittleEndian.Uint16(hdr[16:18]))
	volume, key := string(body[:volLen]), string(body[volLen:volLen+keyLen])
	payload := body[volLen+keyLen:]
	pos := off + int64(recHeaderSize+volLen+keyLen)
	switch typ {
	case recPut:
		l.replayPut(volume, logEntry{key: key, seg: seg, off: uint32(pos), size: uint32(len(payload)), // #nosec G115 -- bounded by the segment size
			meta: uint32(metaSectionLen(payload)), seq: seq}, tomb) // #nosec G115 -- bounded by the payload
	case recDelete:
		if t := l.vols[volume]; t != nil {
			if cur, ok := t.Get(logEntry{key: key}); ok && cur.seq < seq {
				t.Delete(cur)
			}
		}
		if tomb[volume] == nil {
			tomb[volume] = map[string]uint64{}
		}
		tomb[volume][key] = max(tomb[volume][key], seq)
	case recDrop:
		drops[volume] = max(drops[volume], seq)
	}
	if seq >= l.nextSeq {
		l.nextSeq = seq + 1
	}
}

// errBadRecord marks bytes of a segment that are not a valid record.
var errBadRecord = errors.New("invalid metadata log record")

// logDamage is an area of a segment holding no valid record.
type logDamage struct {
	off, end int64
	err      error
}

// scanRecords calls fn with every valid record of the first size bytes of
// f, in order: the header and the rest of the record, both valid only
// during the call. Bytes failing the checks of a record (magic, length,
// checksum, type) start a damaged area that ends at the next valid record,
// found by its magic and a matching checksum, or at size: one damaged
// record never hides the ones after it. A read error is returned.
func (l *metaLog) scanRecords(f *os.File, size int64, fn func(off int64, hdr, body []byte) error) ([]logDamage, error) {
	var damage []logDamage
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, size), 1<<20)
	hdr := make([]byte, recHeaderSize)
	for off := int64(0); off < size; {
		body, err := l.nextRecord(r, hdr, size-off)
		if err == nil {
			if err := fn(off, hdr, body); err != nil {
				return damage, err
			}
			off += int64(recHeaderSize + len(body))
			continue
		}
		if !errors.Is(err, errBadRecord) {
			return damage, err
		}
		next, rerr := l.resync(f, off+1, size)
		if rerr != nil {
			return damage, rerr
		}
		damage = append(damage, logDamage{off: off, end: next, err: err})
		off = next
		r.Reset(io.NewSectionReader(f, off, size-off))
	}
	return damage, nil
}

// nextRecord reads the record at the position of r, at most remain bytes
// long, into hdr and the returned body. Invalid or missing bytes are an
// errBadRecord, a failed read is returned as is.
func (l *metaLog) nextRecord(r io.Reader, hdr []byte, remain int64) ([]byte, error) {
	short := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: truncated", errBadRecord)
		}
		return err
	}
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, short(err)
	}
	if string(hdr[:4]) != recMagic {
		return nil, fmt.Errorf("%w: bad magic", errBadRecord)
	}
	total := int64(binary.LittleEndian.Uint32(hdr[8:12]))
	volLen, keyLen := int(binary.LittleEndian.Uint16(hdr[14:16])), int(binary.LittleEndian.Uint16(hdr[16:18]))
	if total < int64(recHeaderSize+volLen+keyLen) || total > remain {
		return nil, fmt.Errorf("%w: bad length %d", errBadRecord, total)
	}
	body := make([]byte, total-recHeaderSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, short(err)
	}
	if crc32.Update(crc32.Checksum(hdr[8:], l.table), l.table, body) != binary.LittleEndian.Uint32(hdr[4:8]) {
		return nil, fmt.Errorf("%w: bad checksum", errBadRecord)
	}
	if typ := hdr[12]; typ != recPut && typ != recDelete && typ != recDrop {
		return nil, fmt.Errorf("%w: unknown type %d", errBadRecord, typ)
	}
	return body, nil
}

// resync returns the offset of the first valid record of f at or after
// from, size when there is none.
func (l *metaLog) resync(f *os.File, from, size int64) (int64, error) {
	const chunk = 1 << 20
	buf := make([]byte, chunk+len(recMagic)-1)
	hdr := make([]byte, recHeaderSize)
	for base := from; base < size; base += chunk {
		n, err := f.ReadAt(buf[:min(int64(len(buf)), size-base)], base)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		for i := 0; ; i++ {
			j := strings.Index(string(buf[i:n]), recMagic)
			if j < 0 {
				break
			}
			i += j
			pos := base + int64(i)
			_, err := l.nextRecord(io.NewSectionReader(f, pos, size-pos), hdr, size-pos)
			if err == nil {
				return pos, nil
			}
			if !errors.Is(err, errBadRecord) {
				return 0, err
			}
		}
	}
	return size, nil
}

func (l *metaLog) replayPut(volume string, e logEntry, tomb map[string]map[string]uint64) {
	if seq, ok := tomb[volume][e.key]; ok && seq > e.seq {
		return
	}
	t := l.vols[volume]
	if t == nil {
		t = btree.NewG(32, entryLess)
		l.vols[volume] = t
	}
	if cur, ok := t.Get(e); ok && cur.seq > e.seq {
		return
	}
	t.ReplaceOrInsert(e)
}

// metaSectionLen returns the length of the header and metadata section of
// an encoded xl.meta (all of it for a legacy or undecodable document).
func metaSectionLen(raw []byte) int {
	if len(raw) < xlMetaHeaderSize || string(raw[:4]) != xlMetaMagic {
		return len(raw)
	}
	need := xlMetaHeaderSize + int(binary.BigEndian.Uint32(raw[8:12])) + xlMetaCRCSize
	if need > len(raw) {
		return len(raw)
	}
	return need
}

// --- writer -----------------------------------------------------------------

// submit hands a change to the writer and waits until it is durable.
func (l *metaLog) submit(r *logReq) error {
	if err := l.enqueue(r); err != nil {
		return err
	}
	return l.wait(r)
}

// enqueue hands a change to the writer without waiting.
func (l *metaLog) enqueue(r *logReq) error {
	r.done = make(chan error, 1)
	select {
	case l.reqs <- r:
		return nil
	case <-l.stop:
		return fmt.Errorf("%w: the metadata log is closed", domain.ErrDiskNotFound)
	}
}

// wait waits until an enqueued change is durable.
func (l *metaLog) wait(r *logReq) error {
	select {
	case err := <-r.done:
		return err
	case <-l.stopped:
		// The writer stopped: the request was answered or never will be.
		select {
		case err := <-r.done:
			return err
		default:
			return fmt.Errorf("%w: the metadata log is closed", domain.ErrDiskNotFound)
		}
	}
}

// put records the xl.meta of an object.
func (l *metaLog) put(volume, key string, payload []byte) error {
	return l.submit(&logReq{typ: recPut, volume: volume, key: key, payload: payload})
}

// remove records that an object has no version left.
func (l *metaLog) remove(volume, key string) error {
	return l.submit(&logReq{typ: recDelete, volume: volume, key: key})
}

// drop records that a volume was removed with its objects.
func (l *metaLog) drop(volume string) error {
	return l.submit(&logReq{typ: recDrop, volume: volume})
}

// run is the writer: every round commits the requests waiting at once.
func (l *metaLog) run() {
	defer close(l.stopped)
	for {
		var first *logReq
		select {
		case first = <-l.reqs:
		case <-l.stop:
			return
		}
		batch := []*logReq{first}
		n := len(first.payload)
	collect:
		for len(batch) < maxBatchRecords && n < maxBatchBytes {
			select {
			case r := <-l.reqs:
				batch = append(batch, r)
				n += len(r.payload)
			default:
				break collect
			}
		}
		err := l.commit(batch)
		for _, r := range batch {
			r.done <- err
		}
	}
}

// commit writes and flushes a batch, then applies it to the index.
func (l *metaLog) commit(batch []*logReq) error {
	if l.broken != nil {
		return l.broken
	}
	segSize := l.segSize.Load()
	var buf []byte
	start := l.active.size.Load()
	for _, r := range batch {
		recLen := int64(recHeaderSize + len(r.volume) + len(r.key) + len(r.payload))
		if start+int64(len(buf)) > 0 && start+int64(len(buf))+recLen > segSize {
			if err := l.flush(buf); err != nil {
				return err
			}
			buf = buf[:0]
			if err := l.roll(); err != nil {
				l.broken = err
				return err
			}
			start = 0
		}
		if r.seq == 0 {
			r.seq = l.nextSeq
			l.nextSeq++
		}
		pos := start + int64(len(buf)) + int64(recHeaderSize+len(r.volume)+len(r.key))
		r.recLen = recLen
		r.entry = logEntry{key: r.key, seg: l.active.id, off: uint32(pos), size: uint32(len(r.payload)), // #nosec G115 -- bounded by the segment size
			meta: uint32(metaSectionLen(r.payload)), seq: r.seq} // #nosec G115 -- bounded by the payload
		buf = l.appendRecord(buf, r)
	}
	if err := l.flush(buf); err != nil {
		return err
	}
	l.apply(batch)
	return nil
}

func (l *metaLog) appendRecord(buf []byte, r *logReq) []byte {
	start := len(buf)
	buf = append(buf, recMagic...)
	buf = binary.LittleEndian.AppendUint32(buf, 0)                // crc, set below
	buf = binary.LittleEndian.AppendUint32(buf, uint32(r.recLen)) // #nosec G115 -- bounded by MaxMetaLogSegmentSize
	buf = append(buf, r.typ, 0)
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(r.volume))) // #nosec G115 -- volume names are short
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(r.key)))    // #nosec G115 -- keys are at most 1024 bytes
	buf = append(buf, 0, 0)
	buf = binary.LittleEndian.AppendUint64(buf, r.seq)
	buf = append(buf, r.volume...)
	buf = append(buf, r.key...)
	buf = append(buf, r.payload...)
	binary.LittleEndian.PutUint32(buf[start+4:], crc32.Checksum(buf[start+8:], l.table))
	return buf
}

// flush appends buf to the active segment and makes it durable. On failure
// the segment is cut back to its previous length.
func (l *metaLog) flush(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	g := l.active
	size := g.size.Load()
	if _, err := g.f.WriteAt(buf, size); err != nil {
		_ = g.f.Truncate(size)
		return fmt.Errorf("storage: write metadata log: %w", mapErr(err))
	}
	if l.fsync {
		if err := datasync(g.f); err != nil {
			_ = g.f.Truncate(size)
			l.broken = fmt.Errorf("%w: storage: flush metadata log: %w", domain.ErrFaultyDisk, err)
			return l.broken
		}
	}
	g.size.Store(size + int64(len(buf)))
	return nil
}

// apply updates the index with a committed batch.
func (l *metaLog) apply(batch []*logReq) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range batch {
		switch r.typ {
		case recPut:
			t := l.vols[r.volume]
			if t == nil {
				t = btree.NewG(32, entryLess)
				l.vols[r.volume] = t
			}
			cur, exists := t.Get(r.entry)
			if r.from != nil && (!exists || cur.seg != r.from.seg || cur.off != r.from.off) {
				continue // a copy of a record replaced meanwhile: garbage
			}
			if exists && cur.seq > r.entry.seq {
				continue
			}
			t.ReplaceOrInsert(r.entry)
			l.account(r.volume, cur, exists, -1)
			l.account(r.volume, r.entry, true, 1)
		case recDelete:
			t := l.vols[r.volume]
			if t == nil {
				continue
			}
			if cur, ok := t.Get(logEntry{key: r.key}); ok && cur.seq < r.seq {
				t.Delete(cur)
				l.account(r.volume, cur, true, -1)
			}
		case recDrop:
			t := l.vols[r.volume]
			if t == nil {
				continue
			}
			var stale []logEntry
			t.Ascend(func(e logEntry) bool {
				if e.seq < r.seq {
					stale = append(stale, e)
				}
				return true
			})
			for _, e := range stale {
				t.Delete(e)
				l.account(r.volume, e, true, -1)
			}
			if t.Len() == 0 {
				delete(l.vols, r.volume)
			}
		}
	}
}

// account adds (sign 1) or removes (sign -1) an entry from the live bytes
// of its segment and from the index size; mu is held.
func (l *metaLog) account(volume string, e logEntry, exists bool, sign int64) {
	if !exists {
		return
	}
	if g := l.segs[e.seg]; g != nil {
		g.live.Add(sign * e.recordLen(volume))
	}
	l.keys.Add(sign)
	l.keyBytes.Add(sign * int64(len(e.key)))
	if sign > 0 {
		l.checkMemory()
	}
}

// checkMemory warns once when the index outgrows its memory budget.
func (l *metaLog) checkMemory() {
	limit := l.memMax.Load()
	if limit == 0 || l.warned.Load() {
		return
	}
	if used := l.indexBytes(); used > limit && l.warned.CompareAndSwap(false, true) {
		l.logger.Warn("storage: the metadata log index exceeds its memory budget", "dir", l.dir, "bytes", used, "budget", limit, "objects", l.keys.Load())
	}
}

// indexBytes estimates the memory of the index.
func (l *metaLog) indexBytes() uint64 {
	n := l.keyBytes.Load() + l.keys.Load()*indexEntryOverhead
	return uint64(max(n, 0)) // #nosec G115 -- never negative
}

// --- reads ------------------------------------------------------------------

// lookup returns the entry of an object and pins its segment; release it.
func (l *metaLog) lookup(volume, key string) (logEntry, *logSegment, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.vols[volume]
	if t == nil {
		return logEntry{}, nil, false
	}
	e, ok := t.Get(logEntry{key: key})
	if !ok {
		return logEntry{}, nil, false
	}
	g := l.segs[e.seg]
	if g == nil {
		return logEntry{}, nil, false
	}
	g.refs.Add(1)
	return e, g, true
}

func (l *metaLog) release(g *logSegment) {
	if g.refs.Add(-1) == 0 && g.retired.Load() {
		l.dispose(g)
	}
}

// has reports whether an object of volume has a record.
func (l *metaLog) has(volume, key string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.vols[volume]
	return t != nil && t.Has(logEntry{key: key})
}

// read returns the xl.meta of an object (only its metadata section unless
// full); fs.ErrNotExist when the log holds none.
func (l *metaLog) read(volume, key string, full bool) ([]byte, error) {
	e, g, ok := l.lookup(volume, key)
	if !ok {
		return nil, fs.ErrNotExist
	}
	defer l.release(g)
	n := e.size
	if !full {
		n = e.meta
	}
	buf := make([]byte, n)
	if _, err := g.f.ReadAt(buf, int64(e.off)); err != nil {
		return nil, fmt.Errorf("%w: storage: read metadata log %s: %w", ErrFileCorrupt, g.path, err)
	}
	return buf, nil
}

// hasChildren reports whether a key below prefix (other than the
// directory object named prefix) has a record.
func (l *metaLog) hasChildren(volume, prefix string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.vols[volume]
	if t == nil {
		return false
	}
	found := false
	t.AscendGreaterOrEqual(logEntry{key: prefix}, func(e logEntry) bool {
		if !strings.HasPrefix(e.key, prefix) {
			return false
		}
		if e.key == prefix {
			return true
		}
		found = true
		return false
	})
	return found
}

// empty reports whether volume has no object in the log.
func (l *metaLog) empty(volume string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.vols[volume]
	return t == nil || t.Len() == 0
}

// scan returns up to n keys of volume from start (inclusive) on.
func (l *metaLog) scan(volume, start string, n int) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	t := l.vols[volume]
	if t == nil {
		return nil
	}
	out := make([]string, 0, n)
	t.AscendGreaterOrEqual(logEntry{key: start}, func(e logEntry) bool {
		out = append(out, e.key)
		return len(out) < n
	})
	return out
}

// volumes returns the volumes with objects in the log.
func (l *metaLog) volumes() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.vols))
	for v := range l.vols {
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// --- compaction -------------------------------------------------------------

func (l *metaLog) compactLoop() {
	t := time.NewTicker(compactInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if _, err := l.compact(); err != nil {
				l.logger.Warn("storage: metadata log compaction failed", "dir", l.dir, "err", err)
			}
		}
	}
}

// compact compacts the sealed segments whose live fraction is below
// compactRatio and returns how many it removed.
func (l *metaLog) compact() (int, error) {
	n := 0
	for {
		g := l.candidate()
		if g == nil {
			return n, nil
		}
		if err := l.compactSegment(g); err != nil {
			return n, err
		}
		n++
		select {
		case <-l.stop:
			return n, nil
		default:
		}
	}
}

// candidate returns the sealed segment with the least live fraction under
// compactRatio, nil when none.
func (l *metaLog) candidate() *logSegment {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var best *logSegment
	bestRatio := compactRatio
	for _, g := range l.segs {
		if g == l.activeSegment() || g.retired.Load() {
			continue
		}
		size := g.size.Load()
		if size == 0 {
			return g
		}
		if r := float64(g.live.Load()) / float64(size); r < bestRatio {
			best, bestRatio = g, r
		}
	}
	return best
}

// activeSegment returns the segment the writer appends to: the newest one
// (roll registers it under mu); mu is held.
func (l *metaLog) activeSegment() *logSegment {
	highest := uint32(0)
	var out *logSegment
	for id, g := range l.segs {
		if id >= highest {
			highest, out = id, g
		}
	}
	return out
}

// compactSegment copies the live records of g forward and retires it.
// Deletes and drops are copied while an older segment remains (they hide
// its records at replay). The copies are queued in chunks, so that they
// share the flushes of the writer; a copied put replaces the index entry
// only if it is still the one it was copied from (apply), so a write of
// the object meanwhile wins.
func (l *metaLog) compactSegment(g *logSegment) error {
	var pending []*logReq
	wait := func() error {
		var first error
		for _, q := range pending {
			if err := l.wait(q); err != nil && first == nil {
				first = err
			}
		}
		pending = pending[:0]
		return first
	}
	_, err := l.scanRecords(g.f, g.size.Load(), func(off int64, hdr, rec []byte) error {
		typ, seq := hdr[12], binary.LittleEndian.Uint64(hdr[20:28])
		volLen, keyLen := int(binary.LittleEndian.Uint16(hdr[14:16])), int(binary.LittleEndian.Uint16(hdr[16:18]))
		body := slices.Clone(rec) // queued beyond the call
		volume, key := string(body[:volLen]), string(body[volLen:volLen+keyLen])
		payloadOff := uint32(off) + uint32(recHeaderSize+volLen+keyLen) // #nosec G115 -- bounded by the segment size
		var q *logReq
		switch typ {
		case recPut:
			if cur, ok := l.entry(volume, key); ok && cur.seg == g.id && cur.off == payloadOff {
				q = &logReq{typ: recPut, volume: volume, key: key, payload: body[volLen+keyLen:], seq: seq, from: &cur}
			}
		case recDelete, recDrop:
			if l.hasOlder(g.id) {
				q = &logReq{typ: typ, volume: volume, key: key, seq: seq}
			}
		}
		if q == nil {
			return nil
		}
		if err := l.enqueue(q); err != nil {
			return err
		}
		pending = append(pending, q)
		if len(pending) >= maxBatchRecords {
			return wait()
		}
		return nil
	})
	if werr := wait(); err == nil {
		err = werr
	}
	if err != nil {
		return fmt.Errorf("storage: compact %s: %w", g.path, err)
	}
	l.mu.Lock()
	delete(l.segs, g.id)
	g.retired.Store(true)
	refs := g.refs.Load()
	l.mu.Unlock()
	if refs == 0 {
		l.dispose(g)
	}
	return nil
}

// entry returns the index entry of an object.
func (l *metaLog) entry(volume, key string) (logEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if t := l.vols[volume]; t != nil {
		return t.Get(logEntry{key: key})
	}
	return logEntry{}, false
}

// hasOlder reports whether a segment older than id may still be read at
// replay: open, or retired but not yet durably removed.
func (l *metaLog) hasOlder(id uint32) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for other := range l.files {
		if other < id {
			return true
		}
	}
	return false
}

// --- statistics ---------------------------------------------------------------

// MetadataLogStats describe the metadata log of a drive.
type MetadataLogStats struct {
	Enabled     bool
	Segments    int
	Bytes       int64
	LiveBytes   int64
	Objects     int64
	IndexBytes  uint64
	IndexBudget uint64
	SegmentSize int64
}

func (l *metaLog) stats() MetadataLogStats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	st := MetadataLogStats{Enabled: true, Segments: len(l.segs), Objects: l.keys.Load(), IndexBytes: l.indexBytes(),
		IndexBudget: l.memMax.Load(), SegmentSize: l.segSize.Load()}
	for _, g := range l.segs {
		st.Bytes += g.size.Load()
		st.LiveBytes += g.live.Load()
	}
	return st
}

// syncDirPath flushes a directory entry list (best effort).
func syncDirPath(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- a directory of the drive
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
