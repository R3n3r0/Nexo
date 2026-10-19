package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/r3n3r0/nexo/internal/domain"
)

// readMeta reads and decodes the xl.meta of an object directory. Without
// withData only its metadata section is read. File system errors are
// returned unmapped so that callers can tell a missing object apart.
func (s *xlStorage) readMeta(dir string, withData bool) (*xlMeta, error) {
	name := filepath.Join(dir, MetaFile)
	var raw []byte
	var err error
	if withData {
		raw, err = readFile(name)
	} else {
		raw, err = readMetaSection(name)
	}
	if err != nil {
		return nil, err
	}
	return decodeXLMeta(raw, withData)
}

// readMetaOrNew reads the xl.meta of an object directory with its data,
// or returns an empty document when the object does not exist yet. An
// xl.meta that cannot be decoded (bitrot of the metadata) is moved into the
// quarantine area and the object starts over with an empty document: the
// write that called it records its version instead of failing on this
// drive for ever, and the healing restores the other versions from the
// drives of the set (on a single drive they were unreadable anyway; the
// quarantined copy is kept for inspection).
func (s *xlStorage) readMetaOrNew(volume, objPath, dir string) (*xlMeta, error) {
	meta, err := s.readMeta(dir, true)
	switch {
	case err == nil:
		return meta, nil
	case errors.Is(err, fs.ErrNotExist):
		return &xlMeta{}, nil
	case errors.Is(err, domain.ErrInvalidFormat):
		if qerr := s.quarantineMeta(volume, objPath, dir, err); qerr != nil {
			return nil, qerr
		}
		return &xlMeta{}, nil
	default:
		return nil, s.metaErr(volume, objPath, err)
	}
}

// quarantineMeta moves the undecodable xl.meta of an object directory into
// the quarantine area of the drive (QuarantineDir), named after the time
// and a random suffix, and logs where it went.
func (s *xlStorage) quarantineMeta(volume, objPath, dir string, cause error) error {
	qdir := filepath.Join(s.root, domain.SystemBucket, QuarantineDir)
	if err := s.mkdirAll(qdir, s.root); err != nil {
		return fmt.Errorf("storage: create %s: %w", qdir, mapErr(err))
	}
	dst := filepath.Join(qdir, s.deps.Clock.Now().UTC().Format("20060102T150405.000000000Z")+"-"+randomSuffix()+"."+MetaFile)
	if err := s.rename(filepath.Join(dir, MetaFile), dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // moved by a concurrent write
		}
		return fmt.Errorf("storage: quarantine %s/%s: %w", volume, objPath, s.writeErr(volume, objPath, err))
	}
	s.syncDir(dir)
	s.syncDir(qdir)
	s.deps.Logger.Warn("storage: corrupt xl.meta moved to quarantine, the object starts over on this drive",
		"bucket", volume, "key", objPath, "drive", s.root, "quarantined", dst, "err", cause)
	return nil
}

// metaErr maps an error of readMeta.
func (s *xlStorage) metaErr(volume, objPath string, err error) error {
	if errors.Is(err, domain.ErrInvalidFormat) {
		return fmt.Errorf("storage: %s/%s: %w", volume, objPath, err)
	}
	return s.fsErr(volume, objPath, err)
}

// writeMeta encodes meta and writes it atomically as the xl.meta of dir.
func (s *xlStorage) writeMeta(dir, volDir string, meta *xlMeta, sync bool) error {
	data, err := meta.encode()
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, MetaFile)
	// Staged in the object directory when it exists (a rename within the
	// directory, see RenameData), through the temporary area otherwise.
	staged, err := s.writeTempIn(dir, data, sync)
	if errors.Is(err, fs.ErrNotExist) {
		err = s.writeAtomic(dst, volDir, data, sync)
	} else if err == nil {
		if err = s.rename(staged, dst); err != nil {
			_ = os.Remove(staged)
			err = mapErr(err)
		} else if sync {
			s.syncDir(dir)
		}
	}
	if err != nil {
		return fmt.Errorf("storage: write %s: %w", MetaFile, s.writeErr(filepath.Base(volDir), "", err))
	}
	return nil
}

// readMetaSection reads the header and metadata section of an xl.meta,
// leaving the inline data of small objects on disk. Legacy JSON documents
// are read whole.
func readMetaSection(name string) ([]byte, error) {
	f, st, err := openFile(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	const probe = 4 << 10
	buf := make([]byte, min(st.Size(), probe))
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	if isLegacyXLMeta(buf) {
		rest, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return append(buf, rest...), nil
	}
	if len(buf) < xlMetaHeaderSize || string(buf[:4]) != xlMetaMagic {
		return buf, nil // let the decoder report it
	}
	need := int64(xlMetaHeaderSize) + int64(binary.BigEndian.Uint32(buf[8:12])) + xlMetaCRCSize
	if need <= int64(len(buf)) {
		return buf[:need], nil
	}
	if need > st.Size() || need > xlMetaMaxMetaSize+xlMetaHeaderSize+xlMetaCRCSize {
		return buf, nil // truncated or corrupt: let the decoder report it
	}
	full := make([]byte, need)
	copy(full, buf)
	if _, err := io.ReadFull(f, full[len(buf):]); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFileCorrupt, err)
	}
	return full, nil
}
