package scanner_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	clustermock "github.com/r3n3r0/nexo/internal/service/cluster/mock"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	scannermock "github.com/r3n3r0/nexo/internal/service/scanner/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

const blockSize = 64 << 10

// deployment is an SNMD object layer of n drives (one erasure set) whose
// bucket "ver" is versioned.
type deployment struct {
	roots   []string
	drives  []storage.StorageAPI
	objects object.Service
	healer  object.Healer
}

func newDeployment(t *testing.T, n int) *deployment {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	if dir, err := os.MkdirTemp("/dev/shm", "nexo-scanner-"); err == nil {
		base = dir
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	topo := topology.New(topology.Deps{Logger: quiet()})
	layout, err := topo.Parse(ctx, []string{fmt.Sprintf("%s/d{1...%d}", base, n)})
	require.NoError(t, err)
	d := &deployment{}
	for _, ep := range layout.Pools[0].Endpoints {
		drive, err := storage.NewLocal(storage.Deps{Logger: quiet()}, ep)
		require.NoError(t, err)
		t.Cleanup(func() { _ = drive.Close(ctx) })
		d.drives = append(d.drives, drive)
		d.roots = append(d.roots, ep.Path)
	}
	id, err := storage.NewFormatter(storage.FormatterDeps{Topology: topo, Logger: quiet()}).EnsureFormats(ctx, layout, d.drives)
	require.NoError(t, err)
	buckets := objectmock.NewBucketConfigSource(t)
	buckets.EXPECT().BucketConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, b string) (object.BucketConfig, error) {
		if b == "ver" {
			return object.BucketConfig{Versioning: domain.VersioningEnabled}, nil
		}
		return object.BucketConfig{}, nil
	}).Maybe()
	d.objects, err = object.NewServerPools(object.PoolsDeps{
		Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{d.drives}}}, DeploymentID: id, Buckets: buckets, Logger: quiet(),
		Codecs: erasure.NewFactory(erasure.Deps{Logger: quiet()}, erasure.WithDeploymentID(id)),
	}, object.WithBlockSize(blockSize), object.WithInlineLimit(1024))
	require.NoError(t, err)
	d.healer = d.objects.(object.Healer)
	return d
}

func put(t *testing.T, svc object.Service, bucket, key string, size int) {
	t.Helper()
	data := bytes.Repeat([]byte{byte(len(key))}, size)
	_, err := svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: key, Reader: bytes.NewReader(data), Size: int64(size)})
	require.NoError(t, err)
}

// populate fills two buckets: "plain" with objects at the root and under
// two prefixes, "ver" with versions and a delete marker.
func populate(t *testing.T, svc object.Service) {
	t.Helper()
	ctx := context.Background()
	for _, b := range []string{"plain", "ver"} {
		require.NoError(t, svc.MakeBucket(ctx, b, object.MakeBucketOptions{}))
	}
	put(t, svc, "plain", "root.txt", 10)
	put(t, svc, "plain", "photos/a.jpg", 2000)
	put(t, svc, "plain", "photos/2026/b.jpg", 3*blockSize)
	put(t, svc, "plain", "docs/c.pdf", 300)
	put(t, svc, "ver", "k", 100)
	put(t, svc, "ver", "k", 200)
	put(t, svc, "ver", "gone", 50)
	_, err := svc.DeleteObject(ctx, "ver", "gone", object.DeleteObjectOptions{})
	require.NoError(t, err)
}

