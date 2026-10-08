package sysstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// ErrNotFound reports a missing (or deleted) document.
var ErrNotFound = fmt.Errorf("%w: sysstore: no such document", domain.ErrNotFound)

// envelopeFormat marks the files written by the store.
const envelopeFormat = "nexo-sysdoc-v1"

// stragglerTimeout bounds the writes still running on slow drives after a
// write returned with its quorum.
const stragglerTimeout = 30 * time.Second

// listConcurrency bounds the documents List reads at the same time.
const listConcurrency = 16

// envelope is the on-disk form of a document copy.
type envelope struct {
	Format string `json:"format"`
	// Stamp orders the versions of the document: a write stamps
	// max(now, newest stamp + 1) under the document lock.
	Stamp   int64  `json:"stamp"`
	Deleted bool   `json:"deleted,omitempty"`
	Data    []byte `json:"data,omitempty"`
}

// store implements Store.
type store struct {
	deps Deps
	opts options
	// writeQ and readQ are the quorums: W = N/2+1 and R = N-W+1.
	writeQ, readQ int
}

var _ Store = (*store)(nil)

// New returns a Store over deps.Drives.
func New(deps Deps, opts ...Option) (Store, error) {
	if len(deps.Drives) == 0 {
		return nil, fmt.Errorf("%w: sysstore: at least one drive is required", domain.ErrInvalidArgument)
	}
	if deps.Locks == nil {
		deps.Locks = lock.NewLocal(lock.Deps{})
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	n := len(deps.Drives)
	w := n/2 + 1
	return &store{deps: deps, opts: o, writeQ: w, readQ: n - w + 1}, nil
}

// --- paths -----------------------------------------------------------------------

// validSegmentRune reports whether r may appear in a path segment.
func validSegmentRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("._-~+=@,", r)
	}
}

// checkPath validates a document (or directory, when dir) path.
func checkPath(p string, dir bool) error {
	if p == "" {
		if dir {
			return nil
		}
		return fmt.Errorf("%w: sysstore: empty document path", domain.ErrInvalidArgument)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: sysstore: invalid path %q", domain.ErrInvalidArgument, p)
		}
		for _, r := range seg {
			if !validSegmentRune(r) {
				return fmt.Errorf("%w: sysstore: invalid character %q in path %q", domain.ErrInvalidArgument, r, p)
			}
		}
	}
	return nil
}

// diskPath is the path of a document in the system volume.
func (s *store) diskPath(p string) string {
	if p == "" {
		return s.opts.root
	}
	return s.opts.root + "/" + p
}

// --- quorum calls ------------------------------------------------------------------

// errOffline marks a drive that is offline or did not answer in time.
var errOffline = fmt.Errorf("sysstore: %w", domain.ErrDiskNotFound)

// outcome is the answer of one drive.
type outcome[T any] struct {
	val T
	err error
}

// gather runs fn on every drive concurrently. It returns as soon as every
// drive answered, or once need answers satisfy ok and the grace period
// elapsed: the drives that did not answer then are reported errOffline
// (their calls keep running on callCtx, canceled with cancelLate unless
// keep is set). Results travel through a channel only, so a late answer
// never races with the caller.
func gather[T any](ctx context.Context, s *store, need int, keep bool, ok func(T, error) bool,
	fn func(ctx context.Context, d storage.StorageAPI) (T, error),
) []outcome[T] {
	n := len(s.deps.Drives)
	out := make([]outcome[T], n)
	for i := range out {
		out[i].err = errOffline
	}
	callCtx, cancel := context.WithCancel(ctx)
	if keep {
		// The writes still running once the quorum is reached complete in
		// the background (bounded), so that slow drives are not left stale.
		callCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), stragglerTimeout)
	}
	type answer struct {
		i int
		outcome[T]
	}
	ch := make(chan answer, n)
	pending := 0
	for i, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		pending++
		go func() {
			v, err := fn(callCtx, d)
			ch <- answer{i: i, outcome: outcome[T]{val: v, err: err}}
		}()
	}
	if pending == 0 {
		cancel()
		return out
	}
	// The stragglers are released when the last of them answers.
	late := func(left int) {
		go func() {
			for range left {
				<-ch
			}
			cancel()
		}()
	}
	good := 0
	var graceC <-chan time.Time
	for pending > 0 {
		select {
		case a := <-ch:
			pending--
			out[a.i] = a.outcome
			if ok(a.val, a.err) {
				good++
			}
			if good >= need && graceC == nil && pending > 0 {
				timer := time.NewTimer(s.opts.grace)
				defer timer.Stop()
				graceC = timer.C
			}
		case <-graceC:
			if !keep {
				cancel()
			}
			late(pending)
			return out
		case <-ctx.Done():
			if !keep {
				cancel()
			}
			late(pending)
			return out
		}
	}
	cancel()
	return out
}

// reply is what one drive holds for a document.
type reply struct {
	env *envelope
	// missing reports a drive that answered without a copy.
	missing bool
}

