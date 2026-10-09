package object

import (
	"context"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/service/storage"
)

// OrphanDataDirGrace is how old a data directory that no version
// references must be before the healing removes it.
const OrphanDataDirGrace = 15 * time.Minute

// globEscape escapes the metacharacters of filepath.Match in s: the key
// part of a StatInfoFile pattern matches itself only.
func globEscape(s string) string {
	if !strings.ContainsAny(s, `*?[\`) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// purgeOrphans removes, on every online drive of the set, the data
// directories of key that no version of any drive references and that
// were last modified before cutoff: the data of a replaced version whose
// purge failed, or of a commit interrupted by a crash, which nothing else
// ever reclaims. It runs under the write lock of the key (HealObject):
// no write of the key commits meanwhile. Nothing is removed while a drive
// of the set is offline or cannot tell its versions (its references are
// unknown), and a sub-directory that holds anything but part files is
// never touched. It returns how many directories it removed.
func (e *erasureSet) purgeOrphans(ctx context.Context, bucket, key string, cutoff time.Time) int {
	referenced := map[string]bool{}
	perDrive := make([][]storage.FileInfo, e.n())
	errs := e.onDrives(func(i int, d storage.StorageAPI) error {
		versions, err := driveVersions(ctx, d, bucket, key)
		perDrive[i] = versions
		return err
	})
	for _, err := range errs {
		if err != nil {
			// An offline drive may reference what the others do not.
			return 0
		}
	}
	for _, versions := range perDrive {
		for _, fi := range versions {
			if fi.DataDir != "" {
				referenced[fi.DataDir] = true
			}
		}
	}
	removed := make([]int, e.n())
	_ = e.onDrives(func(i int, d storage.StorageAPI) error {
		entries, err := d.StatInfoFile(ctx, bucket, storage.ObjectDataDir(globEscape(key), "*"), true)
		if err != nil {
			return nil
		}
		for _, en := range entries {
			name := path.Base(en.Name)
			if !en.Dir || referenced[name] || !en.ModTime.Before(cutoff) {
				continue
			}
			if _, err := uuid.Parse(name); err != nil || len(name) != 36 {
				continue
			}
			if !onlyParts(ctx, d, bucket, key, name) {
				continue
			}
			err := d.Delete(ctx, bucket, storage.ObjectDataDir(key, name), storage.DeleteOptions{Recursive: true})
			if err != nil {
				e.logger.WarnContext(ctx, "object: cannot remove an orphan data directory", "bucket", bucket, "key", key,
					"data_dir", name, "drive", i+1, "err", err)
				continue
			}
			removed[i]++
		}
		return nil
	})
	total := 0
	for _, n := range removed {
		total += n
	}
	if total > 0 {
		e.logger.InfoContext(ctx, "object: orphan data directories removed", "bucket", bucket, "key", key, "count", total)
	}
	return total
}

// onlyParts reports whether the data directory name of key holds part
// files only (an empty one included).
func onlyParts(ctx context.Context, d storage.StorageAPI, bucket, key, name string) bool {
	children, err := d.StatInfoFile(ctx, bucket, storage.ObjectDataDir(globEscape(key), name)+"/*", true)
	if err != nil {
		return false
	}
	for _, c := range children {
		if c.Dir || !strings.HasPrefix(path.Base(c.Name), partPrefix) {
			return false
		}
	}
	return true
}
