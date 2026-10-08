package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
)

// errWalkStop stops a walk once the entry limit is reached.
var errWalkStop = errors.New("storage: walk limit reached")

// WalkDir implements StorageAPI. Object entries are directories holding an
// xl.meta; prefixes are directories holding other entries. Both are emitted
// in S3 key order ("a", "a-b", "a/..."), objects before the prefix of the
// same name, so that callers can build listings without re-sorting.
func (s *xlStorage) WalkDir(ctx context.Context, opts WalkDirOptions, fn WalkDirFunc) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: nil walk function", domain.ErrInvalidArgument)
	}
	volDir, err := s.volumeDir(opts.Bucket)
	if err != nil {
		return err
	}
	if _, err := os.Stat(volDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, opts.Bucket)
		}
		return fmt.Errorf("storage: stat volume %s: %w", opts.Bucket, mapErr(err))
	}
	base := strings.Trim(opts.BaseDir, "/")
	dir, prefix := volDir, ""
	if base != "" {
		if dir, err = s.filePath(opts.Bucket, base); err != nil {
			return err
		}
		prefix = base + "/"
	}
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if opts.ReportNotFound {
				return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, opts.Bucket, base)
			}
			return nil
		}
		return fmt.Errorf("storage: stat %s: %w", dir, mapErr(err))
	}
	w := &walker{ctx: ctx, opts: opts, fn: fn}
	if err := w.walk(dir, prefix); err != nil && !errors.Is(err, errWalkStop) {
		return err
	}
	return nil
}

type walker struct {
	ctx   context.Context
	opts  WalkDirOptions
	fn    WalkDirFunc
	count int
}

// walkItem is a directory entry classified as object and/or prefix.
type walkItem struct {
	// name is the S3 key of the object or the prefix (ending with "/").
	name string
	path string
	// object reports an entry holding an xl.meta; prefix one holding children.
	object bool
}

func (w *walker) walk(dir, keyPrefix string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("storage: read %s: %w", dir, mapErr(err))
	}
	items := make([]walkItem, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		seg, dirObject := keyFromDirName(e.Name())
		hasMeta := fileExists(filepath.Join(sub, MetaFile))
		if dirObject {
			if hasMeta {
				items = append(items, walkItem{name: keyPrefix + seg, path: sub, object: true})
			}
			continue
		}
		if hasMeta {
			// An object directory: its sub-directories are data dirs, not prefixes.
			items = append(items, walkItem{name: keyPrefix + seg, path: sub, object: true})
			continue
		}
		items = append(items, walkItem{name: keyPrefix + seg + "/", path: sub})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].name != items[j].name {
			return items[i].name < items[j].name
		}
		return items[i].object && !items[j].object
	})
	for _, it := range items {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if !w.matches(it) {
			continue
		}
		if it.object {
			raw, err := os.ReadFile(filepath.Join(it.path, MetaFile))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // removed concurrently
				}
				return fmt.Errorf("storage: read %s: %w", it.path, mapErr(err))
			}
			if err := w.fn(WalkEntry{Name: it.name, Metadata: raw}); err != nil {
				return err
			}
			w.count++
			if w.opts.Limit > 0 && w.count >= w.opts.Limit {
				return errWalkStop
			}
			continue
		}
		err := w.fn(WalkEntry{Name: it.name, IsDir: true})
		switch {
		case errors.Is(err, fs.SkipDir):
			continue
		case err != nil:
			return err
		}
		if w.opts.Recursive {
			if err := w.walk(it.path, it.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// matches applies FilterPrefix and ForwardTo to an item. Prefixes pass when
// they may contain matching entries.
func (w *walker) matches(it walkItem) bool {
	if p := w.opts.FilterPrefix; p != "" {
		if it.object {
			if !strings.HasPrefix(it.name, p) {
				return false
			}
		} else if !strings.HasPrefix(it.name, p) && !strings.HasPrefix(p, it.name) {
			return false
		}
	}
	if f := w.opts.ForwardTo; f != "" {
		if it.object {
			if it.name < f {
				return false
			}
		} else if it.name < f && !strings.HasPrefix(f, it.name) {
			return false
		}
	}
	return true
}

func fileExists(name string) bool {
	st, err := os.Stat(name)
	return err == nil && !st.IsDir()
}

// isDataDir reports whether an entry of an object directory is a data
// directory (a UUID) rather than a child prefix.
func isDataDir(name string) bool {
	_, err := uuid.Parse(name)
	return err == nil
}
