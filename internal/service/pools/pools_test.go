package pools_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/pools"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// fixture is a deployment of three pools of one set of four drives.
type fixture struct {
	objects object.Service
	mover   object.PoolMover
	docs    sysstore.Store
	ids     []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	// The tmpfs when there is one: faster, and the placement refuses a
	// pool on a file system over 95% full.
	if dir, err := os.MkdirTemp("/dev/shm", "nexo-pools-"); err == nil {
		base = dir
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	locks := lock.NewLocal(lock.Deps{})
	var specs []object.PoolSpec
	var first []storage.StorageAPI
	for p := 0; p < 3; p++ {
		var set []storage.StorageAPI
		for d := 0; d < 4; d++ {
			set = append(set, testutil.NewLocalDriveIndex(t, filepath.Join(base, fmt.Sprintf("p%d-d%d", p, d)), d))
		}
		if p == 0 {
			first = set
		}
		specs = append(specs, object.PoolSpec{Sets: [][]storage.StorageAPI{set}})
	}
	svc, err := object.NewServerPools(object.PoolsDeps{
		Pools: specs, DeploymentID: "pools-test", Locks: locks,
		Codecs: erasure.NewFactory(erasure.Deps{Logger: slog.New(slog.DiscardHandler)}, erasure.WithDeploymentID("pools-test")),
		Logger: slog.New(slog.DiscardHandler),
	}, object.WithInlineLimit(1<<10), object.WithBlockSize(1<<16), object.WithPoolSpaceTTL(0))
	require.NoError(t, err)
	docs, err := sysstore.New(sysstore.Deps{Drives: first, Locks: locks})
	require.NoError(t, err)
	return &fixture{objects: svc, mover: svc.(object.PoolMover), docs: docs, ids: []string{"/p1/d{1...4}", "/p2/d{1...4}", "/p3/d{1...4}"}}
}

func (f *fixture) service(t *testing.T, mover object.PoolMover) pools.Service {
	t.Helper()
	if mover == nil {
		mover = f.mover
	}
	svc, err := pools.New(pools.Deps{Objects: f.objects, Mover: mover, Docs: f.docs, PoolIDs: f.ids, Logger: slog.New(slog.DiscardHandler)},
		pools.WithPollInterval(20*time.Millisecond), pools.WithBatch(7), pools.WithRetryDelay(0))
	require.NoError(t, err)
	return svc
}

// keysIn lists the keys of a pool in a bucket.
func (f *fixture) keysIn(t *testing.T, pool int, bucket string) []string {
	t.Helper()
	keys, err := f.mover.PoolKeys(context.Background(), pool, bucket, "", 100000)
	require.NoError(t, err)
	return keys
}

func (f *fixture) put(t *testing.T, bucket, key string, data []byte) domain.ObjectInfo {
	t.Helper()
	info, err := f.objects.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: key, Reader: bytes.NewReader(data), Size: int64(len(data))})
	require.NoError(t, err)
	return info
}

func (f *fixture) read(t *testing.T, bucket, key string) []byte {
	t.Helper()
	r, err := f.objects.GetObject(context.Background(), bucket, key, object.GetObjectOptions{})
	require.NoError(t, err, key)
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return data
}

func waitJob(t *testing.T, svc pools.Service, get func(pools.Status) *pools.Job, state pools.JobState) pools.Status {
	t.Helper()
	var st pools.Status
	require.Eventually(t, func() bool {
		var err error
		st, err = svc.Status(context.Background())
		require.NoError(t, err)
		j := get(st)
		return j != nil && j.State == state
	}, 30*time.Second, 20*time.Millisecond)
	return st
}

