package admin_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/heal"
	healmock "github.com/r3n3r0/nexo/internal/service/heal/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	scannermock "github.com/r3n3r0/nexo/internal/service/scanner/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

func TestServerInfo(t *testing.T) {
	start := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ep, _ := domain.ParseEndpoint("/data1")
	ep.SetIdx, ep.DiskIdx = 0, 1

	online := storagemock.NewStorageAPI(t)
	online.EXPECT().Endpoint().Return(ep)
	online.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).
		Return(domain.DiskInfo{Endpoint: "/data1", Path: "/data1", ID: "disk-1", State: domain.StateOnline, Total: 100, Free: 60, Used: 40}, nil)
	online.EXPECT().Healing().Return(nil)

	broken := storagemock.NewStorageAPI(t)
	brokenEP, _ := domain.ParseEndpoint("/data2")
	broken.EXPECT().Endpoint().Return(brokenEP)
	broken.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).Return(domain.DiskInfo{}, errors.New("io error"))
	broken.EXPECT().Healing().Return(nil)

	objects := objectmock.NewService(t)
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "a"}, Objects: 2, Size: 100},
		{Bucket: domain.Bucket{Name: "b"}, Objects: 3, Size: 50},
	}, nil)

	svc := admin.New(admin.Deps{
		Clock:        clk,
		Build:        admin.BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"},
		Mode:         domain.ModeSNSD,
		Region:       "us-east-1",
		DeploymentID: "dep-1",
		Drives:       []storage.StorageAPI{online, broken},
		Pools:        []domain.PoolInfo{{Index: 0, Sets: 1, DrivesPerSet: 2}},
		Objects:      objects,
	})
	clk.Advance(90 * time.Second)
	info, err := svc.ServerInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", info.Version)
	assert.Equal(t, domain.ModeSNSD, info.Mode)
	assert.Equal(t, "dep-1", info.DeploymentID)
	assert.Equal(t, start, info.StartedAt)
	assert.Equal(t, 90*time.Second, info.Uptime)
	require.Len(t, info.Pools, 1)
	require.Len(t, info.Nodes, 1)
	require.Len(t, info.Nodes[0].Disks, 2)
	d1, d2 := info.Nodes[0].Disks[0], info.Nodes[0].Disks[1]
	assert.Equal(t, "/data1", d1.Path)
	assert.Equal(t, "disk-1", d1.ID)
	assert.Equal(t, domain.StateOnline, d1.State)
	assert.Equal(t, uint64(40), d1.Used)
	assert.Equal(t, 1, d1.DiskIdx)
	assert.Equal(t, "/data2", d2.Path, "endpoint fills the missing fields")
	assert.Equal(t, domain.StateOffline, d2.State)
	assert.Equal(t, "io error", d2.Error)
	assert.Equal(t, uint64(2), info.Buckets)
	assert.Equal(t, uint64(5), info.Objects)
	assert.Equal(t, uint64(150), info.Usage)
}

func TestDataUsageAndNotImplemented(t *testing.T) {
	ctx := context.Background()
	svc := admin.New(admin.Deps{})
	_, err := svc.DataUsage(ctx)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.StartHeal(ctx, admin.HealInput{})
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.HealStatus(ctx, "h1")
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.ListHeals(ctx)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, admin.ActionRestart), domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, "dance"), domain.ErrInvalidArgument)

	objects := objectmock.NewService(t)
	objects.EXPECT().DataUsage(mock.Anything, "").
		Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "a"}, Objects: 1, Size: 10, Versions: 2}}, nil).Once()
	withObjects := admin.New(admin.Deps{Objects: objects})
	du, err := withObjects.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), du.BucketsCount)
	assert.Equal(t, uint64(10), du.TotalSize)
	assert.Equal(t, uint64(2), du.Buckets["a"].VersionsCount)

	objects.EXPECT().DataUsage(mock.Anything, "").Return(nil, errors.New("down")).Once()
	_, err = withObjects.DataUsage(ctx)
	assert.Error(t, err)

	var got admin.Action
	withActions := admin.New(admin.Deps{Actions: admin.ActionHandlerFunc(func(_ context.Context, a admin.Action) error { got = a; return nil })})
	require.NoError(t, withActions.ServiceAction(ctx, admin.ActionStop))
	assert.Equal(t, admin.ActionStop, got)
}

