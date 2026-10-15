package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Defaults of the file handles the peers open on the drives of a node
// (storage/open-file): at most defaultMaxHandles open at once, an idle
// handle (no stream opened for defaultHandleIdle) is closed.
const (
	defaultMaxHandles = 1 << 16
	defaultHandleIdle = time.Hour
)

// errHandleNotFound reports an unknown, closed or expired handle.
var errHandleNotFound = fmt.Errorf("%w: rpc: file handle", domain.ErrNotFound)

// handleEntry is a file a peer opened.
type handleEntry struct {
	h     storage.FileHandle
	drive string
	// node and boot identify the process of the peer that opened the
	// handle (Claims.NodeID and the boot id it sends with open-file):
	// handles of an earlier process of the same node are closed when a
	// new one opens its first file (the peer restarted and forgot them).
	node, boot string
	lastUse    time.Time
	// streams counts the streams of the handle being read: a handle in
	// use is never idle.
	streams int
}

// handleTable holds the files the peers opened on the drives of this node:
// a read that released its lock keeps the shards of the version it reads
// open here (storage.StorageAPI.OpenFile), even when a writer removes them.
// Handles a peer forgets are closed: after the idle timeout without a
// stream being read (a crashed peer, a lost close; swept periodically by
// the StorageRoutes runner and at each open), or as soon as a restarted
// process of the same peer opens a file. A closed handle keeps its file
// until its last stream ends.
type handleTable struct {
	clock clock.Clock
	max   int
	idle  time.Duration

	mu      sync.Mutex
	entries map[string]*handleEntry
}

func newHandleTable(clk clock.Clock, maxHandles int, idle time.Duration) *handleTable {
	return &handleTable{clock: clk, max: maxHandles, idle: idle, entries: map[string]*handleEntry{}}
}

// add registers a handle of drive opened by the process boot of the peer
// node and returns its id; at the limit the handle is closed and the call
// refused (the peer reads without pin).
func (t *handleTable) add(ctx context.Context, drive, node, boot string, h storage.FileHandle) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		_ = h.Close(ctx)
		return "", fmt.Errorf("rpc: handle id: %w", err)
	}
	id := hex.EncodeToString(raw)
	now := t.clock.Now()
	t.mu.Lock()
	expired := t.sweepLocked(now)
	expired = append(expired, t.forgetLocked(node, boot)...)
	full := len(t.entries) >= t.max
	if !full {
		t.entries[id] = &handleEntry{h: h, drive: drive, node: node, boot: boot, lastUse: now}
	}
	t.mu.Unlock()
	closeAll(ctx, expired)
	if full {
		_ = h.Close(ctx)
		return "", fmt.Errorf("%w: rpc: %d file handles open on this node", domain.ErrTooManyRequests, t.max)
	}
	return id, nil
}

// acquire returns the handle id of drive for a stream, marking it in use
// until release (called once, when the stream is closed).
func (t *handleTable) acquire(drive, id string) (storage.FileHandle, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[id]
	if !ok || e.drive != drive {
		return nil, nil, errHandleNotFound
	}
	e.lastUse = t.clock.Now()
	e.streams++
	var once sync.Once
	release := func() {
		once.Do(func() {
			t.mu.Lock()
			e.streams--
			e.lastUse = t.clock.Now()
			t.mu.Unlock()
		})
	}
	return e.h, release, nil
}

// remove closes the handle id of drive (a no-op when it is gone).
func (t *handleTable) remove(ctx context.Context, drive, id string) error {
	t.mu.Lock()
	e, ok := t.entries[id]
	if ok && e.drive == drive {
		delete(t.entries, id)
	}
	expired := t.sweepLocked(t.clock.Now())
	t.mu.Unlock()
	closeAll(ctx, expired)
	if !ok || e.drive != drive {
		return nil
	}
	return e.h.Close(ctx)
}

// sweep closes the handles idle for longer than the timeout and returns
// how many it closed.
func (t *handleTable) sweep(ctx context.Context) int {
	t.mu.Lock()
	expired := t.sweepLocked(t.clock.Now())
	t.mu.Unlock()
	closeAll(ctx, expired)
	return len(expired)
}

// closeEvery closes every handle (the node stops).
func (t *handleTable) closeEvery(ctx context.Context) {
	t.mu.Lock()
	all := make([]storage.FileHandle, 0, len(t.entries))
	for id, e := range t.entries {
		all = append(all, e.h)
		delete(t.entries, id)
	}
	t.mu.Unlock()
	closeAll(ctx, all)
}

// len returns the number of open handles.
func (t *handleTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// sweepLocked removes the handles idle for longer than the timeout (no
// stream being read) and returns them, to be closed without the lock.
func (t *handleTable) sweepLocked(now time.Time) []storage.FileHandle {
	var out []storage.FileHandle
	for id, e := range t.entries {
		if e.streams == 0 && now.Sub(e.lastUse) > t.idle {
			out = append(out, e.h)
			delete(t.entries, id)
		}
	}
	return out
}

// forgetLocked removes the handles node opened from another process than
// boot (an earlier one: the peer restarted and will never close them) and
// returns them, to be closed without the lock. A peer that sends no boot
// id (an older release) is left to the idle timeout.
func (t *handleTable) forgetLocked(node, boot string) []storage.FileHandle {
	if node == "" || boot == "" {
		return nil
	}
	var out []storage.FileHandle
	for id, e := range t.entries {
		if e.node == node && e.boot != "" && e.boot != boot {
			out = append(out, e.h)
			delete(t.entries, id)
		}
	}
	return out
}

// sweepInterval is the period of the sweep of the idle handles: a quarter
// of the idle timeout, within [1s, 5m].
func sweepInterval(idle time.Duration) time.Duration {
	return min(max(idle/4, time.Second), 5*time.Minute)
}

// releasingReader is a stream of a handle: Close releases the handle.
type releasingReader struct {
	io.ReadCloser
	release func()
}

func (r *releasingReader) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}

func closeAll(ctx context.Context, hs []storage.FileHandle) {
	for _, h := range hs {
		_ = h.Close(context.WithoutCancel(ctx))
	}
}
