package heal_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

// brokenWalk returns base behind the generated mock: every call is
// delegated, except the object walks of bucket, which break after a few
// entries while broken is set (a remote drive whose walk stream fails).
func brokenWalk(t *testing.T, base storage.StorageAPI, bucket string, broken *atomic.Bool) storage.StorageAPI {
	t.Helper()
	m := storagemock.NewStorageAPI(t)
	m.EXPECT().Endpoint().RunAndReturn(base.Endpoint).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(base.IsOnline).Maybe()
	m.EXPECT().Healing().RunAndReturn(base.Healing).Maybe()
	m.EXPECT().DiskInfo(mock.Anything, mock.Anything).RunAndReturn(base.DiskInfo).Maybe()
	m.EXPECT().MakeVol(mock.Anything, mock.Anything).RunAndReturn(base.MakeVol).Maybe()
	m.EXPECT().ListVols(mock.Anything).RunAndReturn(base.ListVols).Maybe()
	m.EXPECT().StatVol(mock.Anything, mock.Anything).RunAndReturn(base.StatVol).Maybe()
	m.EXPECT().DeleteVol(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.DeleteVol).Maybe()
	m.EXPECT().ReadVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadVersion).Maybe()
	m.EXPECT().WriteMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.WriteMetadata).Maybe()
	m.EXPECT().UpdateMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.UpdateMetadata).Maybe()
	m.EXPECT().DeleteVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.DeleteVersion).Maybe()
	m.EXPECT().CreateFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.CreateFile).Maybe()
	m.EXPECT().ReadFileStream(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadFileStream).Maybe()
	m.EXPECT().RenameData(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.RenameData).Maybe()
	m.EXPECT().RenameFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.RenameFile).Maybe()
	m.EXPECT().ReadAll(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadAll).Maybe()
	m.EXPECT().WriteAll(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.WriteAll).Maybe()
	m.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.Delete).Maybe()
	m.EXPECT().CheckParts(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.CheckParts).Maybe()
	m.EXPECT().StatInfoFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.StatInfoFile).Maybe()
	m.EXPECT().Close(mock.Anything).RunAndReturn(base.Close).Maybe()
	m.EXPECT().WalkDir(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, opts storage.WalkDirOptions, fn storage.WalkDirFunc) error {
			if !broken.Load() || opts.Bucket != bucket {
				return base.WalkDir(ctx, opts, fn)
			}
			n := 0
			err := base.WalkDir(ctx, opts, func(we storage.WalkEntry) error {
				if n == 3 {
					return errors.New("injected: walk stream broken")
				}
				n++
				return fn(we)
			})
			return fmt.Errorf("%w: %w", domain.ErrDiskNotFound, err)
		}).Maybe()
	return m
}

// countObjects returns how many objects of bucket drive i holds.
func (e *env) countObjects(t *testing.T, i int, bucket string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(e.roots[i], bucket, "dir*", "obj-*", storage.MetaFile))
	require.NoError(t, err)
	return len(matches)
}

// TestHealDiskWalkInterrupted (TC-10): when the walks of the source drives
// break midway, the drive is not declared healed: the tracker stays, and
// the next run resumes and heals every object.
func TestHealDiskWalkInterrupted(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	e.seed(t, "bkt", 20)
	e.wipe(t, 0)
	_, err := e.svc.HealFormat(ctx, heal.Options{})
	require.NoError(t, err)
	ep := e.drives[0].Endpoint().String()

	broken := &atomic.Bool{}
	broken.Store(true)
	drives := []storage.StorageAPI{e.drives[0]}
	for _, d := range e.drives[1:] {
		drives = append(drives, brokenWalk(t, d, "bkt", broken))
	}
	svc, err := heal.New(heal.Deps{Healer: e.healer, Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}}, Logger: quiet()},
		heal.WithMonitorInterval(0))
	require.NoError(t, err)
	_, err = svc.HealDisk(ctx, ep)
	require.ErrorIs(t, err, domain.ErrDiskNotFound, "an interrupted walk is not a complete one")
	tracker := e.drives[0].Healing()
	require.NotNil(t, tracker, "the drive is still marked for healing")
	assert.Empty(t, tracker.HealedBuckets)
	assert.Less(t, e.countObjects(t, 0, "bkt"), 20)

	broken.Store(false)
	res, err := svc.HealDisk(ctx, ep)
	require.NoError(t, err)
	assert.True(t, res.Resumed)
	assert.Nil(t, e.drives[0].Healing(), "healed: the tracker is removed")
	assert.Equal(t, 20, e.countObjects(t, 0, "bkt"))
}

