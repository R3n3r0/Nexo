package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// janitor is the Janitor of a node's local drives.
type janitor struct {
	deps JanitorDeps
	opts janitorOptions

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Janitor = (*janitor)(nil)

// NewJanitor returns the janitor of deps.Drives.
func NewJanitor(deps JanitorDeps, opts ...JanitorOption) (Janitor, error) {
	for _, d := range deps.Drives {
		if d == nil {
			return nil, fmt.Errorf("%w: storage: nil drive given to the janitor", domain.ErrInvalidArgument)
		}
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultJanitorOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &janitor{deps: deps, opts: o}, nil
}

// Start implements lifecycle.Runner: it sweeps every interval in the
// background.
func (j *janitor) Start(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cancel != nil || j.opts.interval <= 0 || len(j.deps.Drives) == 0 {
		return nil
	}
	base, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j.cancel, j.done = cancel, make(chan struct{})
	go j.loop(base, j.done)
	return nil
}

// Stop implements lifecycle.Runner.
func (j *janitor) Stop(ctx context.Context) error {
	j.mu.Lock()
	cancel, done := j.cancel, j.done
	j.cancel, j.done = nil, nil
	j.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("storage: stop the janitor: %w", ctx.Err())
	}
}

func (j *janitor) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(j.opts.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := j.Sweep(ctx); err != nil && ctx.Err() == nil {
				j.deps.Logger.WarnContext(ctx, "storage: temporary area sweep failed", "err", err)
			}
		}
	}
}

// Sweep implements Janitor.
func (j *janitor) Sweep(ctx context.Context) (int, error) {
	cutoff := j.deps.Clock.Now().Add(-j.opts.maxAge)
	removed := 0
	var errs []error
	for _, d := range j.deps.Drives {
		if !d.IsOnline() {
			continue
		}
		n, err := j.sweepDrive(ctx, d, cutoff)
		removed += n
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.Endpoint().String(), err))
		}
	}
	if removed > 0 {
		j.deps.Logger.InfoContext(ctx, "storage: removed stale temporary entries", "count", removed, "older_than", j.opts.maxAge)
	}
	return removed, errors.Join(errs...)
}

// sweepDrive removes the entries of the temporary area of d last modified
// before cutoff: a file by its own time, a directory by the newest of its
// files.
func (j *janitor) sweepDrive(ctx context.Context, d StorageAPI, cutoff time.Time) (int, error) {
	entries, err := d.StatInfoFile(ctx, domain.SystemBucket, TmpDir+"/*", true)
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !j.stale(ctx, d, e, cutoff) {
			continue
		}
		err := d.Delete(ctx, domain.SystemBucket, TmpDir+"/"+path.Base(e.Name), DeleteOptions{Recursive: true})
		if err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
			j.deps.Logger.WarnContext(ctx, "storage: cannot remove a stale temporary entry", "drive", d.Endpoint().String(), "entry", e.Name, "err", err)
			continue
		}
		removed++
	}
	return removed, nil
}

// stale reports whether the entry, and every file below it, was last
// modified before cutoff.
func (j *janitor) stale(ctx context.Context, d StorageAPI, e StatInfo, cutoff time.Time) bool {
	if !e.ModTime.Before(cutoff) {
		return false
	}
	if !e.Dir {
		return true
	}
	dir := TmpDir + "/" + path.Base(e.Name)
	for depth := 0; depth < 4; depth++ {
		pattern := dir + "/*"
		for range depth {
			pattern += "/*"
		}
		inner, err := d.StatInfoFile(ctx, domain.SystemBucket, pattern, true)
		if err != nil {
			return errors.Is(err, domain.ErrObjectNotFound)
		}
		if len(inner) == 0 {
			return true
		}
		for _, f := range inner {
			if !f.ModTime.Before(cutoff) {
				return false
			}
		}
	}
	return true
}
