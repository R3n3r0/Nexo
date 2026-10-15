package cache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Layout of an entry of the disk tier (docs/11 §11.3: "file con header +
// body"): a fixed header, the description of the entry (JSON) and its
// content, both covered by a CRC-32C checked on every read.
//
//	magic "NXC1" | version u8 | flags u8 | reserved u16 | meta length u32 |
//	body length u64 | meta CRC u32 | body CRC u32 | meta | body
const (
	diskMagic      = "NXC1"
	diskVersion    = 1
	diskHeaderSize = 28
	diskExt        = ".nxc"
	diskTmpMarker  = ".tmp-"
	// diskLayout is the directory of the entries of this layout below the
	// cache directory.
	diskLayout = "v1"
	// maxDiskMeta bounds the description read from a file.
	maxDiskMeta = 1 << 20
)

// crcTable is the Castagnoli polynomial (hardware accelerated).
func crcTable() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

// disk is the disk tier: one file per entry, written atomically (temporary
// file renamed), verified on every read.
type disk struct {
	dir    string
	max    int64
	table  *crc32.Table
	logger *slog.Logger
}

// newDisk opens the disk tier in dir.
func newDisk(dir string, maxSize int64, logger *slog.Logger) (*disk, error) {
	root := filepath.Join(dir, diskLayout)
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("cache: disk tier %s: %w", dir, err)
	}
	return &disk{dir: root, max: maxSize, table: crcTable(), logger: logger}, nil
}

// pathOf is the file of a key.
func (d *disk) pathOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(d.dir, h[:2], h+diskExt)
}

// write stores an entry and returns the size of its file.
func (d *disk) write(meta *entryMeta, body []byte) (int64, error) {
	m := *meta
	m.CRC = crc32.Checksum(body, d.table)
	m.Size = int64(len(body))
	mdata, err := json.Marshal(&m)
	if err != nil {
		return 0, err
	}
	hdr := make([]byte, diskHeaderSize)
	copy(hdr, diskMagic)
	hdr[4] = diskVersion
	binary.BigEndian.PutUint32(hdr[8:], uint32(len(mdata))) // #nosec G115 -- the JSON description of an entry, far below 4 GiB
	binary.BigEndian.PutUint64(hdr[12:], uint64(len(body)))
	binary.BigEndian.PutUint32(hdr[20:], crc32.Checksum(mdata, d.table))
	binary.BigEndian.PutUint32(hdr[24:], m.CRC)
	path := d.pathOf(meta.Key)
	n, err := d.writeFile(path, hdr, mdata, body)
	if errors.Is(err, fs.ErrNotExist) {
		// The directory was removed meanwhile: create it again.
		if err = os.MkdirAll(filepath.Dir(path), 0o750); err == nil {
			n, err = d.writeFile(path, hdr, mdata, body)
		}
	}
	return n, err
}

// writeFile writes the parts to a temporary file renamed to path.
func (d *disk) writeFile(path string, parts ...[]byte) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return 0, err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := path + diskTmpMarker + hex.EncodeToString(rnd[:])
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600) // #nosec G304 -- a temporary file under the cache directory, named after a hash
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range parts {
		w, err := f.Write(p)
		n += int64(w)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return 0, err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// errCorrupt reports an entry of the disk tier that fails its checks.
var errCorrupt = errors.New("cache: corrupt disk entry")

// readHeader reads and checks the header and the description of a file.
func (d *disk) readHeader(f io.Reader) (*entryMeta, int64, error) {
	hdr := make([]byte, diskHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, 0, fmt.Errorf("%w: header: %v", errCorrupt, err)
	}
	if string(hdr[:4]) != diskMagic || hdr[4] != diskVersion {
		return nil, 0, fmt.Errorf("%w: unknown format", errCorrupt)
	}
	mlen := binary.BigEndian.Uint32(hdr[8:])
	blen := binary.BigEndian.Uint64(hdr[12:])
	if mlen == 0 || mlen > maxDiskMeta || blen > maxBlockSize {
		return nil, 0, fmt.Errorf("%w: invalid lengths", errCorrupt)
	}
	mdata := make([]byte, mlen)
	if _, err := io.ReadFull(f, mdata); err != nil {
		return nil, 0, fmt.Errorf("%w: description: %v", errCorrupt, err)
	}
	if crc32.Checksum(mdata, d.table) != binary.BigEndian.Uint32(hdr[20:]) {
		return nil, 0, fmt.Errorf("%w: description checksum", errCorrupt)
	}
	var m entryMeta
	if err := json.Unmarshal(mdata, &m); err != nil {
		return nil, 0, fmt.Errorf("%w: description: %v", errCorrupt, err)
	}
	if m.Size != int64(blen) || m.CRC != binary.BigEndian.Uint32(hdr[24:]) {
		return nil, 0, fmt.Errorf("%w: description and header disagree", errCorrupt)
	}
	return &m, int64(diskHeaderSize) + int64(mlen) + int64(blen), nil
}

// read returns the content of the entry of key, checked against its
// description: a missing, truncated or corrupt file is an error (the
// entry is then dropped and the origin serves the request).
func (d *disk) read(key string, meta *entryMeta) ([]byte, error) {
	f, err := os.Open(d.pathOf(key))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	m, _, err := d.readHeader(f)
	if err != nil {
		return nil, err
	}
	if m.Key != key || m.Size != meta.Size || m.CRC != meta.CRC {
		return nil, fmt.Errorf("%w: another entry", errCorrupt)
	}
	body := make([]byte, m.Size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, fmt.Errorf("%w: content: %v", errCorrupt, err)
	}
	if crc32.Checksum(body, d.table) != m.CRC {
		return nil, fmt.Errorf("%w: content checksum", errCorrupt)
	}
	return body, nil
}

// remove deletes the file of key.
func (d *disk) remove(key string) {
	if err := os.Remove(d.pathOf(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Debug("cache: disk entry not removed", "key", key, "err", err)
	}
}

// load walks the disk tier and reports every valid entry; the temporary
// and corrupt files are removed.
func (d *disk) load(fn func(meta *entryMeta, size int64)) error {
	// The removals go through a root of the directory: a parent replaced
	// by a symbolic link during the walk cannot point them outside of it.
	root, err := os.OpenRoot(d.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = root.Close() }()
	remove := func(path string) {
		if rel, err := filepath.Rel(d.dir, path); err == nil {
			_ = root.Remove(rel)
		}
	}
	return filepath.WalkDir(d.dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		name := e.Name()
		if strings.Contains(name, diskTmpMarker) || !strings.HasSuffix(name, diskExt) {
			remove(path)
			return nil
		}
		meta, size, err := d.readMeta(path)
		if err != nil || d.pathOf(meta.Key) != path {
			d.logger.Warn("cache: dropping an unreadable disk entry", "file", path, "err", err)
			remove(path)
			return nil
		}
		if !strings.HasPrefix(meta.Key, keyVersion+"|") {
			// A key of another format is never looked up again.
			d.logger.Debug("cache: dropping a disk entry of another key format", "file", path)
			remove(path)
			return nil
		}
		fn(meta, size)
		return nil
	})
}

// readMeta reads the description of a file and checks its size.
func (d *disk) readMeta(path string) (*entryMeta, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- an entry file under the cache directory (pathOf or the walk of it)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	m, size, err := d.readHeader(f)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if st.Size() != size {
		return nil, 0, fmt.Errorf("%w: truncated", errCorrupt)
	}
	return m, size, nil
}
