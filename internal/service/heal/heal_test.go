package heal_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

const (
	ok      = object.HealDriveOK
	missing = object.HealDriveMissing
	corrupt = object.HealDriveCorrupt
	offline = object.HealDriveOffline
)

func states(drives []object.HealDrive) (before, after []object.HealDriveState) {
	for _, d := range drives {
		before = append(before, d.Before)
		after = append(after, d.After)
	}
	return before, after
}

func TestNewRequiresDeps(t *testing.T) {
	healer := objectmock.NewHealer(t)
	tests := []struct {
		name string
		deps heal.Deps
	}{
		{"no healer", heal.Deps{Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{{}}}}}},
		{"no pools", heal.Deps{Healer: healer}},
		{"empty pool", heal.Deps{Healer: healer, Pools: []object.PoolSpec{{}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := heal.New(tc.deps)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

// TestHealBitrot is TS-E-04: a flipped bit in a shard is reported corrupt
// by the deep check, the object stays readable, and the shard is rebuilt
// byte for byte, on demand and by the MRF queue after a read met it.
func TestHealBitrot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	objects := e.seed(t, "photos", 4)
	key := "dir1/obj-001" // erasure coded
	// The drive of the first data shard: always read by a GET.
	drive := -1
	for i, d := range e.drives {
		fi, err := d.ReadVersion(ctx, "photos", key, "", storage.ReadOptions{})
		require.NoError(t, err)
		if fi.Erasure.Index == 1 {
			drive = i
		}
	}
	require.GreaterOrEqual(t, drive, 0)
	path := e.shardOn(t, drive, "photos", key)
	wantStates := []object.HealDriveState{ok, ok, ok, ok}
	wantStates[drive] = corrupt
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	flip := func() {
		bad := append([]byte(nil), want...)
		bad[len(bad)/2] ^= 0x10
		require.NoError(t, os.WriteFile(path, bad, 0o600))
	}
	flip()

	// A shallow check sees nothing: the size is right.
	res, err := e.svc.HealObject(ctx, "photos", key, "", heal.Options{})
	require.NoError(t, err)
	assert.Zero(t, res.Healed)
	// The deep dry run finds it and changes nothing.
	res, err = e.svc.HealObject(ctx, "photos", key, "", heal.Options{Deep: true, DryRun: true})
	require.NoError(t, err)
	before, after := states(res.Drives)
	assert.Equal(t, wantStates, before)
	assert.Equal(t, before, after)
	assert.Equal(t, 1, res.Healed)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, want, got, "dry run")
	// The deep heal rebuilds it.
	res, err = e.svc.HealObject(ctx, "photos", key, "", heal.Options{Deep: true})
	require.NoError(t, err)
	_, after = states(res.Drives)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, after)
	got, err = os.ReadFile(e.shardOn(t, drive, "photos", key))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, int64(len(objects[key])), res.Bytes)

	// Heal-on-read: a GET serves the right content and queues the object,
	// the MRF worker repairs the shard.
	e.start(t)
	flip()
	assert.Equal(t, objects[key], e.get(t, "photos", key))
	require.Eventually(t, func() bool {
		files, _ := filepath.Glob(filepath.Join(e.roots[drive], "photos", key, "*", "part.1"))
		if len(files) != 1 {
			return false
		}
		got, err := os.ReadFile(files[0])
		return err == nil && string(got) == string(want)
	}, 10*time.Second, 20*time.Millisecond, "the MRF worker rebuilds the shard")
	require.Eventually(t, func() bool { return e.queue.Stats(ctx).Pending == 0 }, 5*time.Second, 10*time.Millisecond)

	_, err = e.svc.HealObject(ctx, "photos", "nope", "", heal.Options{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

// TestMRFOfflineDrive: a write that misses an offline drive is queued and
// retried with backoff until the drive is back, then healed.
func TestMRFOfflineDrive(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	e.seed(t, "docs", 1)
	e.start(t)
	back := e.takeOffline(t, 1)
	data := payload(3*testBlockSize+5, 7)
	e.put(t, "docs", "late", data)
	e.put(t, "docs", "small", []byte("inline"))
	// Retried while the drive is away.
	require.Eventually(t, func() bool { return e.queue.Stats(ctx).Retried >= 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, 2, e.queue.Stats(ctx).Pending+e.queue.Stats(ctx).Leased)
	back()
	require.Eventually(t, func() bool {
		return e.hasObject(1, "docs", "late") && e.hasObject(1, "docs", "small") && e.queue.Stats(ctx).Pending+e.queue.Stats(ctx).Leased == 0
	}, 10*time.Second, 20*time.Millisecond, "healed once the drive is back")
	res, err := e.svc.HealObject(ctx, "docs", "late", "", heal.Options{Deep: true, DryRun: true})
	require.NoError(t, err)
	before, _ := states(res.Drives)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, before)
}

// TestHealPrefixDangling heals a prefix, then finds and removes (only on
// request) a version written on one drive only.
func TestHealPrefixDangling(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	objects := e.seed(t, "bkt", 6)
	// Drive 4 lost two objects; another one exists on drive 1 only.
	require.NoError(t, os.RemoveAll(filepath.Join(e.roots[3], "bkt", "dir0")))
	ghost := storage.FileInfo{ModTime: time.Now().UTC(), Size: 5, Data: []byte("ghost"), Metadata: map[string]string{"etag": "x"}}
	require.NoError(t, e.drives[0].WriteMetadata(ctx, "bkt", "dir0/ghost", ghost))

	var seen []string
	sum, err := e.svc.HealPrefix(ctx, "bkt", "dir0/", heal.Options{DryRun: true}, func(res heal.ObjectResult, err error) {
		seen = append(seen, res.Object)
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"dir0/ghost", "dir0/obj-000", "dir0/obj-003"}, seen, "the union of the drives, in key order")
	assert.Equal(t, heal.Summary{Buckets: 1, Objects: 3, Healed: 2, Dangling: 1, Bytes: uint64(len(objects["dir0/obj-000"]) + len(objects["dir0/obj-003"]))}, sum)
	assert.False(t, e.hasObject(3, "bkt", "dir0/obj-000"), "dry run")

	sum, err = e.svc.HealPrefix(ctx, "bkt", "", heal.Options{}, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), sum.Objects)
	assert.Equal(t, uint64(2), sum.Healed)
	assert.Equal(t, uint64(1), sum.Dangling)
	assert.Zero(t, sum.Removed, "dangling versions are only removed on request")
	assert.True(t, e.hasObject(3, "bkt", "dir0/obj-000"))
	assert.True(t, e.hasObject(0, "bkt", "dir0/ghost"))

	sum, err = e.svc.HealPrefix(ctx, "", "", heal.Options{Remove: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), sum.Removed)
	assert.False(t, e.hasObject(0, "bkt", "dir0/ghost"))

	_, err = e.svc.HealPrefix(ctx, "missing", "", heal.Options{}, nil)
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

// TestHealFormatAndDisk is TS-E-05: a drive emptied and put back is
// formatted with its position and marked for healing, then healed
// completely: buckets, objects and system files.
func TestHealFormatAndDisk(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	objects := e.seed(t, "alpha", 8)
	for k, v := range e.seed(t, "beta", 4) {
		objects["beta/"+k] = v
	}
	sysFile := storage.ConfigDir + "/iam/users.json"
	for _, d := range e.drives {
		require.NoError(t, d.WriteAll(ctx, domain.SystemBucket, sysFile, []byte(`{"users":1}`)))
	}
	formatBefore, err := os.ReadFile(filepath.Join(e.roots[2], domain.SystemBucket, storage.FormatFile))
	require.NoError(t, err)
	e.wipe(t, 2)

	res, err := e.svc.HealFormat(ctx, heal.Options{DryRun: true})
	require.NoError(t, err)
	before, after := states(res.Drives)
	assert.Equal(t, []object.HealDriveState{ok, ok, missing, ok}, before)
	assert.Equal(t, before, after)
	assert.Equal(t, []string{e.drives[2].Endpoint().String()}, res.Healed)
	assert.Nil(t, e.drives[2].Healing(), "dry run")

	res, err = e.svc.HealFormat(ctx, heal.Options{})
	require.NoError(t, err)
	_, after = states(res.Drives)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, after)
	formatAfter, err := os.ReadFile(filepath.Join(e.roots[2], domain.SystemBucket, storage.FormatFile))
	require.NoError(t, err)
	assert.JSONEq(t, string(formatBefore), string(formatAfter), "the drive gets back the document of its position")
	tracker := e.drives[2].Healing()
	require.NotNil(t, tracker)
	assert.Equal(t, 2, tracker.DiskIndex)

	dr, err := e.svc.HealDisk(ctx, e.drives[2].Endpoint().String())
	require.NoError(t, err)
	assert.False(t, dr.Resumed)
	assert.Equal(t, 2, dr.Buckets)
	assert.Equal(t, uint64(12), dr.Objects)
	assert.Equal(t, uint64(12), dr.Healed)
	assert.Zero(t, dr.Failed)
	assert.Nil(t, e.drives[2].Healing(), "the tracker is removed once healed")
	for k := range objects {
		bucket, key := "alpha", k
		if len(k) > 5 && k[:5] == "beta/" {
			bucket, key = "beta", k[5:]
		}
		assert.True(t, e.hasObject(2, bucket, key), "%s/%s on the healed drive", bucket, key)
	}
	data, err := e.drives[2].ReadAll(ctx, domain.SystemBucket, sysFile)
	require.NoError(t, err)
	assert.JSONEq(t, `{"users":1}`, string(data))

	// The healed drive with D-1 others serves everything.
	defer e.takeOffline(t, 0)()
	defer e.takeOffline(t, 1)()
	for k, want := range objects {
		bucket, key := "alpha", k
		if len(k) > 5 && k[:5] == "beta/" {
			bucket, key = "beta", k[5:]
		}
		assert.Equal(t, want, e.get(t, bucket, key), "%s/%s", bucket, key)
	}

	_, err = e.svc.HealDisk(ctx, "/nowhere")
	assert.ErrorIs(t, err, domain.ErrDiskNotFound)
}

// TestHealDiskResume is the second half of TS-E-05: a drive healing
// interrupted halfway (the process killed) resumes after a restart from
// its checkpoint instead of starting over.
func TestHealDiskResume(t *testing.T) {
	ctx := context.Background()
	base := baseDir(t)
	e := newEnv(t, base, 4)
	e.seed(t, "alpha", 6)
	e.seed(t, "beta", 6)
	e.wipe(t, 3)
	_, err := e.svc.HealFormat(ctx, heal.Options{})
	require.NoError(t, err)
	ep := e.drives[3].Endpoint().String()

	// The healing stops after 8 objects: alpha done, beta in progress.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var calls atomic.Int32
	interrupted := objectmock.NewHealer(t)
	interrupted.EXPECT().HealBucket(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(e.healer.HealBucket).Maybe()
	interrupted.EXPECT().HealObject(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, bucket, key, versionID string, opts object.HealOptions) (object.HealResult, error) {
			if calls.Add(1) == 9 {
				cancel()
				return object.HealResult{}, context.Canceled
			}
			return e.healer.HealObject(ctx, bucket, key, versionID, opts)
		})
	first, err := heal.New(heal.Deps{Healer: interrupted, Pools: e.pools, Logger: quiet()}, heal.WithMonitorInterval(0), heal.WithCheckpoint(3, time.Hour))
	require.NoError(t, err)
	_, err = first.HealDisk(runCtx, ep)
	require.ErrorIs(t, err, context.Canceled)
	tracker := e.drives[3].Healing()
	require.NotNil(t, tracker, "the checkpoint survives")
	assert.Equal(t, []string{"alpha"}, tracker.HealedBuckets)
	assert.Equal(t, "beta", tracker.Bucket)
	assert.Equal(t, "dir0/obj-003", tracker.Object, "the last object healed")
	assert.Equal(t, uint64(8), tracker.ObjectsHealed)

	// Restart: new drives and services on the same directories.
	again := newEnv(t, base, 4)
	require.NotNil(t, again.drives[3].Healing(), "the tracker is read back at startup")
	dr, err := again.svc.HealDisk(ctx, ep)
	require.NoError(t, err)
	assert.True(t, dr.Resumed)
	assert.Equal(t, 1, dr.Buckets, "alpha is not healed again")
	assert.Equal(t, uint64(4), dr.Objects, "beta resumes after its last object")
	assert.Equal(t, tracker.Started.Unix(), dr.Started.Unix())
	assert.Nil(t, again.drives[3].Healing())
	for _, b := range []string{"alpha", "beta"} {
		for i := range 6 {
			key := filepathKey(i)
			assert.True(t, again.hasObject(3, b, key), "%s/%s", b, key)
		}
	}
}

func filepathKey(i int) string {
	return "dir" + string(rune('0'+i%3)) + "/obj-00" + string(rune('0'+i))
}

// TestMonitor: the background monitor formats an emptied drive and heals
// it without any request.
func TestMonitor(t *testing.T) {
	e := newEnv(t, "", 4, heal.WithMonitorInterval(20*time.Millisecond))
	objects := e.seed(t, "auto", 4)
	e.start(t)
	e.wipe(t, 0)
	require.Eventually(t, func() bool {
		for k := range objects {
			if !e.hasObject(0, "auto", k) {
				return false
			}
		}
		return e.drives[0].Healing() == nil
	}, 10*time.Second, 20*time.Millisecond)
	_, err := os.Stat(filepath.Join(e.roots[0], domain.SystemBucket, storage.FormatFile))
	assert.NoError(t, err)
}

// waitJob polls a job until it is finished.
func waitJob(t *testing.T, svc heal.Service, id string) heal.Job {
	t.Helper()
	var j heal.Job
	require.Eventually(t, func() bool {
		var err error
		j, err = svc.Job(context.Background(), id)
		require.NoError(t, err)
		return j.State != heal.JobRunning
	}, 10*time.Second, 10*time.Millisecond)
	return j
}

func TestJobs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4, heal.WithJobItems(2))
	e.seed(t, "bkt", 6)
	e.start(t)
	require.NoError(t, os.RemoveAll(filepath.Join(e.roots[1], "bkt", "dir1")))
	require.NoError(t, os.RemoveAll(filepath.Join(e.roots[2], "bkt", "dir2")))

	// Dry run: the report, nothing changed.
	j, err := e.svc.StartJob(ctx, heal.JobRequest{Bucket: "bkt", Recursive: true, Options: heal.Options{DryRun: true}})
	require.NoError(t, err)
	assert.Equal(t, heal.JobRunning, j.State)
	assert.NotEmpty(t, j.ID)
	j = waitJob(t, e.svc, j.ID)
	assert.Equal(t, heal.JobDone, j.State, j.Error)
	assert.Equal(t, uint64(7), j.Scanned, "the bucket and its 6 objects")
	assert.Equal(t, uint64(4), j.Healed)
	assert.Zero(t, j.Failed)
	assert.Len(t, j.Items, 2)
	assert.True(t, j.ItemsTruncated)
	require.Len(t, j.Drives, 4)
	assert.Equal(t, uint64(2), j.Drives[1].Missing)
	assert.Equal(t, uint64(2), j.Drives[2].Missing)
	assert.Zero(t, j.Drives[1].Healed, "dry run")
	assert.False(t, e.hasObject(1, "bkt", "dir1/obj-001"))

	// The real job repairs them.
	j, err = e.svc.StartJob(ctx, heal.JobRequest{Bucket: "bkt", Prefix: "dir", Recursive: true})
	require.NoError(t, err)
	j = waitJob(t, e.svc, j.ID)
	assert.Equal(t, heal.JobDone, j.State)
	assert.Equal(t, uint64(2), j.Drives[1].Healed)
	assert.True(t, e.hasObject(1, "bkt", "dir1/obj-001"))
	assert.True(t, e.hasObject(2, "bkt", "dir2/obj-002"))

	// One object, the whole deployment (format and buckets), a bucket.
	for _, req := range []heal.JobRequest{
		{Bucket: "bkt", Prefix: "dir0/obj-000", Options: heal.Options{Deep: true}},
		{},
		{Bucket: "bkt"},
	} {
		j, err = e.svc.StartJob(ctx, req)
		require.NoError(t, err)
		j = waitJob(t, e.svc, j.ID)
		assert.Equal(t, heal.JobDone, j.State, "%+v: %s", req, j.Error)
		assert.Zero(t, j.Healed)
	}
	assert.Equal(t, uint64(1), j.Scanned, "the bucket")

	// Failures: an unknown bucket fails the job, invalid requests are refused.
	j, err = e.svc.StartJob(ctx, heal.JobRequest{Bucket: "missing"})
	require.NoError(t, err)
	j = waitJob(t, e.svc, j.ID)
	assert.Equal(t, heal.JobFailed, j.State)
	assert.Contains(t, j.Error, "bucket not found")
	for _, req := range []heal.JobRequest{{Prefix: "x"}, {Bucket: "Bad_Name"}} {
		_, err = e.svc.StartJob(ctx, req)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", req)
	}

	jobs, err := e.svc.Jobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 6)
	assert.Equal(t, j.ID, jobs[0].ID, "newest first")
	_, err = e.svc.Job(ctx, "nope")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = e.svc.Job(ctx, "../x")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// Another node (another service on the same drives) sees the jobs
	// through their persisted state.
	other, err := heal.New(heal.Deps{Healer: e.healer, Pools: e.pools, NodeID: "node2", Logger: quiet()}, heal.WithMonitorInterval(0))
	require.NoError(t, err)
	remote, err := other.Job(ctx, jobs[1].ID)
	require.NoError(t, err)
	assert.Equal(t, jobs[1].ID, remote.ID)
	assert.Equal(t, heal.JobDone, remote.State)
	listed, err := other.Jobs(ctx)
	require.NoError(t, err)
	assert.Len(t, listed, 6)
}

