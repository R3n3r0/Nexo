package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// MirrorRequest is one mirror of a directory or prefix to another one
// (docs/02 C-OBJ-06).
type MirrorRequest struct {
	Source Target
	Target Target
	// Options: Overwrite replaces the different target paths, Remove
	// deletes the target paths missing from the source.
	Options MirrorOptions
	// DryRun reports the actions without doing them.
	DryRun bool
	// Transfer tunes the copies. Its Progress, when set, receives the
	// transferred bytes and the name of the file being copied.
	Transfer TransferOptions
}

// MirrorPlan is the comparison of the two sides of a mirror.
type MirrorPlan struct {
	// Actions are sorted by path (PlanMirror).
	Actions []MirrorAction
	// Bytes is the size of the copies and overwrites.
	Bytes int64
}

// MirrorEvent is the outcome of one action of a mirror.
type MirrorEvent struct {
	Kind MirrorActionKind
	// Source is the copied path (zero for a removal).
	Source Target
	// Dest is the written, kept (conflict) or removed path.
	Dest Target
	// Size is the copied size: the size of the source in a dry run, for a
	// conflict and for an action not attempted; 0 for a removal.
	Size int64
	// Err is set for an action not attempted (a name that cannot be
	// written under a local directory wraps ErrUnsafeName) or, during a
	// watch, for a removal that failed: the mirror goes on.
	Err error
}

// MirrorReporter receives the events of a mirror in order; its error ends
// the mirror.
type MirrorReporter func(MirrorEvent) error

// MirrorStats sums the actions of a mirror pass.
type MirrorStats struct {
	Copied    int
	Bytes     int64
	Removed   int
	Conflicts int
	// Failed counts the actions not attempted; First is the error of the
	// first one.
	Failed int
	First  error
}

// Mirrorer synchronizes a directory or prefix to another one: the engine of
// nexoctl mirror, which only decodes its flags, calls it and prints the
// events.
type Mirrorer interface {
	// Plan lists both sides of req and compares them (PlanMirror).
	Plan(ctx context.Context, req MirrorRequest) (*MirrorPlan, error)
	// Apply runs the actions of plan in order and reports each one. An
	// action on a path that cannot be written is reported with Err and
	// counted as failed; a failed transfer or removal ends the pass with
	// its error.
	Apply(ctx context.Context, req MirrorRequest, plan *MirrorPlan, report MirrorReporter) (MirrorStats, error)
	// Watch keeps the target synchronized with the changes of the local
	// source until ctx is done, then returns nil: a created or modified
	// file is copied unless the target is up to date (SameEntry) or
	// differs without Options.Overwrite (a conflict event), a removed
	// path is removed from the target with Options.Remove. ready, when
	// not nil, runs once the watches are in place (the first pass:
	// nothing changed meanwhile is missed); its error ends the watch. The
	// failure of one change goes to onErr and the watch goes on.
	Watch(ctx context.Context, req MirrorRequest, ready func() error, report MirrorReporter, onErr func(error)) error
}

// MirrorDeps are the dependencies of the mirror.
type MirrorDeps struct {
	// Resolve returns the S3 client of an alias (required).
	Resolve ClientResolver
}

// MirrorerOption configures the mirror.
type MirrorerOption func(*mirrorer)

// WithWatchDebounce sets how long the file events of one path are
// coalesced during a watch (default DefaultWatchDebounce).
func WithWatchDebounce(d time.Duration) MirrorerOption {
	return func(m *mirrorer) {
		if d > 0 {
			m.debounce = d
		}
	}
}

type mirrorer struct {
	deps     MirrorDeps
	transfer Transfer
	debounce time.Duration
}

var _ Mirrorer = (*mirrorer)(nil)

