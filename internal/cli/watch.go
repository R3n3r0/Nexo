package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// DefaultWatchDebounce coalesces the bursts of file system events of one
// write (mirror --watch).
const DefaultWatchDebounce = 200 * time.Millisecond

// WatchEvent is a change of a path under a watched directory.
type WatchEvent struct {
	// Rel is the "/"-separated path relative to the root.
	Rel string
	// Removed reports a path that no longer exists (a file or a whole
	// directory); otherwise Rel is a regular file created or modified.
	Removed bool
}

// WatchDir watches root and its subdirectories (including the ones created
// later) until ctx is done, then returns nil. ready, when not nil, runs once
// the watches are in place and before the events are processed (mirror
// --watch synchronizes there, so that no change is missed); its error ends
// the watch. The events of a path within debounce are coalesced; when they
// settle, fn receives the regular files that exist (created or modified)
// and the paths that disappeared, in path order. The failures of fn are
// passed to onErr and do not stop the watch.
func WatchDir(ctx context.Context, root string, debounce time.Duration, ready func() error, fn func(context.Context, WatchEvent) error, onErr func(error)) error {
	if debounce <= 0 {
		debounce = DefaultWatchDebounce
	}
	if st, err := os.Stat(root); err != nil {
		return fmt.Errorf("watch %s: %w", root, err)
	} else if !st.IsDir() {
		return fmt.Errorf("watch %s: not a directory", root)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch %s: %w", root, err)
	}
	defer func() { _ = w.Close() }()
	pending := map[string]bool{}
	addTree := func(dir string, notify bool) error {
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				if err := w.Add(p); err != nil {
					return fmt.Errorf("watch %s: %w", p, err)
				}
				return nil
			}
			if notify && d.Type().IsRegular() {
				pending[p] = true
			}
			return nil
		})
	}
	if err := addTree(root, false); err != nil {
		return err
	}
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	timer := time.NewTimer(debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	flush := func() {
		paths := SortedKeys(pending)
		clear(pending)
		for _, p := range paths {
			rel, err := filepath.Rel(root, p)
			if err != nil || rel == "." {
				continue
			}
			ev := WatchEvent{Rel: filepath.ToSlash(rel)}
			st, err := os.Lstat(p)
			switch {
			case err == nil && st.Mode().IsRegular():
			case err == nil:
				continue // a directory or a special file
			case errors.Is(err, fs.ErrNotExist):
				ev.Removed = true
			default:
				onErr(err)
				continue
			}
			if err := fn(ctx, ev); err != nil {
				onErr(err)
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				if st, err := os.Lstat(ev.Name); err == nil && st.IsDir() {
					// Files may be created before the watch of the new
					// directory starts: they are listed now.
					if err := addTree(ev.Name, true); err != nil {
						onErr(err)
					}
					timer.Reset(debounce)
					continue
				}
			}
			if ev.Has(fsnotify.Chmod) && !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
				continue
			}
			pending[ev.Name] = true
			timer.Reset(debounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			onErr(err)
		case <-timer.C:
			flush()
		}
	}
}