func TestJobLimitsAndRetention(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	e.seed(t, "bkt", 2)
	release := make(chan struct{})
	blocking := objectmock.NewHealer(t)
	blocking.EXPECT().HealBucket(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, bucket string, opts object.HealOptions) (object.HealResult, error) {
			select {
			case <-release:
			case <-ctx.Done():
				return object.HealResult{}, ctx.Err()
			}
			return e.healer.HealBucket(ctx, bucket, opts)
		})
	svc, err := heal.New(heal.Deps{Healer: blocking, Pools: e.pools, Logger: quiet()}, heal.WithMaxJobs(1),
		heal.WithJobRetention(100*time.Millisecond), heal.WithMonitorInterval(10*time.Millisecond))
	require.NoError(t, err)
	require.NoError(t, svc.Start(ctx))

	j, err := svc.StartJob(ctx, heal.JobRequest{Bucket: "bkt"})
	require.NoError(t, err)
	same, err := svc.StartJob(ctx, heal.JobRequest{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, j.ID, same.ID, "an identical running job is returned")
	_, err = svc.StartJob(ctx, heal.JobRequest{Bucket: "bkt", Recursive: true})
	assert.ErrorIs(t, err, domain.ErrTooManyRequests)
	close(release)
	assert.Equal(t, heal.JobDone, waitJob(t, svc, j.ID).State)
	// Expired after the retention: forgotten, also on the drives.
	require.Eventually(t, func() bool {
		_, err := svc.Job(ctx, j.ID)
		return errors.Is(err, domain.ErrNotFound)
	}, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		_, err := e.drives[0].ReadAll(ctx, domain.SystemBucket, heal.JobsDir+"/"+j.ID+".json")
		return errors.Is(err, domain.ErrObjectNotFound)
	}, 5*time.Second, 10*time.Millisecond)

	// Stop cancels a running job.
	block := make(chan struct{})
	stuck := objectmock.NewHealer(t)
	stuck.EXPECT().HealBucket(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, _ string, _ object.HealOptions) (object.HealResult, error) {
			<-ctx.Done()
			close(block)
			return object.HealResult{}, ctx.Err()
		})
	svc2, err := heal.New(heal.Deps{Healer: stuck, Pools: e.pools, Logger: quiet()}, heal.WithMonitorInterval(0))
	require.NoError(t, err)
	require.NoError(t, svc2.Start(ctx))
	j, err = svc2.StartJob(ctx, heal.JobRequest{Bucket: "bkt"})
	require.NoError(t, err)
	require.NoError(t, svc2.Stop(ctx))
	<-block
	got, err := svc2.Job(ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, heal.JobCanceled, got.State)
	require.NoError(t, svc.Stop(ctx))
}