func TestNewRequiresObjects(t *testing.T) {
	_, err := scanner.New(scanner.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestUsage: a cycle computes the usage of every bucket and prefix, equal
// to the walk of the object layer, saves it on the drives where another
// node reads it.
func TestUsage(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4)
	populate(t, d.objects)
	s, err := scanner.New(scanner.Deps{Objects: d.objects, Healer: d.healer, Drives: d.drives, Logger: quiet()}, scanner.WithSpeed(scanner.SpeedFastest))
	require.NoError(t, err)
	_, err = s.Usage(ctx)
	require.ErrorIs(t, err, domain.ErrNotFound, "no cycle yet")

	stats, err := s.Scan(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), stats.Cycle)
	assert.Equal(t, uint64(2), stats.Buckets)
	assert.Equal(t, uint64(6), stats.Objects)
	assert.Equal(t, uint64(8), stats.Versions)
	assert.False(t, stats.Finished.Before(stats.Started))

	u, err := s.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), u.Cycle)
	want := map[string]scanner.Counts{
		"plain": {Objects: 4, Size: 10 + 2000 + 3*blockSize + 300},
		"ver":   {Objects: 1, Versions: 2, DeleteMarkers: 1, Size: 200 + 100 + 50},
	}
	walked, err := d.objects.DataUsage(ctx, "")
	require.NoError(t, err)
	for _, b := range walked {
		assert.Equal(t, want[b.Name], u.Buckets[b.Name].Counts, b.Name)
		assert.Equal(t, scanner.Counts{Objects: b.Objects, Versions: b.Versions, DeleteMarkers: b.DeleteMarkers, Size: b.Size},
			u.Buckets[b.Name].Counts, "%s: same counters as the walk of the object layer", b.Name)
	}
	assert.Equal(t, map[string]scanner.Counts{
		"":        {Objects: 1, Size: 10},
		"photos/": {Objects: 2, Size: 2000 + 3*blockSize},
		"docs/":   {Objects: 1, Size: 300},
	}, u.Buckets["plain"].Prefixes)

	// Another node reads the usage.json of the drives.
	other, err := scanner.New(scanner.Deps{Objects: d.objects, Drives: d.drives, Logger: quiet()})
	require.NoError(t, err)
	remote, err := other.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, u.Buckets, remote.Buckets)
	assert.Equal(t, u.LastUpdate.UnixNano(), remote.LastUpdate.UnixNano())
	// ... and continues the numbering of the cycles when it starts.
	require.NoError(t, other.Start(ctx))
	require.NoError(t, other.Stop(ctx))
	assert.Equal(t, uint64(1), other.Status(ctx).Cycle)

	// A bucket removed and a prefix limit.
	require.NoError(t, d.objects.DeleteBucket(ctx, "ver", object.DeleteBucketOptions{Force: true}))
	limited, err := scanner.New(scanner.Deps{Objects: d.objects, Logger: quiet()}, scanner.WithMaxPrefixes(2), scanner.WithPageSize(1))
	require.NoError(t, err)
	_, err = limited.Scan(ctx)
	require.NoError(t, err)
	u, err = limited.Usage(ctx)
	require.NoError(t, err)
	assert.NotContains(t, u.Buckets, "ver")
	assert.Len(t, u.Buckets["plain"].Prefixes, 2)
	assert.True(t, u.Buckets["plain"].PrefixesTruncated)
	assert.Equal(t, want["plain"], u.Buckets["plain"].Counts, "the totals count every prefix")
}

// TestSingleDrive: the scanner works on the single-drive layer, without
// heal sample.
func TestSingleDrive(t *testing.T) {
	ctx := context.Background()
	drive, err := storage.NewLocal(storage.Deps{Logger: quiet()}, domain.Endpoint{Path: t.TempDir(), IsLocal: true})
	require.NoError(t, err)
	fs, err := object.NewFS(object.Deps{Store: drive, Logger: quiet()})
	require.NoError(t, err)
	require.NoError(t, fs.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, fs, "bkt", "a/b", 5)
	s, err := scanner.New(scanner.Deps{Objects: fs, Drives: []storage.StorageAPI{drive}, Logger: quiet()}, scanner.WithHealSample(1, 1))
	require.NoError(t, err)
	stats, err := s.Scan(ctx)
	require.NoError(t, err)
	assert.Zero(t, stats.HealChecked)
	u, err := s.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, scanner.Counts{Objects: 1, Size: 5}, u.Buckets["bkt"].Counts)
	_, err = drive.ReadAll(ctx, domain.SystemBucket, scanner.UsageFile)
	assert.NoError(t, err)
}