func (r reply) answered() bool { return r.env != nil || r.missing }

// readCopies reads the copies of a document.
func (s *store) readCopies(ctx context.Context, p string) []outcome[reply] {
	dp := s.diskPath(p)
	answered := func(r reply, err error) bool { return err == nil && r.answered() }
	return gather(ctx, s, s.readQ, false, answered, func(ctx context.Context, d storage.StorageAPI) (reply, error) {
		data, err := d.ReadAll(ctx, domain.SystemBucket, dp)
		switch {
		case errors.Is(err, domain.ErrObjectNotFound):
			return reply{missing: true}, nil
		case err != nil:
			return reply{}, err
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil || env.Format != envelopeFormat {
			s.deps.Logger.WarnContext(ctx, "sysstore: unreadable document copy, ignored", "drive", d.Endpoint().String(), "path", dp, "err", err)
			return reply{missing: true}, nil
		}
		return reply{env: &env}, nil
	})
}

// newest returns the newest copy among the answers and how many drives
// answered.
func newest(outs []outcome[reply]) (*envelope, int) {
	var best *envelope
	answered := 0
	for _, o := range outs {
		if o.err != nil || !o.val.answered() {
			continue
		}
		answered++
		if e := o.val.env; e != nil && (best == nil || e.Stamp > best.Stamp) {
			best = e
		}
	}
	return best, answered
}

// stale lists the drives whose copy is older than best.
func stale(outs []outcome[reply], best *envelope) []int {
	if best == nil {
		return nil
	}
	var idx []int
	for i, o := range outs {
		if o.err != nil || !o.val.answered() {
			continue
		}
		switch {
		case o.val.env == nil:
			if !best.Deleted {
				idx = append(idx, i)
			}
		case o.val.env.Stamp < best.Stamp:
			idx = append(idx, i)
		}
	}
	return idx
}

func (s *store) readQuorumErr(p string, answered int) error {
	return fmt.Errorf("%w: sysstore: %s: %d of %d drives answered, %d needed",
		domain.ErrReadQuorum, p, answered, len(s.deps.Drives), s.readQ)
}

// --- reads -----------------------------------------------------------------------

// Get implements Store.
func (s *store) Get(ctx context.Context, p string) ([]byte, error) {
	if err := checkPath(p, false); err != nil {
		return nil, err
	}
	outs := s.readCopies(ctx, p)
	best, answered := newest(outs)
	if answered < s.readQ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, s.readQuorumErr(p, answered)
	}
	if len(stale(outs, best)) > 0 {
		s.repair(ctx, p)
	}
	if best == nil || best.Deleted {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	return best.Data, nil
}

// repair rewrites the newest copy of a document on the drives holding an
// older one (or none), under the document lock: a write running at the
// same time is never overwritten with an older copy. It is best effort:
// a busy lock or a failure only defers the repair to the next read.
func (s *store) repair(ctx context.Context, p string) {
	ctx = context.WithoutCancel(ctx)
	lc, err := s.deps.Locks.NewNSLock(ctx, domain.SystemBucket, s.diskPath(p)).GetLock(ctx, 250*time.Millisecond)
	if err != nil {
		return
	}
	defer lc.Unlock(ctx)
	outs := s.readCopies(lc.Context(), p)
	best, answered := newest(outs)
	if best == nil || answered < s.readQ {
		return
	}
	idx := stale(outs, best)
	if len(idx) == 0 {
		return
	}
	data, err := json.Marshal(best)
	if err != nil {
		return
	}
	dp := s.diskPath(p)
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := s.deps.Drives[i]
			if err := d.WriteAll(lc.Context(), domain.SystemBucket, dp, data); err != nil {
				s.deps.Logger.DebugContext(ctx, "sysstore: read repair failed", "drive", d.Endpoint().String(), "path", dp, "err", err)
			}
		}()
	}
	wg.Wait()
	s.deps.Logger.DebugContext(ctx, "sysstore: read repair", "path", dp, "drives", len(idx))
}

