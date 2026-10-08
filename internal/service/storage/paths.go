package storage

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
)

// xlDirSuffix replaces the trailing slash of "directory object" keys
// ("photos/") on disk, so that the object directory does not collide with
// the prefix of the same name. Keys whose segments end with it are refused.
const xlDirSuffix = "__XLDIR__"

// volumeDir validates a volume name and returns its directory.
func (s *xlStorage) volumeDir(volume string) (string, error) {
	if volume == "" || volume == "." || volume == ".." || strings.ContainsAny(volume, "/\\\x00") ||
		strings.ContainsRune(volume, os.PathSeparator) {
		return "", fmt.Errorf("%w: invalid volume name %q", domain.ErrInvalidBucketName, volume)
	}
	return filepath.Join(s.root, volume), nil
}

// filePath validates a slash-separated path inside a volume and returns the
// corresponding file system path. Empty, "." and ".." segments are refused
// (a trailing slash is tolerated) and no existing component may be a
// symbolic link, so the result always lies inside the drive.
func (s *xlStorage) filePath(volume, p string) (string, error) {
	dir, err := s.volumeDir(volume)
	if err != nil {
		return "", err
	}
	if err := validatePath(p); err != nil {
		return "", err
	}
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := s.checkNoSymlinks(full); err != nil {
		return "", err
	}
	return full, nil
}

// validatePath checks the syntax of a path relative to a volume.
func validatePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: invalid path %q", domain.ErrInvalidObjectName, p)
	}
	for _, seg := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." || (os.PathSeparator != '/' && strings.ContainsRune(seg, os.PathSeparator)) {
			return fmt.Errorf("%w: invalid path %q", domain.ErrInvalidObjectName, p)
		}
	}
	return nil
}

// objectDir returns the directory holding the xl.meta of an object key.
// Keys ending with a slash are stored under a "__XLDIR__" suffix.
func (s *xlStorage) objectDir(volume, key string) (string, error) {
	if err := validatePath(key); err != nil {
		return "", err
	}
	for _, seg := range strings.Split(strings.TrimSuffix(key, "/"), "/") {
		if strings.HasSuffix(seg, xlDirSuffix) {
			return "", fmt.Errorf("%w: key %q uses the reserved suffix %s", domain.ErrInvalidObjectName, key, xlDirSuffix)
		}
	}
	return s.filePath(volume, onDiskKey(key))
}

// checkNoSymlinks refuses a path whose existing components below the
// drive root include a symbolic link. Missing components are fine: the
// drive creates them as real directories.
func (s *xlStorage) checkNoSymlinks(full string) error {
	rel, err := filepath.Rel(s.root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: path %q is outside the drive", domain.ErrInvalidObjectName, full)
	}
	cur := s.root
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		cur += string(filepath.Separator) + seg
		st, err := os.Lstat(cur)
		if err != nil {
			return nil // missing (or unreadable: the operation reports it)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errSymlink, filepath.ToSlash(rel))
		}
		if !st.IsDir() {
			return nil
		}
	}
	return nil
}

// onDiskKey maps an object key to its directory path relative to the volume.
func onDiskKey(key string) string {
	if strings.HasSuffix(key, "/") {
		return strings.TrimSuffix(key, "/") + xlDirSuffix
	}
	return key
}

// ObjectDataPath returns the path, relative to the volume, of a part file of
// an object version: "<key>/<dataDir>/part.N". Directory objects ("dir/")
// keep their data under the suffixed directory used for their metadata.
func ObjectDataPath(key, dataDir string, part int) string {
	return onDiskKey(key) + "/" + dataDir + "/" + partName(part)
}

// ObjectDataDir returns the path, relative to the volume, of the data
// directory of an object version (see ObjectDataPath).
func ObjectDataDir(key, dataDir string) string {
	return onDiskKey(key) + "/" + dataDir
}

// keyFromDirName converts an on-disk object directory name back to the key
// segment it represents.
func keyFromDirName(name string) (segment string, dirObject bool) {
	if strings.HasSuffix(name, xlDirSuffix) {
		return strings.TrimSuffix(name, xlDirSuffix) + "/", true
	}
	return name, false
}

// isDataDir reports whether an entry of an object directory is a data
// directory (a UUID) rather than a child prefix.
func isDataDir(name string) bool {
	if len(name) != 36 {
		return false
	}
	_, err := uuid.Parse(name)
	return err == nil
}

// isDataDirOnDisk reports whether dir, a sub-directory of an object
// directory, is a data directory: a UUID name holding only files (the
// part.N of a version). A UUID-named directory holding an xl.meta or any
// sub-directory is a child object or prefix ("a/<uuid>/b"), never data.
// Errors (fs.ErrNotExist included) leave the directory unclassified.
func isDataDirOnDisk(dir string) (bool, error) {
	if !isDataDir(filepath.Base(dir)) {
		return false, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == MetaFile {
			return false, nil
		}
	}
	return true, nil
}

func partName(n int) string { return fmt.Sprintf("part.%d", n) }

// regularFileExists reports whether name is a regular file (links are not
// followed).
func regularFileExists(name string) bool {
	st, err := os.Lstat(name)
	return err == nil && st.Mode().IsRegular()
}
