package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// walkBatch is the number of keys a log walk takes from the index at once:
// the index is not locked while fn runs (it may write to the drive).
const walkBatch = 256

// walkLog is WalkDir for a volume in log mode: the keys come from the index
// in order, the prefixes are derived from them. It emits what the walk of
// the directory tree emits for the same objects (walk.go): objects and
// prefixes in S3 key order, a directory object before the prefix of the
// same name, a prefix only when an object lies below it, and the same
// handling of Recursive, FilterPrefix, ForwardTo, Limit and fs.SkipDir.
func (s *xlStorage) walkLog(ctx context.Context, opts WalkDirOptions, fn WalkDirFunc) error {
	if _, err := s.ensureVolume(opts.Bucket); err != nil {
		return err
	}
	if err := s.checkLog(); err != nil {
		return err
	}
	base := strings.Trim(opts.BaseDir, "/")
	keyPrefix := ""
	if base != "" {
		// A link on the drive is refused as by the tree walk.
		if _, err := s.filePath(opts.Bucket, base); err != nil {
			return err
		}
		keyPrefix = base + "/"
	}
	if base != "" && opts.ReportNotFound && !s.ml().hasChildren(opts.Bucket, keyPrefix) && !s.ml().has(opts.Bucket, keyPrefix) {
		return fmt.Errorf("%w: %s/%s", domain.ErrObjectNotFound, opts.Bucket, base)
	}
	w := &logWalker{s: s, ctx: ctx, opts: opts, fn: fn, keyPrefix: keyPrefix, filter: &walker{opts: opts}}
	err := w.run()
	switch {
	case err == nil, errors.Is(err, errWalkLimit), errors.Is(err, fs.SkipAll), errors.Is(err, fs.SkipDir):
		return nil
	default:
		return err
	}
}

type logWalker struct {
	s         *xlStorage
	ctx       context.Context
	opts      WalkDirOptions
	fn        WalkDirFunc
	keyPrefix string
	// filter applies FilterPrefix and ForwardTo like the tree walk.
	filter *walker
	// open are the prefixes already emitted (or passed) above the current
	// key, shortest first (recursive walks).
	open  []string
	count int
	// skip is a prefix whose keys are not reported (fs.SkipDir).
	skip string
	// seek is where the next batch starts.
	seek string
}

func (w *logWalker) run() error {
	w.seek = max(w.keyPrefix, w.opts.FilterPrefix, w.opts.ForwardTo)
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		keys := w.s.ml().scan(w.opts.Bucket, w.seek, walkBatch)
		for _, k := range keys {
			if k < w.seek {
				continue // passed by a seek within this batch
			}
			if !strings.HasPrefix(k, w.keyPrefix) {
				return nil
			}
			if p := w.opts.FilterPrefix; p != "" && k > p && !strings.HasPrefix(k, p) {
				return nil
			}
			if err := w.visit(k); err != nil {
				return err
			}
		}
		if len(keys) < walkBatch {
			return nil
		}
		w.seek = max(w.seek, keys[len(keys)-1]+"\x00")
	}
}

// visit handles one key of the index.
func (w *logWalker) visit(k string) error {
	if w.skip != "" {
		if strings.HasPrefix(k, w.skip) {
			return nil
		}
		w.skip = ""
	}
	rest := k[len(w.keyPrefix):]
	if rest == "" {
		return nil // the directory object named like the walked directory lives in its parent
	}
	if !w.opts.Recursive {
		if i := strings.IndexByte(rest, '/'); i >= 0 && i < len(rest)-1 {
			// Below a prefix of this level: report it once, then go past
			// its keys.
			p := w.keyPrefix + rest[:i+1]
			w.seek = afterPrefix(p)
			return w.emitPrefix(p)
		}
		return w.emitObject(k)
	}
	for len(w.open) > 0 && !strings.HasPrefix(k, w.open[len(w.open)-1]) {
		w.open = w.open[:len(w.open)-1]
	}
	opened := len(w.keyPrefix)
	if len(w.open) > 0 {
		opened = len(w.open[len(w.open)-1])
	}
	// The ancestors of k below the walked directory, shortest first: a
	// trailing slash (a directory object) opens no prefix.
	for j := opened; j < len(k)-1; j++ {
		if k[j] != '/' {
			continue
		}
		p := k[:j+1]
		w.open = append(w.open, p)
		if err := w.emitPrefix(p); err != nil {
			return err
		}
		if w.skip != "" {
			return nil // the prefix was skipped with its content
		}
	}
	return w.emitObject(k)
}

// emitPrefix reports a prefix; fs.SkipDir skips its content.
func (w *logWalker) emitPrefix(p string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if !w.filter.matchPrefix(p) {
		return nil
	}
	err := w.fn(WalkEntry{Name: p, IsDir: true})
	if errors.Is(err, fs.SkipDir) {
		w.skip = p
		w.seek = max(w.seek, afterPrefix(p))
		return nil
	}
	return err
}

// emitObject reports an object with the metadata section of its xl.meta;
// fs.SkipDir skips the remaining entries of its directory.
func (w *logWalker) emitObject(k string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if !w.filter.matchObject(k) {
		return nil
	}
	raw, err := w.s.ml().read(w.opts.Bucket, k, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed concurrently
		}
		return err
	}
	err = w.fn(WalkEntry{Name: k, Metadata: raw})
	if errors.Is(err, fs.SkipDir) {
		dir := parentDir(k)
		if len(dir) <= len(w.keyPrefix) {
			return fs.SkipAll // the walked directory itself
		}
		w.skip = dir
		w.seek = max(w.seek, afterPrefix(dir))
		return nil
	}
	if err != nil {
		return err
	}
	w.count++
	if w.opts.Limit > 0 && w.count >= w.opts.Limit {
		return errWalkLimit
	}
	return nil
}

// parentDir returns the directory a key is listed in: the key up to its
// last slash, a trailing slash (directory object) excluded.
func parentDir(k string) string {
	t := strings.TrimSuffix(k, "/")
	return t[:strings.LastIndexByte(t, '/')+1]
}

// afterPrefix returns the smallest key greater than every key starting with
// p (p ends with a slash, which is below '0').
func afterPrefix(p string) string {
	return p[:len(p)-1] + string([]byte{p[len(p)-1] + 1})
}