// List implements Store.
func (s *store) List(ctx context.Context, dir string) (map[string][]byte, error) {
	if err := checkPath(dir, true); err != nil {
		return nil, err
	}
	pattern := s.diskPath(dir) + "/*"
	listed := func(_ []storage.StatInfo, err error) bool { return err == nil }
	outs := gather(ctx, s, s.readQ, false, listed, func(ctx context.Context, d storage.StorageAPI) ([]storage.StatInfo, error) {
		infos, err := d.StatInfoFile(ctx, domain.SystemBucket, pattern, true)
		if errors.Is(err, domain.ErrObjectNotFound) {
			return nil, nil
		}
		return infos, err
	})
	names := map[string]bool{}
	answered := 0
	for _, o := range outs {
		if o.err != nil {
			continue
		}
		answered++
		for _, info := range o.val {
			if info.Dir {
				continue
			}
			name := path.Base(info.Name)
			if checkPath(name, false) == nil && !strings.HasPrefix(name, ".") {
				names[name] = true
			}
		}
	}
	if answered < s.readQ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, s.readQuorumErr(dir, answered)
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	out := make(map[string][]byte, len(sorted))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, listConcurrency)
	for _, name := range sorted {
		docPath := name
		if dir != "" {
			docPath = dir + "/" + name
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			data, err := s.Get(ctx, docPath)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			default:
				out[name] = data
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// --- writes ----------------------------------------------------------------------

// locked runs fn under the write lock of the document, with the newest
// copy read from a read quorum (nil when no drive has one).
func (s *store) locked(ctx context.Context, p string, fn func(ctx context.Context, cur *envelope) error) error {
	if err := checkPath(p, false); err != nil {
		return err
	}
	lc, err := s.deps.Locks.NewNSLock(ctx, domain.SystemBucket, s.diskPath(p)).GetLock(ctx, s.opts.lockTimeout)
	if err != nil {
		return fmt.Errorf("sysstore: lock %s: %w", p, err)
	}
	defer lc.Unlock(ctx)
	lctx := lc.Context()
	outs := s.readCopies(lctx, p)
	cur, answered := newest(outs)
	if answered < s.readQ {
		if err := lctx.Err(); err != nil {
			return err
		}
		// Without a read quorum the newest stamp is unknown: writing could
		// shadow a newer copy.
		return fmt.Errorf("%w: sysstore: %s: %d of %d drives answered, %d needed",
			domain.ErrWriteQuorum, p, answered, len(s.deps.Drives), s.readQ)
	}
	return fn(lctx, cur)
}

// write stores the next version of a document on a write quorum of
// drives.
func (s *store) write(ctx context.Context, p string, cur *envelope, deleted bool, data []byte) error {
	stamp := s.deps.Clock.Now().UnixNano()
	if cur != nil && stamp <= cur.Stamp {
		stamp = cur.Stamp + 1
	}
	raw, err := json.Marshal(envelope{Format: envelopeFormat, Stamp: stamp, Deleted: deleted, Data: data})
	if err != nil {
		return fmt.Errorf("sysstore: encode %s: %w", p, err)
	}
	dp := s.diskPath(p)
	written := func(_ struct{}, err error) bool { return err == nil }
	outs := gather(ctx, s, s.writeQ, true, written, func(ctx context.Context, d storage.StorageAPI) (struct{}, error) {
		return struct{}{}, d.WriteAll(ctx, domain.SystemBucket, dp, raw)
	})
	ok := 0
	var firstErr error
	for _, o := range outs {
		if o.err == nil {
			ok++
		} else if firstErr == nil {
			firstErr = o.err
		}
	}
	if ok < s.writeQ {
		s.rollback(ctx, dp, cur, outs)
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: sysstore: %s: written on %d of %d drives, %d needed: %w",
			domain.ErrWriteQuorum, p, ok, len(s.deps.Drives), s.writeQ, firstErr)
	}
	return nil
}

// rollback restores the previous copy (or removes the new one) on the
// drives a failed write reached, so that a refused write never becomes
// visible later. It runs under the document lock.
func (s *store) rollback(ctx context.Context, dp string, prev *envelope, outs []outcome[struct{}]) {
	ctx = context.WithoutCancel(ctx)
	var raw []byte
	if prev != nil {
		var err error
		if raw, err = json.Marshal(prev); err != nil {
			return
		}
	}
	var wg sync.WaitGroup
	for i, o := range outs {
		if o.err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := s.deps.Drives[i]
			var err error
			if raw != nil {
				err = d.WriteAll(ctx, domain.SystemBucket, dp, raw)
			} else {
				err = d.Delete(ctx, domain.SystemBucket, dp, storage.DeleteOptions{})
			}
			if err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
				s.deps.Logger.WarnContext(ctx, "sysstore: rollback of a refused write failed", "drive", d.Endpoint().String(), "path", dp, "err", err)
			}
		}()
	}
	wg.Wait()
}

// Put implements Store.
func (s *store) Put(ctx context.Context, p string, data []byte) error {
	return s.locked(ctx, p, func(ctx context.Context, cur *envelope) error {
		return s.write(ctx, p, cur, false, data)
	})
}

// Create implements Store.
func (s *store) Create(ctx context.Context, p string, data []byte) ([]byte, error) {
	var stored []byte
	err := s.locked(ctx, p, func(ctx context.Context, cur *envelope) error {
		if cur != nil && !cur.Deleted {
			stored = cur.Data
			return nil
		}
		if err := s.write(ctx, p, cur, false, data); err != nil {
			return err
		}
		stored = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// Delete implements Store.
func (s *store) Delete(ctx context.Context, p string) error {
	return s.locked(ctx, p, func(ctx context.Context, cur *envelope) error {
		if cur == nil || cur.Deleted {
			return nil
		}
		return s.write(ctx, p, cur, true, nil)
	})
}