// TestDecommission (S-STO-09): the objects of a decommissioned pool all
// move to the other pools and read the same; the pool takes no new
// object; the job resumes after a restart and completes once the pool is
// empty and its uploads are over.
//
// covers: S-STO-09
func TestDecommission(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, b := range []string{"alpha", "beta"} {
		require.NoError(t, f.objects.MakeBucket(ctx, b, object.MakeBucketOptions{}))
	}
	content := map[string][]byte{}
	for i := 0; i < 60; i++ {
		b := []string{"alpha", "beta"}[i%2]
		key := fmt.Sprintf("dir%d/obj-%03d", i%5, i)
		data := bytes.Repeat([]byte{byte(i)}, 100+i*997)
		f.put(t, b, key, data)
		content[b+"\x00"+key] = data
	}
	held := len(f.keysIn(t, 1, "alpha")) + len(f.keysIn(t, 1, "beta"))
	require.Positive(t, held, "pool 2 holds objects")

	// An upload in progress in pool 2 holds the decommission back.
	f.mover.SetPoolDraining(0, true)
	f.mover.SetPoolDraining(2, true)
	up, err := f.objects.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "alpha", Key: "late"})
	require.NoError(t, err)
	f.mover.SetPoolDraining(0, false)
	f.mover.SetPoolDraining(2, false)

	svc := f.service(t, nil)
	require.ErrorIs(t, svc.Decommission(ctx, 0), domain.ErrInvalidArgument, "never the first pool")
	require.ErrorIs(t, svc.Decommission(ctx, 7), domain.ErrInvalidArgument)
	require.NoError(t, svc.Decommission(ctx, 1))
	require.NoError(t, svc.Start(ctx))
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	// New objects avoid the draining pool.
	for i := 0; i < 20; i++ {
		f.put(t, "beta", fmt.Sprintf("new-%02d", i), []byte("new"))
	}
	require.Eventually(t, func() bool {
		st, err := svc.Status(ctx)
		require.NoError(t, err)
		j := st.Pools[1].Decommission
		return j != nil && j.PendingUploads == 1
	}, 30*time.Second, 20*time.Millisecond, "every object moved, the upload holds the job")
	assert.Empty(t, f.keysIn(t, 1, "alpha"))
	assert.Empty(t, f.keysIn(t, 1, "beta"))

	// The upload completes in pool 2: the job moves its object, then ends.
	part, err := f.objects.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "alpha", Key: "late", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader([]byte("late object")), Size: 11})
	require.NoError(t, err)
	_, err = f.objects.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "alpha", Key: "late", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: part.ETag}}})
	require.NoError(t, err)
	st := waitJob(t, svc, func(s pools.Status) *pools.Job { return s.Pools[1].Decommission }, pools.JobComplete)
	job := st.Pools[1].Decommission
	assert.Equal(t, int64(held+1), job.Objects)
	assert.True(t, st.Pools[1].Draining)
	assert.Empty(t, f.keysIn(t, 1, "alpha"))
	assert.Empty(t, f.keysIn(t, 1, "beta"))
	for name, want := range content {
		b, k, _ := strings.Cut(name, "\x00")
		assert.Equal(t, want, f.read(t, b, k), name)
	}
	assert.Equal(t, "late object", string(f.read(t, "alpha", "late")))

	// Another node (or this one after a restart) keeps the pool draining.
	svc2 := f.service(t, nil)
	f.mover.SetPoolDraining(1, false)
	require.NoError(t, svc2.Start(ctx))
	require.NoError(t, svc2.Stop(ctx))
	assert.True(t, f.mover.PoolUsage(ctx)[1].Draining)

	// Pools 2 and 3 cannot both go: nothing would hold the objects.
	require.NoError(t, svc.Decommission(ctx, 2))
	require.NoError(t, svc.CancelDecommission(ctx, 2))
	require.ErrorIs(t, svc.StartRebalance(ctx), domain.ErrInvalidArgument, "not during a decommission")
}

