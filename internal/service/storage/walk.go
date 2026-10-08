package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/r3n3r0/nexo/internal/domain"
)

// errWalkLimit stops a walk once the entry limit is reached.
var errWalkLimit = errors.New("storage: walk limit reached")

// WalkDir implements StorageAPI. Object entries are directories holding an
// xl.meta; prefixes are directories with at least one object below them
// (empty directories and orphan data directories are skipped). Both are
// emitted in S3 key order ("a", "a-b", "a/..."; an object before the prefix
// of the same name), so that callers can build listings without
// re-sorting. Each directory is read when the walk reaches it and an
// entry is classified (one stat) only when emitted or when its kind
// decides the order, so a walk stopped early (Limit, fs.SkipAll, a
// canceled context) does little I/O. Without Recursive, prefixes are
// reported but not entered: they are the common prefixes of a listing with
// the "/" delimiter. Object entries carry the metadata section of their
// xl.meta, which DecodeFileInfos accepts.
func (s *xlStorage) WalkDir(ctx context.Context, opts WalkDirOptions, fn WalkDirFunc) error {
	if err := s.checkOpen(ctx); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: nil walk function", domain.ErrInvalidArgument)
	}
	volDir, err := s.ensureVolume(opts.Bucket)
	if err != nil {
		return err
	}
	base := strings.Trim(opts.BaseDir, "/")
	dir, keyPrefix := volDir, ""
	if base != "" {
		if dir, err = s.filePath(opts.Bucket, base); err != nil {
			return err
		}
		keyPrefix = base + "/"
	}
	if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return s.fsErr(opts.Bucket, base, err)
		}
		if opts.ReportNotFound {
			return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, opts.Bucket, base)
		}
		return nil
	}
	w := &walker{ctx: ctx, opts: opts, fn: fn, volume: opts.Bucket, s: s}
	err = w.walkDir(dir, keyPrefix)
	switch {
	case err == nil, errors.Is(err, errWalkLimit), errors.Is(err, fs.SkipAll), errors.Is(err, fs.SkipDir):
		return nil
	default:
		return err
	}
}

type walker struct {
	ctx    context.Context
	opts   WalkDirOptions
	fn     WalkDirFunc
	volume string
	s      *xlStorage
	count  int
	// err is the first I/O error met while classifying entries.
	err error
}

// walkKind classifies a directory entry.
type walkKind uint8

const (
	kindUnknown walkKind = iota
	kindObject
	kindPrefix
	kindSkip
)

// walkItem is a sub-directory of the directory being walked.
type walkItem struct {
	// base is the key the entry stands for: the object key, or the prefix
	// without its trailing slash. Directory objects ("x__XLDIR__") have a
	// base ending with the slash.
	base      string
	path      string
	dirObject bool
	kind      walkKind
}

func (w *walker) walkDir(dir, keyPrefix string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed concurrently
		}
		return w.s.fsErr(w.volume, keyPrefix, err)
	}
	items := make([]*walkItem, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() { // files and symbolic links are never entries
			continue
		}
		seg, dirObject := keyFromDirName(e.Name())
		it := &walkItem{base: keyPrefix + seg, path: filepath.Join(dir, e.Name()), dirObject: dirObject}
		if w.mayMatch(it) {
			items = append(items, it)
		}
	}
	slices.SortStableFunc(items, w.compare)
	if w.err != nil {
		return w.err
	}
	for _, it := range items {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		switch w.classify(it) {
		case kindObject:
			if !w.matchObject(it.base) {
				continue
			}
			raw, err := readMetaSection(filepath.Join(it.path, MetaFile))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // removed concurrently
				}
				return w.s.fsErr(w.volume, it.base, err)
			}
			err = w.fn(WalkEntry{Name: it.base, Metadata: raw})
			if errors.Is(err, fs.SkipDir) {
				return nil // skip the remaining entries of this directory
			}
			if err != nil {
				return err
			}
			w.count++
			if w.opts.Limit > 0 && w.count >= w.opts.Limit {
				return errWalkLimit
			}
		case kindPrefix:
			name := it.base + "/"
			if !w.matchPrefix(name) {
				continue
			}
			err := w.fn(WalkEntry{Name: name, IsDir: true})
			if errors.Is(err, fs.SkipDir) {
				continue
			}
			if err != nil {
				return err
			}
			if w.opts.Recursive {
				if err := w.walkDir(it.path, name); err != nil {
					return err
				}
			}
		}
		if w.err != nil {
			return w.err
		}
	}
	return nil
}