// TestHealSample: the sampled objects are healed, deep on the deep cycles.
func TestHealSample(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4)
	populate(t, d.objects)
	// Drive 2 lost an object; a shard of another is corrupt on drive 3.
	require.NoError(t, os.RemoveAll(filepath.Join(d.roots[1], "plain", "docs")))
	files, err := filepath.Glob(filepath.Join(d.roots[2], "plain", "photos", "2026", "b.jpg", "*", "part.1"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	shard, err := os.ReadFile(files[0])
	require.NoError(t, err)
	bad := append([]byte(nil), shard...)
	bad[100] ^= 1
	require.NoError(t, os.WriteFile(files[0], bad, 0o600))

	// Every object is sampled; deep every second cycle.
	s, err := scanner.New(scanner.Deps{Objects: d.objects, Healer: d.healer, Logger: quiet()},
		scanner.WithHealSample(1, 2), scanner.WithSpeed(scanner.SpeedFastest))
	require.NoError(t, err)
	stats, err := s.Scan(ctx)
	require.NoError(t, err)
	assert.False(t, stats.Deep)
	assert.Equal(t, uint64(6), stats.HealChecked)
	assert.Equal(t, uint64(1), stats.HealRepaired, "the missing object; the bitrot needs a deep cycle")
	_, err = os.Stat(filepath.Join(d.roots[1], "plain", "docs", "c.pdf", storage.MetaFile))
	assert.NoError(t, err)

	stats, err = s.Scan(ctx)
	require.NoError(t, err)
	assert.True(t, stats.Deep)
	assert.Equal(t, uint64(1), stats.HealRepaired)
	fixed, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Equal(t, shard, fixed)
	assert.Equal(t, uint64(2), s.Status(ctx).Last.Cycle)
}

// TestCycleHooks covers the stale uploads cleanup, the lifecycle hook,
// the pause on API load and the throttling of the configured speed.
func TestCycleHooks(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4)
	populate(t, d.objects)
	_, err := d.objects.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "plain", Key: "big"})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	lc := scannermock.NewLifecycle(t)
	lc.EXPECT().Apply(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, obj domain.ObjectInfo) (scanner.LifecycleAction, error) {
		if obj.Key == "root.txt" {
			return scanner.LifecycleDeleted, nil
		}
		return scanner.LifecycleNone, nil
	})
	var busy atomic.Int32
	busy.Store(3)
	load := scannermock.NewLoadSampler(t)
	load.EXPECT().Load(mock.Anything).RunAndReturn(func(context.Context) float64 {
		if busy.Add(-1) >= 0 {
			return 1
		}
		return 0.1
	})
	cfg := config.New(config.Deps{Store: config.NewMemoryStore(map[string]config.KV{scanner.SubsystemScanner: {scanner.KeySpeed: "fast"}})})
	s, err := scanner.New(scanner.Deps{Objects: d.objects, Config: cfg, Load: load, Lifecycle: lc, Logger: quiet()},
		scanner.WithStaleUploads(time.Millisecond), scanner.WithLoadThreshold(0.5, time.Millisecond), scanner.WithPageSize(2))
	require.NoError(t, err)
	stats, err := s.Scan(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.UploadsCleaned)
	assert.Equal(t, uint64(1), stats.LifecycleActions)
	assert.Positive(t, stats.Paused)
	assert.Positive(t, stats.Slept, "the fast speed sleeps as long as it worked")
	u, err := s.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), u.Buckets["plain"].Objects, "the expired version is left out")
	uploads, err := d.objects.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "plain"})
	require.NoError(t, err)
	assert.Empty(t, uploads.Uploads)
}

// TestRunsOnTheLeader: the cycles run while this node leads only.
func TestRunsOnTheLeader(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4)
	populate(t, d.objects)
	var listener cluster.LeaderListener
	unsubscribed := false
	leader := clustermock.NewLeader(t)
	leader.EXPECT().Subscribe(mock.Anything).RunAndReturn(func(l cluster.LeaderListener) func() {
		listener = l
		return func() { unsubscribed = true }
	})
	s, err := scanner.New(scanner.Deps{Objects: d.objects, Leader: leader, Logger: quiet()},
		scanner.WithStartDelay(0), scanner.WithCycle(5*time.Millisecond), scanner.WithSpeed(scanner.SpeedFastest))
	require.NoError(t, err)
	require.NoError(t, s.Start(ctx))
	require.NotNil(t, listener)
	time.Sleep(20 * time.Millisecond)
	assert.False(t, s.Status(ctx).Active)
	assert.Zero(t, s.Status(ctx).Cycle, "not the leader: no cycle")

	term, revoke := context.WithCancel(ctx)
	listener.OnElected(term)
	require.Eventually(t, func() bool { return s.Status(ctx).Last.Cycle >= 2 }, 5*time.Second, 5*time.Millisecond)
	assert.True(t, s.Status(ctx).Active)
	revoke()
	listener.OnRevoked(ctx)
	require.Eventually(t, func() bool { return !s.Status(ctx).Active }, 5*time.Second, 5*time.Millisecond)
	cycles := s.Status(ctx).Cycle
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, cycles, s.Status(ctx).Cycle, "no cycle after the leadership is lost")

	require.NoError(t, s.Stop(ctx))
	assert.True(t, unsubscribed)
	require.NoError(t, s.Stop(ctx), "idempotent")
}

// TestStartWithoutLeader: a single node scans on its own, after the start
// delay, until stopped.
func TestStartWithoutLeader(t *testing.T) {
	ctx := context.Background()
	d := newDeployment(t, 4)
	populate(t, d.objects)
	s, err := scanner.New(scanner.Deps{Objects: d.objects, Drives: d.drives, Logger: quiet()},
		scanner.WithStartDelay(time.Millisecond), scanner.WithCycle(time.Millisecond), scanner.WithSpeed("SLOW"))
	require.NoError(t, err)
	require.NoError(t, s.Start(ctx))
	require.Eventually(t, func() bool { return s.Status(ctx).Last.Cycle >= 2 }, 10*time.Second, 5*time.Millisecond)
	require.NoError(t, s.Stop(ctx))
	st := s.Status(ctx)
	assert.False(t, st.Active)
	assert.False(t, st.Scanning)
	u, err := s.Usage(ctx)
	require.NoError(t, err)
	assert.Len(t, u.Buckets, 2)
}