// TestDecommissionResumeAndCancel: a job stopped midway resumes from its
// cursor; a canceled decommission makes the pool eligible again.
//
// covers: S-STO-09
func TestDecommissionResumeAndCancel(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	require.NoError(t, f.objects.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	for i := 0; i < 90; i++ {
		f.put(t, "bkt", fmt.Sprintf("k%03d", i), []byte(fmt.Sprintf("v%d", i)))
	}
	before := len(f.keysIn(t, 2, "bkt"))
	require.Positive(t, before)

	svc := f.service(t, slowMover{PoolMover: f.mover, delay: 5 * time.Millisecond})
	require.NoError(t, svc.Decommission(ctx, 2))
	require.NoError(t, svc.Start(ctx))
	require.Eventually(t, func() bool {
		st, err := svc.Status(ctx)
		require.NoError(t, err)
		j := st.Pools[2].Decommission
		return j != nil && j.Objects > 3
	}, 30*time.Second, 5*time.Millisecond)
	require.NoError(t, svc.Stop(ctx))
	st, err := svc.Status(ctx)
	require.NoError(t, err)
	stopped := st.Pools[2].Decommission
	require.Equal(t, pools.JobRunning, stopped.State)

	// A new service (a restart) goes on from there.
	svc = f.service(t, nil)
	require.NoError(t, svc.Start(ctx))
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	st = waitJob(t, svc, func(s pools.Status) *pools.Job { return s.Pools[2].Decommission }, pools.JobComplete)
	assert.Equal(t, int64(before), st.Pools[2].Decommission.Objects, "every object moved once")
	assert.Empty(t, f.keysIn(t, 2, "bkt"))
	for i := 0; i < 90; i++ {
		assert.Equal(t, fmt.Sprintf("v%d", i), string(f.read(t, "bkt", fmt.Sprintf("k%03d", i))))
	}

	require.NoError(t, svc.CancelDecommission(ctx, 2))
	st, err = svc.Status(ctx)
	require.NoError(t, err)
	assert.False(t, st.Pools[2].Draining)
	require.ErrorIs(t, svc.CancelDecommission(ctx, 2), domain.ErrInvalidArgument)
}

// slowMover slows the moves down so that a test can stop a job midway.
type slowMover struct {
	object.PoolMover
	delay time.Duration
}

func (m slowMover) MoveObject(ctx context.Context, in object.MoveObjectInput) (object.MoveObjectResult, error) {
	time.Sleep(m.delay)
	return m.PoolMover.MoveObject(ctx, in)
}

// countingMover reports the usage of a pool by the number of keys it
// holds (out of 100), so that a rebalance sees its own progress.
type countingMover struct {
	object.PoolMover
	f *fixture
	t *testing.T
}

func (m countingMover) PoolUsage(ctx context.Context) []object.PoolUsage {
	out := m.PoolMover.PoolUsage(ctx)
	for i := range out {
		n := len(m.f.keysIn(m.t, i, "bkt"))
		out[i].Total, out[i].Free = 100, uint64(100-n)
	}
	return out
}

// TestRebalance (S-STO-10): the objects of the fullest pool move to the
// emptier ones until every pool is near the average; nothing is lost.
//
// covers: S-STO-10
func TestRebalance(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	require.NoError(t, f.objects.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	// Everything goes to pool 1.
	f.mover.SetPoolDraining(1, true)
	f.mover.SetPoolDraining(2, true)
	for i := 0; i < 60; i++ {
		f.put(t, "bkt", fmt.Sprintf("k%03d", i), []byte(fmt.Sprintf("v%d", i)))
	}
	f.mover.SetPoolDraining(1, false)
	f.mover.SetPoolDraining(2, false)
	require.Len(t, f.keysIn(t, 0, "bkt"), 60)

	svc := f.service(t, countingMover{PoolMover: f.mover, f: f, t: t})
	require.ErrorIs(t, svc.StopRebalance(ctx), domain.ErrInvalidArgument)
	require.NoError(t, svc.StartRebalance(ctx))
	require.ErrorIs(t, svc.Decommission(ctx, 1), domain.ErrInvalidArgument, "not during a rebalance")
	require.NoError(t, svc.Start(ctx))
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	st := waitJob(t, svc, func(s pools.Status) *pools.Job { return s.Rebalance }, pools.JobComplete)
	assert.Positive(t, st.Rebalance.Objects)
	n0 := len(f.keysIn(t, 0, "bkt"))
	assert.LessOrEqual(t, n0, 20+5, "pool 1 is near the average of 20 objects")
	assert.Equal(t, 60, n0+len(f.keysIn(t, 1, "bkt"))+len(f.keysIn(t, 2, "bkt")))
	for i := 0; i < 60; i++ {
		assert.Equal(t, fmt.Sprintf("v%d", i), string(f.read(t, "bkt", fmt.Sprintf("k%03d", i))))
	}
}
