package heal_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// testBlockSize keeps the erasure blocks small: objects of a few blocks
// stay small.
const testBlockSize = 64 << 10

// baseDir returns a fresh directory for the drives, on tmpfs when there
// is one (faster).
func baseDir(t *testing.T) string {
	t.Helper()
	if dir, err := os.MkdirTemp("/dev/shm", "nexo-heal-"); err == nil {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	return t.TempDir()
}

// layoutOf parses base/d{1...n} as the command line of an SNMD server.
func layoutOf(t *testing.T, base string, n int) (*topology.Layout, topology.Service) {
	t.Helper()
	topo := topology.New(topology.Deps{Logger: quiet()})
	layout, err := topo.Parse(context.Background(), []string{fmt.Sprintf("%s/d{1...%d}", base, n)})
	require.NoError(t, err)
	return layout, topo
}

// openDrives opens the drives of base/d{1...n}, closed at the end of the
// test.
func openDrives(t *testing.T, base string, n int) []storage.StorageAPI {
	t.Helper()
	layout, _ := layoutOf(t, base, n)
	var drives []storage.StorageAPI
	for _, ep := range layout.Pools[0].Endpoints {
		d, err := storage.NewLocal(storage.Deps{Logger: quiet()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		drives = append(drives, d)
	}
	return drives
}

// env is an SNMD deployment of n drives in one erasure set: the object
// layer with the MRF queue as its notifier, and the healing service.
type env struct {
	base    string
	roots   []string
	drives  []storage.StorageAPI
	pools   []object.PoolSpec
	queue   heal.Queue
	objects object.Service
	healer  object.Healer
	svc     heal.Service
}

// newEnv formats and opens a deployment in base (a fresh one when empty)
// and builds its services; the queue and the healing service are not
// started.
func newEnv(t *testing.T, base string, n int, opts ...heal.Option) *env {
	t.Helper()
	ctx := context.Background()
	if base == "" {
		base = baseDir(t)
	}
	layout, topo := layoutOf(t, base, n)
	e := &env{base: base, drives: openDrives(t, base, n)}
	for _, ep := range layout.Pools[0].Endpoints {
		e.roots = append(e.roots, ep.Path)
	}
	id, err := storage.NewFormatter(storage.FormatterDeps{Topology: topo, Logger: quiet()}).EnsureFormats(ctx, layout, e.drives)
	require.NoError(t, err)
	e.pools = []object.PoolSpec{{Sets: [][]storage.StorageAPI{e.drives}}}
	e.queue = heal.NewQueue(heal.QueueDeps{Drives: e.drives, Logger: quiet()}, heal.WithBackoff(5*time.Millisecond, 50*time.Millisecond))
	e.objects, err = object.NewServerPools(object.PoolsDeps{Pools: e.pools, DeploymentID: id, Heal: e.queue, Logger: quiet(),
		Codecs: erasure.NewFactory(erasure.Deps{Logger: quiet()}, erasure.WithDeploymentID(id))},
		object.WithBlockSize(testBlockSize), object.WithInlineLimit(16<<10))
	require.NoError(t, err)
	e.healer = e.objects.(object.Healer)
	e.svc, err = heal.New(heal.Deps{Healer: e.healer, Pools: e.pools, Queue: e.queue, Logger: quiet()},
		append([]heal.Option{heal.WithMonitorInterval(0)}, opts...)...)
	require.NoError(t, err)
	return e
}

// start starts the queue and the healing service, stopped at the end of
// the test.
func (e *env) start(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, e.queue.Start(ctx))
	require.NoError(t, e.svc.Start(ctx))
	t.Cleanup(func() {
		require.NoError(t, e.svc.Stop(ctx))
		require.NoError(t, e.queue.Stop(ctx))
	})
}

func payload(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

func (e *env) put(t *testing.T, bucket, key string, data []byte) {
	t.Helper()
	_, err := e.objects.PutObject(context.Background(), object.PutObjectInput{
		Bucket: bucket, Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)),
	})
	require.NoError(t, err)
}

func (e *env) get(t *testing.T, bucket, key string) []byte {
	t.Helper()
	r, err := e.objects.GetObject(context.Background(), bucket, key, object.GetObjectOptions{})
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

// seed creates bucket with count objects, alternately inline and erasure
// coded, and returns their content by key.
func (e *env) seed(t *testing.T, bucket string, count int) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	err := e.objects.MakeBucket(ctx, bucket, object.MakeBucketOptions{})
	if err != nil {
		require.ErrorIs(t, err, domain.ErrBucketAlreadyExists)
	}
	out := map[string][]byte{}
	for i := range count {
		key := fmt.Sprintf("dir%d/obj-%03d", i%3, i)
		size := 100 + i
		if i%2 == 1 {
			size = 2*testBlockSize + 17*i
		}
		data := payload(size, byte(i))
		e.put(t, bucket, key, data)
		out[key] = data
	}
	return out
}

// hasObject reports whether drive i holds the metadata of bucket/key.
func (e *env) hasObject(i int, bucket, key string) bool {
	_, err := os.Stat(filepath.Join(e.roots[i], bucket, filepath.FromSlash(key), storage.MetaFile))
	return err == nil
}

// shardOn returns the path of the first part file of bucket/key on drive i.
func (e *env) shardOn(t *testing.T, i int, bucket, key string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(e.roots[i], bucket, filepath.FromSlash(key), "*", "part.1"))
	require.NoError(t, err)
	require.Len(t, files, 1, "%s/%s on drive %d", bucket, key, i+1)
	return files[0]
}

// takeOffline renames the root of drive i away (the drive is offline)
// and returns the function that puts it back.
func (e *env) takeOffline(t *testing.T, i int) func() {
	t.Helper()
	moved := e.roots[i] + ".away"
	require.NoError(t, os.Rename(e.roots[i], moved))
	return func() { require.NoError(t, os.Rename(moved, e.roots[i])) }
}

// wipe empties drive i, as a replaced drive: its root exists, empty. The
// old content leaves the root at once (a rename) and is removed out of
// reach of the drive: a running monitor formats a drive as soon as it
// finds it empty, and its writes would race a removal in place
// (os.RemoveAll of the root then fails with "directory not empty").
func (e *env) wipe(t *testing.T, i int) {
	t.Helper()
	trash, err := os.MkdirTemp(e.base, "wiped-")
	require.NoError(t, err)
	require.NoError(t, os.Rename(e.roots[i], filepath.Join(trash, "root")))
	require.NoError(t, os.MkdirAll(e.roots[i], 0o750))
	require.NoError(t, os.RemoveAll(trash))
}

// TestWipeUnderWriter: wipe empties a drive that is written meanwhile, as
// the drive monitor does when it formats a drive it finds empty
// (regression: os.RemoveAll of the root failed with "directory not empty"
// when the monitor of TestMonitor formatted the drive halfway through).
func TestWipeUnderWriter(t *testing.T) {
	e := newEnv(t, "", 4)
	e.seed(t, "bkt", 6)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		sys := filepath.Join(e.roots[0], domain.SystemBucket)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// The errors are expected: the directory may be gone halfway.
			_ = os.MkdirAll(sys, 0o750)
			_ = os.WriteFile(filepath.Join(sys, storage.FormatFile), []byte("{}"), 0o600)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()
	for range 50 {
		e.wipe(t, 0)
	}
}
