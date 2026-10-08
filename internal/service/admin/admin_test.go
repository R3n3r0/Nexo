package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// fakeObjects is a hand-written object.Service mock for ListBuckets.
type fakeObjects struct {
	object.Service
	buckets []domain.BucketInfo
	err     error
}

func (f *fakeObjects) ListBuckets(context.Context) ([]domain.BucketInfo, error) {
	return f.buckets, f.err
}

func TestServerInfo(t *testing.T) {
	start := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ep, _ := domain.ParseEndpoint("/data1")
	svc := New(Deps{
		Clock:     clk,
		Build:     BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"},
		Mode:      domain.ModeSNSD,
		Region:    "us-east-1",
		Endpoints: []domain.Endpoint{ep},
		Objects: &fakeObjects{buckets: []domain.BucketInfo{
			{Bucket: domain.Bucket{Name: "a"}, Objects: 2, Size: 100},
			{Bucket: domain.Bucket{Name: "b"}, Objects: 3, Size: 50},
		}},
	})
	clk.Advance(90 * time.Second)
	info, err := svc.ServerInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", info.Version)
	assert.Equal(t, domain.ModeSNSD, info.Mode)
	assert.Equal(t, start, info.StartedAt)
	assert.Equal(t, 90*time.Second, info.Uptime)
	require.Len(t, info.Nodes, 1)
	require.Len(t, info.Nodes[0].Disks, 1)
	assert.Equal(t, "/data1", info.Nodes[0].Disks[0].Path)
	assert.Equal(t, uint64(2), info.Buckets)
	assert.Equal(t, uint64(5), info.Objects)
	assert.Equal(t, uint64(150), info.Usage)
}

func TestDataUsageAndNotImplemented(t *testing.T) {
	ctx := context.Background()
	svc := New(Deps{})
	_, err := svc.DataUsage(ctx)
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = svc.Heal(ctx, HealInput{})
	assert.ErrorIs(t, err, domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, ActionRestart), domain.ErrNotImplemented)
	assert.ErrorIs(t, svc.ServiceAction(ctx, "dance"), domain.ErrInvalidArgument)

	withObjects := New(Deps{Objects: &fakeObjects{buckets: []domain.BucketInfo{{Bucket: domain.Bucket{Name: "a"}, Objects: 1, Size: 10, Versions: 2}}}})
	du, err := withObjects.DataUsage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), du.BucketsCount)
	assert.Equal(t, uint64(10), du.TotalSize)
	assert.Equal(t, uint64(2), du.Buckets["a"].VersionsCount)

	failing := New(Deps{Objects: &fakeObjects{err: errors.New("down")}})
	_, err = failing.DataUsage(ctx)
	assert.Error(t, err)

	var got Action
	withActions := New(Deps{Actions: ActionHandlerFunc(func(_ context.Context, a Action) error { got = a; return nil })})
	require.NoError(t, withActions.ServiceAction(ctx, ActionStop))
	assert.Equal(t, ActionStop, got)
}