// TestHealDiskRetriesFailedObjects: objects that failed to heal (sources
// momentarily without quorum) keep the drive marked for healing: a new
// pass examines them again, up to the configured retries.
func TestHealDiskRetriesFailedObjects(t *testing.T) {
	tests := []struct {
		name    string
		retries int
		// fails is how many passes the object fails in.
		fails       int
		wantRuns    int
		wantHealedK bool
	}{
		{name: "healed at the second pass", retries: 3, fails: 1, wantRuns: 2, wantHealedK: true},
		{name: "retries exhausted", retries: 1, fails: 5, wantRuns: 2, wantHealedK: false},
		{name: "no retry", retries: 0, fails: 1, wantRuns: 1, wantHealedK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t, "", 4)
			objects := e.seed(t, "bkt", 6)
			e.wipe(t, 1)
			_, err := e.svc.HealFormat(ctx, heal.Options{})
			require.NoError(t, err)
			const flaky = "dir1/obj-001"
			var failures atomic.Int32
			healer := objectmock.NewHealer(t)
			healer.EXPECT().HealBucket(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(e.healer.HealBucket).Maybe()
			healer.EXPECT().HealObject(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
				func(ctx context.Context, bucket, key, versionID string, opts object.HealOptions) (object.HealResult, error) {
					if key == flaky && failures.Load() < int32(tc.fails) {
						failures.Add(1)
						return object.HealResult{}, fmt.Errorf("%w: injected", domain.ErrReadQuorum)
					}
					return e.healer.HealObject(ctx, bucket, key, versionID, opts)
				}).Maybe()
			svc, err := heal.New(heal.Deps{Healer: healer, Pools: e.pools, Logger: quiet()},
				heal.WithMonitorInterval(0), heal.WithDiskRetries(tc.retries))
			require.NoError(t, err)
			ep := e.drives[1].Endpoint().String()
			runs := 0
			for {
				runs++
				_, err := svc.HealDisk(ctx, ep)
				if err == nil {
					break
				}
				tracker := e.drives[1].Healing()
				require.NotNil(t, tracker, "run %d: the drive stays marked for healing", runs)
				assert.Equal(t, runs, tracker.RetryAttempts)
				assert.Zero(t, tracker.ObjectsFailed, "the new pass starts with no failure")
				require.Less(t, runs, 10)
			}
			assert.Equal(t, tc.wantRuns, runs)
			assert.Nil(t, e.drives[1].Healing())
			assert.Equal(t, tc.wantHealedK, e.hasObject(1, "bkt", flaky))
			for key := range objects {
				if key != flaky {
					assert.True(t, e.hasObject(1, "bkt", key), key)
				}
			}
		})
	}
}

// TestMRFEntryKeptOnShutdown: an MRF entry being healed when the node
// stops is not healed: it stays queued and is persisted for the next run.
func TestMRFEntryKeptOnShutdown(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	started := make(chan struct{}, 1)
	healer := objectmock.NewHealer(t)
	healer.EXPECT().HealObject(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _, _, _ string, _ object.HealOptions) (object.HealResult, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return object.HealResult{}, ctx.Err()
		})
	require.NoError(t, e.queue.Start(ctx))
	svc, err := heal.New(heal.Deps{Healer: healer, Pools: e.pools, Queue: e.queue, Logger: quiet()},
		heal.WithMonitorInterval(0), heal.WithMRFWorkers(1))
	require.NoError(t, err)
	require.NoError(t, svc.Start(ctx))
	e.queue.NotifyHeal(ctx, "bkt", "obj", "")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the MRF worker did not start healing")
	}
	// Graceful shutdown: the healing service, then the queue.
	require.NoError(t, svc.Stop(ctx))
	require.NoError(t, e.queue.Stop(ctx))
	st := e.queue.Stats(ctx)
	assert.Equal(t, 1, st.Pending)
	assert.Zero(t, st.Healed)

	restarted := heal.NewQueue(heal.QueueDeps{Drives: e.drives, Logger: quiet()})
	require.NoError(t, restarted.Start(ctx))
	defer func() { require.NoError(t, restarted.Stop(ctx)) }()
	assert.Equal(t, 1, restarted.Stats(ctx).Pending, "the interrupted entry survives the restart")
	got, err := nextWithin(t, restarted, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "obj", got.Object)
	assert.Zero(t, got.Attempts, "an interrupted heal is not a failed attempt")
}

// TestQueueWaitsForOfflineDrive: an entry that waits for an offline drive
// backs off but is never abandoned, however long the drive stays away.
func TestQueueWaitsForOfflineDrive(t *testing.T) {
	ctx := context.Background()
	fake := clock.NewFake(time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC))
	q := heal.NewQueue(heal.QueueDeps{Clock: fake, Logger: quiet()}, heal.WithBackoff(time.Second, 8*time.Second), heal.WithMaxAttempts(3))
	q.NotifyHeal(ctx, "b", "k", "")
	var delays []time.Duration
	e, err := nextWithin(t, q, time.Second)
	require.NoError(t, err)
	for range 6 {
		q.Release(ctx, e, true)
		start := fake.Now()
		for {
			got, err := nextWithin(t, q, 5*time.Millisecond)
			if err == nil {
				e = got
				break
			}
			fake.Advance(time.Second)
		}
		delays = append(delays, fake.Now().Sub(start))
	}
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}, delays)
	st := q.Stats(ctx)
	assert.Zero(t, st.Dropped, "never abandoned while waiting for a drive")
	assert.Equal(t, 1, st.Pending+st.Leased)
}