// classify determines (once) whether an entry is an object, a prefix or
// nothing to report.
func (w *walker) classify(it *walkItem) walkKind {
	if it.kind != kindUnknown {
		return it.kind
	}
	switch {
	case regularFileExists(filepath.Join(it.path, MetaFile)):
		it.kind = kindObject
	case it.dirObject:
		it.kind = kindSkip
	default:
		ok, err := hasObjects(it.path)
		if err != nil && w.err == nil && !errors.Is(err, fs.ErrNotExist) {
			w.err = w.s.fsErr(w.volume, it.base, err)
		}
		if ok {
			it.kind = kindPrefix
		} else {
			it.kind = kindSkip
		}
	}
	return it.kind
}

// hasObjects reports whether the tree below dir holds at least one object,
// so that empty directories and orphan data directories (files only) are
// never reported as prefixes. The search is depth-first and stops at the
// first object, which is usually the first entry.
func hasObjects(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	for {
		entries, err := f.ReadDir(64)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if regularFileExists(filepath.Join(sub, MetaFile)) {
				return true, nil
			}
			if found, serr := hasObjects(sub); found || (serr != nil && !errors.Is(serr, fs.ErrNotExist)) {
				return found, serr
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// compare orders two entries of the same directory by the key they stand
// for: the object key, or the prefix with its trailing slash. The kind of
// an entry only matters when the base of one entry is a prefix of the
// other followed by a byte not above '/' ("a" vs "a-b", "a" vs "a/"), so
// most comparisons need no I/O.
func (w *walker) compare(a, b *walkItem) int {
	if c, ok := w.orderPrefixed(a, b); ok {
		return c
	}
	if c, ok := w.orderPrefixed(b, a); ok {
		return -c
	}
	return strings.Compare(a.base, b.base)
}

// orderPrefixed orders a before or after b when a.base is a proper prefix
// of b.base; ok is false otherwise.
func (w *walker) orderPrefixed(a, b *walkItem) (int, bool) {
	if len(a.base) >= len(b.base) || !strings.HasPrefix(b.base, a.base) {
		return 0, false
	}
	next := b.base[len(a.base)]
	if next > '/' || a.dirObject || w.classify(a) != kindPrefix {
		return -1, true // "a" (or "a/" before "a/x") sorts first
	}
	// a stands for a.base+"/": after b when b continues with a byte below
	// '/', and after the directory object "a/" of the same name.
	return 1, true
}

// mayMatch filters entries on their name before any I/O: an entry passes
// when its object key or one of the keys below its prefix may be reported.
func (w *walker) mayMatch(it *walkItem) bool {
	if it.dirObject {
		return w.matchObject(it.base)
	}
	prefix := it.base + "/"
	if p := w.opts.FilterPrefix; p != "" && !strings.HasPrefix(it.base, p) && !strings.HasPrefix(p, prefix) {
		return false
	}
	if f := w.opts.ForwardTo; f != "" && prefix < f && !strings.HasPrefix(f, prefix) {
		return false
	}
	return true
}

// matchObject applies FilterPrefix and ForwardTo to an object key.
func (w *walker) matchObject(name string) bool {
	if p := w.opts.FilterPrefix; p != "" && !strings.HasPrefix(name, p) {
		return false
	}
	if f := w.opts.ForwardTo; f != "" && name < f {
		return false
	}
	return true
}

// matchPrefix applies FilterPrefix and ForwardTo to a prefix (ending with
// a slash): it passes when it may contain matching entries.
func (w *walker) matchPrefix(name string) bool {
	if p := w.opts.FilterPrefix; p != "" && !strings.HasPrefix(name, p) && !strings.HasPrefix(p, name) {
		return false
	}
	if f := w.opts.ForwardTo; f != "" && name < f && !strings.HasPrefix(f, name) {
		return false
	}
	return true
}