func TestHealJobs(t *testing.T) {
	ctx := context.Background()
	healer := healmock.NewService(t)
	svc := admin.New(admin.Deps{Heal: healer})
	job := heal.Job{ID: "h1", State: heal.JobRunning}
	healer.EXPECT().StartJob(mock.Anything, heal.JobRequest{Bucket: "b", Prefix: "p/", Recursive: true,
		Options: heal.Options{DryRun: true, Deep: true, Remove: true}}).Return(job, nil).Once()
	got, err := svc.StartHeal(ctx, admin.HealInput{Bucket: "b", Prefix: "p/", Recursive: true, DryRun: true, Deep: true, Remove: true})
	require.NoError(t, err)
	assert.Equal(t, job, got)

	healer.EXPECT().StartJob(mock.Anything, heal.JobRequest{}).Return(heal.Job{}, domain.ErrTooManyRequests).Once()
	_, err = svc.StartHeal(ctx, admin.HealInput{})
	assert.ErrorIs(t, err, domain.ErrTooManyRequests)

	healer.EXPECT().Job(mock.Anything, "h1").Return(job, nil).Once()
	got, err = svc.HealStatus(ctx, "h1")
	require.NoError(t, err)
	assert.Equal(t, "h1", got.ID)
	healer.EXPECT().Job(mock.Anything, "zz").Return(heal.Job{}, domain.ErrNotFound).Once()
	_, err = svc.HealStatus(ctx, "zz")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	healer.EXPECT().Jobs(mock.Anything).Return([]heal.Job{job}, nil).Once()
	jobs, err := svc.ListHeals(ctx)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
	healer.EXPECT().Jobs(mock.Anything).Return(nil, errors.New("down")).Once()
	_, err = svc.ListHeals(ctx)
	assert.Error(t, err)
}

// TestDataUsageFromScanner: the usage of the scanner is served for the
// buckets it met, the newer buckets are computed on demand, the removed
// ones dropped; without a usage yet everything is computed.
func TestDataUsageFromScanner(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC)
	scan := scannermock.NewService(t)
	objects := objectmock.NewService(t)
	svc := admin.New(admin.Deps{Objects: objects, Scanner: scan})

	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{LastUpdate: at, Cycle: 3, Buckets: map[string]scanner.BucketUsage{
		"old":  {Counts: scanner.Counts{Objects: 4, Versions: 1, DeleteMarkers: 2, Size: 400}, Prefixes: map[string]scanner.Counts{"a/": {Objects: 4, Size: 400}}},
		"gone": {Counts: scanner.Counts{Objects: 9, Size: 900}},
	}}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "new"}}, {Bucket: domain.Bucket{Name: "old"}}}, nil).Once()
	objects.EXPECT().DataUsage(mock.Anything, "new").Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "new"}, Objects: 1, Size: 10}}, nil).Once()
	du, err := svc.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, at, du.LastUpdate)
	assert.Equal(t, uint64(2), du.BucketsCount)
	assert.Equal(t, uint64(5), du.ObjectsCount)
	assert.Equal(t, uint64(1), du.VersionsCount)
	assert.Equal(t, uint64(410), du.TotalSize)
	assert.Equal(t, admin.BucketUsage{Size: 400, ObjectsCount: 4, VersionsCount: 1, DeleteMarkers: 2,
		Prefixes: map[string]admin.PrefixUsage{"a/": {Size: 400, ObjectsCount: 4}}}, du.Buckets["old"])
	assert.NotContains(t, du.Buckets, "gone")

	// No cycle yet: the walk of every bucket.
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, domain.ErrNotFound).Once()
	objects.EXPECT().DataUsage(mock.Anything, "").Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "x"}, Objects: 2}}, nil).Once()
	du, err = svc.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), du.ObjectsCount)

	// Errors.
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return(nil, errors.New("down")).Once()
	_, err = svc.DataUsage(ctx)
	assert.Error(t, err)
	scan.EXPECT().Usage(mock.Anything).Return(scanner.Usage{}, nil).Once()
	objects.EXPECT().ListBuckets(mock.Anything).Return([]domain.BucketInfo{{Bucket: domain.Bucket{Name: "racy"}}, {Bucket: domain.Bucket{Name: "bad"}}}, nil).Once()
	objects.EXPECT().DataUsage(mock.Anything, "racy").Return(nil, domain.ErrBucketNotFound).Once()
	objects.EXPECT().DataUsage(mock.Anything, "bad").Return(nil, errors.New("io")).Once()
	_, err = svc.DataUsage(ctx)
	assert.Error(t, err)
}

func TestServerInfoHealingProgress(t *testing.T) {
	ep, _ := domain.ParseEndpoint("/data3")
	started := time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)
	d := storagemock.NewStorageAPI(t)
	d.EXPECT().Endpoint().Return(ep)
	d.EXPECT().DiskInfo(mock.Anything, storage.DiskInfoOptions{}).Return(domain.DiskInfo{Endpoint: "/data3", State: domain.StateOnline}, nil)
	d.EXPECT().IsOnline().Return(true).Maybe()
	d.EXPECT().Healing().Return(&storage.HealingTracker{Started: started, Bucket: "b", Object: "k", ObjectsHealed: 7, HealedBuckets: []string{"a"}})
	info, err := admin.New(admin.Deps{Drives: []storage.StorageAPI{d}}).ServerInfo(context.Background())
	require.NoError(t, err)
	disk := info.Nodes[0].Disks[0]
	assert.True(t, disk.Healing)
	assert.Equal(t, domain.StateHealing, disk.State)
	require.NotNil(t, disk.HealingProgress)
	assert.Equal(t, domain.HealingProgress{Started: started, Bucket: "b", Object: "k", ObjectsHealed: 7, HealedBuckets: 1}, *disk.HealingProgress)
}