// NewMirrorer returns the mirror engine. A missing client resolver is a
// wiring error (domain.ErrInvalidArgument).
func NewMirrorer(deps MirrorDeps, opts ...MirrorerOption) (Mirrorer, error) {
	if deps.Resolve == nil {
		return nil, fmt.Errorf("%w: the mirror needs a client resolver", domain.ErrInvalidArgument)
	}
	m := &mirrorer{deps: deps, transfer: Transfer(deps), debounce: DefaultWatchDebounce}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Plan implements Mirrorer.
func (m *mirrorer) Plan(ctx context.Context, req MirrorRequest) (*MirrorPlan, error) {
	src, err := CollectEntries(Walk(ctx, m.deps.Resolve, req.Source))
	if err != nil {
		return nil, err
	}
	dst, err := CollectEntries(Walk(ctx, m.deps.Resolve, req.Target))
	if err != nil {
		return nil, err
	}
	plan := &MirrorPlan{Actions: PlanMirror(src, dst, req.Options)}
	for _, a := range plan.Actions {
		if a.Kind == MirrorCopy || a.Kind == MirrorOverwrite {
			plan.Bytes += a.Source.Size
		}
	}
	return plan, nil
}

// Apply implements Mirrorer.
func (m *mirrorer) Apply(ctx context.Context, req MirrorRequest, plan *MirrorPlan, report MirrorReporter) (MirrorStats, error) {
	var stats MirrorStats
	if plan == nil {
		return stats, nil
	}
	opts := req.Transfer
	for _, a := range plan.Actions {
		item, err := mirrorItem(req.Source, req.Target, a)
		if err != nil {
			// Never written: reported as a failure of this path only.
			if item.Source.Raw == "" {
				item.Source = Target{Raw: a.Rel, Path: a.Rel}
			}
			stats.Failed++
			if stats.First == nil {
				stats.First = fmt.Errorf("%s: %w", item.Source, err)
			}
			if err := report(MirrorEvent{Kind: a.Kind, Source: item.Source, Dest: item.Dest, Size: a.Source.Size, Err: err}); err != nil {
				return stats, err
			}
			continue
		}
		ev := MirrorEvent{Kind: a.Kind, Source: item.Source, Dest: item.Dest, Size: a.Source.Size}
		switch a.Kind {
		case MirrorConflict:
			stats.Conflicts++
		case MirrorRemove:
			if !req.DryRun {
				if err := m.transfer.Delete(ctx, item.Dest, ""); err != nil {
					return stats, fmt.Errorf("remove %s: %w", item.Dest, err)
				}
			}
			stats.Removed++
			ev.Size = 0
		default:
			if !req.DryRun {
				opts.Progress.SetLabel(baseName(item.Source))
				res, err := m.transfer.Copy(ctx, item, opts)
				if err != nil {
					return stats, err
				}
				ev.Size = res.Size
			}
			stats.Copied++
			stats.Bytes += ev.Size
		}
		if err := report(ev); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// mirrorItem returns the transfer of a mirror action. The source of a
// removal is not needed: its name may not even be valid on the source
// side. The error is ErrUnsafeName for a name that cannot be written under
// a local directory; the item then carries the source to report.
func mirrorItem(src, dst Target, a MirrorAction) (CopyItem, error) {
	item := CopyItem{Size: a.Source.Size, ModTime: a.Source.ModTime, ETag: a.Source.ETag}
	var err error
	if a.Kind != MirrorRemove {
		if item.Source, err = JoinTarget(src, a.Rel); err != nil {
			return item, err
		}
	}
	if item.Dest, err = JoinTarget(dst, a.Rel); err != nil {
		return item, err
	}
	return item, nil
}

// baseName is the last element of a target (the progress label).
func baseName(t Target) string {
	if t.IsRemote() {
		return path.Base(t.Key)
	}
	return filepath.Base(t.Path)
}

// Watch implements Mirrorer.
func (m *mirrorer) Watch(ctx context.Context, req MirrorRequest, ready func() error, report MirrorReporter, onErr func(error)) error {
	if req.Source.IsRemote() {
		return UsageError("%s: only a local source can be watched", req.Source)
	}
	opts := req.Transfer
	opts.Progress = nil
	handle := func(ctx context.Context, ev WatchEvent) error {
		return m.watchChange(ctx, req, opts, ev, report)
	}
	return WatchDir(ctx, req.Source.Path, m.debounce, ready, handle, onErr)
}

// watchChange synchronizes one change of the source.
func (m *mirrorer) watchChange(ctx context.Context, req MirrorRequest, opts TransferOptions, ev WatchEvent, report MirrorReporter) error {
	from, err := JoinTarget(req.Source, ev.Rel)
	if err != nil {
		return err
	}
	to, err := JoinTarget(req.Target, ev.Rel)
	if err != nil {
		return err
	}
	if ev.Removed {
		if !req.Options.Remove {
			return nil
		}
		return m.removeTree(ctx, to, report)
	}
	st, err := os.Stat(from.Path)
	if errors.Is(err, os.ErrNotExist) {
		// Gone since the event: its removal follows.
		return nil
	}
	if err != nil {
		return err
	}
	source := Entry{Rel: ev.Rel, Size: st.Size(), ModTime: st.ModTime()}
	item := CopyItem{Source: from, Dest: to, Size: source.Size, ModTime: source.ModTime}
	kind := MirrorCopy
	existing, ok, err := m.entryOf(ctx, to)
	if err != nil {
		return err
	}
	if ok {
		if SameEntry(source, existing) {
			return nil
		}
		if !req.Options.Overwrite {
			return report(MirrorEvent{Kind: MirrorConflict, Source: from, Dest: to, Size: source.Size})
		}
		kind = MirrorOverwrite
	}
	res, err := m.transfer.Copy(ctx, item, opts)
	if err != nil {
		return err
	}
	return report(MirrorEvent{Kind: kind, Source: from, Dest: to, Size: res.Size})
}

// removeTree removes a target path and everything under it (a removed file
// or directory of the source). A remote object that cannot be removed is
// reported with Err.
func (m *mirrorer) removeTree(ctx context.Context, dst Target, report MirrorReporter) error {
	if !dst.IsRemote() {
		if _, err := os.Lstat(dst.Path); err != nil {
			return nil
		}
		if err := os.RemoveAll(dst.Path); err != nil {
			return err
		}
		return report(MirrorEvent{Kind: MirrorRemove, Dest: dst})
	}
	client, err := m.deps.Resolve(ctx, dst.Alias)
	if err != nil {
		return err
	}
	_, err = RemoveObjects(ctx, client, dst.Bucket, dst.Key, RemoveOptions{Recursive: true}, func(r RemoveResult) error {
		return report(MirrorEvent{Kind: MirrorRemove, Dest: dst.Child(r.Key), Err: r.Err})
	})
	return err
}

// entryOf returns the entry of a single local file or remote object; ok is
// false when it does not exist.
func (m *mirrorer) entryOf(ctx context.Context, t Target) (Entry, bool, error) {
	if !t.IsRemote() {
		st, err := os.Stat(t.Path)
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, false, nil
		}
		if err != nil {
			return Entry{}, false, err
		}
		return Entry{Size: st.Size(), ModTime: st.ModTime()}, true, nil
	}
	client, err := m.deps.Resolve(ctx, t.Alias)
	if err != nil {
		return Entry{}, false, err
	}
	info, err := client.Stat(ctx, t.Bucket, t.Key, StatOptions{})
	if errors.Is(err, domain.ErrObjectNotFound) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	return Entry{Size: info.Size, ModTime: info.LastModified, ETag: info.ETag}, true, nil
}
