package rpc

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

// closeCounter builds handles that count their closes.
type closeCounter struct {
	t      *testing.T
	mu     sync.Mutex
	closed map[string]int
}

func newCloseCounter(t *testing.T) *closeCounter {
	return &closeCounter{t: t, closed: map[string]int{}}
}

func (c *closeCounter) handle(name string) *storagemock.FileHandle {
	h := storagemock.NewFileHandle(c.t)
	h.EXPECT().Close(mock.Anything).RunAndReturn(func(context.Context) error {
		c.mu.Lock()
		c.closed[name]++
		c.mu.Unlock()
		return nil
	}).Maybe()
	h.EXPECT().ReadStream(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(context.Context, int64, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("shard")), nil
	}).Maybe()
	return h
}

func (c *closeCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed[name]
}

// TestHandleTable: the file handles the peers open are bound to their
// drive, closed by the peer or after the idle timeout (a peer that crashed
// or lost the close), and bounded in number.
func TestHandleTable(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	tbl := newHandleTable(clk, 2, time.Minute)
	cc := newCloseCounter(t)

	a, err := tbl.add(ctx, "/d1", "n1", "", cc.handle("a"))
	require.NoError(t, err)
	b, err := tbl.add(ctx, "/d2", "n1", "", cc.handle("b"))
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	_, _, err = tbl.acquire("/d2", a)
	require.ErrorIs(t, err, domain.ErrNotFound, "a handle of another drive")
	_, _, err = tbl.acquire("/d1", "unknown")
	require.ErrorIs(t, err, domain.ErrNotFound)

	// Full: the new handle is closed and refused.
	_, err = tbl.add(ctx, "/d1", "n1", "", cc.handle("c"))
	require.ErrorIs(t, err, domain.ErrTooManyRequests)
	assert.Equal(t, 1, cc.count("c"))

	// The peer closes b; a, used, outlives the idle timeout of a forgotten one.
	require.NoError(t, tbl.remove(ctx, "/d2", b))
	require.NoError(t, tbl.remove(ctx, "/d2", b), "closing twice")
	assert.Equal(t, 1, cc.count("b"))
	clk.Advance(50 * time.Second)
	_, release, err := tbl.acquire("/d1", a)
	require.NoError(t, err)
	release()
	clk.Advance(50 * time.Second)
	d, err := tbl.add(ctx, "/d1", "n1", "", cc.handle("d"))
	require.NoError(t, err)
	assert.Zero(t, cc.count("a"), "used 50s ago")
	clk.Advance(61 * time.Second)
	_, err = tbl.add(ctx, "/d1", "n1", "", cc.handle("e"))
	require.NoError(t, err)
	assert.Equal(t, 1, cc.count("a"), "idle for more than a minute")
	assert.Equal(t, 1, cc.count("d"))
	_, _, err = tbl.acquire("/d1", d)
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Equal(t, 1, tbl.len())
}

// TestHandleTableForgottenHandles: the handles a peer forgot are closed
// without waiting for another open on the node (the periodic sweep), at
// once when a new process of the peer opens a file (it restarted), and
// never while one of their streams is read, however long it takes
// (regression: a crashed or restarted reader left its handles, and the
// replaced data they hold, open on its peers until some other read opened
// a handle there; a read slower than the idle timeout lost its handles).
func TestHandleTableForgottenHandles(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC))
	tbl := newHandleTable(clk, 100, time.Minute)
	cc := newCloseCounter(t)

	// Two runs of node n1 and one of n2.
	old, err := tbl.add(ctx, "/d1", "n1", "boot-1", cc.handle("old"))
	require.NoError(t, err)
	_, err = tbl.add(ctx, "/d1", "n2", "boot-a", cc.handle("other node"))
	require.NoError(t, err)
	_, err = tbl.add(ctx, "/d1", "n1", "", cc.handle("no boot id"))
	require.NoError(t, err)
	_, err = tbl.add(ctx, "/d1", "n1", "boot-2", cc.handle("restarted"))
	require.NoError(t, err)
	assert.Equal(t, 1, cc.count("old"), "the handle of the earlier run of n1 is closed")
	_, _, err = tbl.acquire("/d1", old)
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Zero(t, cc.count("other node"))
	assert.Zero(t, cc.count("no boot id"), "a peer without boot id is left to the idle timeout")
	assert.Zero(t, cc.count("restarted"))

	// A stream being read keeps its handle past the idle timeout.
	reading, err := tbl.add(ctx, "/d1", "n2", "boot-a", cc.handle("reading"))
	require.NoError(t, err)
	fh, release, err := tbl.acquire("/d1", reading)
	require.NoError(t, err)
	rc, err := fh.ReadStream(ctx, 0, -1)
	require.NoError(t, err)
	stream := &releasingReader{ReadCloser: rc, release: release}
	clk.Advance(2 * time.Minute)
	assert.Equal(t, 3, tbl.sweep(ctx), "the periodic sweep closes the idle handles")
	assert.Equal(t, 1, cc.count("other node"))
	assert.Equal(t, 1, cc.count("no boot id"))
	assert.Equal(t, 1, cc.count("restarted"))
	assert.Zero(t, cc.count("reading"), "a handle with a stream being read is never idle")
	require.NoError(t, stream.Close())
	require.NoError(t, stream.Close(), "a second close releases nothing more")
	clk.Advance(30 * time.Second)
	assert.Zero(t, tbl.sweep(ctx), "idle since the end of its stream, not since its open")
	clk.Advance(31 * time.Second)
	assert.Equal(t, 1, tbl.sweep(ctx))
	assert.Equal(t, 1, cc.count("reading"))

	_, err = tbl.add(ctx, "/d1", "n2", "boot-a", cc.handle("last"))
	require.NoError(t, err)
	tbl.closeEvery(ctx)
	assert.Equal(t, 1, cc.count("last"), "a node that stops closes every handle")
	assert.Zero(t, tbl.len())
}

// TestStorageRoutesSweep: the runner of the storage routes sweeps the idle
// handles on its own and closes every handle when it stops.
func TestStorageRoutesSweep(t *testing.T) {
	ctx := context.Background()
	routes, err := NewStorageRoutes(StorageRoutesDeps{}, WithFileHandles(10, time.Millisecond))
	require.NoError(t, err)
	h := routes.(*storageRoutes)
	cc := newCloseCounter(t)
	_, err = h.handles.add(ctx, "/d1", "n1", "boot", cc.handle("idle"))
	require.NoError(t, err)
	require.NoError(t, routes.Start(ctx))
	assert.Eventually(t, func() bool { return cc.count("idle") == 1 }, 5*time.Second, 10*time.Millisecond,
		"swept without any other open")
	_, err = h.handles.add(ctx, "/d1", "n1", "boot", cc.handle("open"))
	require.NoError(t, err)
	require.NoError(t, routes.Stop(ctx))
	assert.Equal(t, 1, cc.count("open"))
	require.NoError(t, routes.Stop(ctx), "stopping twice")
}

func TestSweepInterval(t *testing.T) {
	assert.Equal(t, 5*time.Minute, sweepInterval(time.Hour), "the default idle timeout")
	assert.Equal(t, 2*time.Second, sweepInterval(8*time.Second))
	assert.Equal(t, time.Second, sweepInterval(time.Millisecond))
}
