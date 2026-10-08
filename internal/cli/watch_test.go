package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
)

// covers: C-OBJ-06, TCL-E-07
func TestWatchDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	var (
		mu     sync.Mutex
		events []cli.WatchEvent
	)
	seen := func(ev cli.WatchEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			if e == ev {
				return true
			}
		}
		return false
	}
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- cli.WatchDir(ctx, dir, 20*time.Millisecond, func() error { close(ready); return nil }, func(_ context.Context, ev cli.WatchEvent) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
			return nil
		}, func(err error) { t.Logf("watch: %v", err) })
	}()
	<-ready

	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o600))
	assert.Eventually(t, func() bool { return seen(cli.WatchEvent{Rel: "new.txt"}) }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "deep", "f.txt"), []byte("f"), 0o600))
	assert.Eventually(t, func() bool { return seen(cli.WatchEvent{Rel: "sub/deep/f.txt"}) }, 5*time.Second, 10*time.Millisecond)

	// Files of a new directory created after the watch started are seen.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "deep", "g.txt"), []byte("g"), 0o600))
	assert.Eventually(t, func() bool { return seen(cli.WatchEvent{Rel: "sub/deep/g.txt"}) }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, os.Remove(filepath.Join(dir, "existing.txt")))
	assert.Eventually(t, func() bool { return seen(cli.WatchEvent{Rel: "existing.txt", Removed: true}) }, 5*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a canceled watch ends without error")
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not stop")
	}

	err := cli.WatchDir(context.Background(), filepath.Join(dir, "missing"), 0, nil, nil, nil)
	assert.Error(t, err)
	err = cli.WatchDir(context.Background(), dir, 0, func() error { return errors.New("sync failed") }, nil, nil)
	assert.EqualError(t, err, "sync failed")
}
