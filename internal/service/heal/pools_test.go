package heal_test

import (
	"context"
	"errors"
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
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

// volDrive is a drive mock listing the given volumes; hang makes the
// listing wait for its context (an unresponsive node).
func volDrive(t *testing.T, online, hang bool, vols ...string) storage.StorageAPI {
	t.Helper()
	d := storagemock.NewStorageAPI(t)
	d.EXPECT().IsOnline().Return(online).Maybe()
	d.EXPECT().ListVols(mock.Anything).RunAndReturn(func(ctx context.Context) ([]storage.VolInfo, error) {
		if hang {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		out := make([]storage.VolInfo, 0, len(vols))
		for _, v := range vols {
			out = append(out, storage.VolInfo{Name: v})
		}
		return out, nil
	}).Maybe()
	return d
}

// TestHealPools: the buckets of the deployment (the volumes of every
// online drive but the system ones) are created on the pools that miss
// them, before the node serves requests; an unresponsive drive does not
// hold the start, the remnants of rolled-back creations are skipped and a
// failed bucket is reported after the others are tried. (The logic lived
// in internal/app; docs/12 puts it in the healing service.)
func TestHealPools(t *testing.T) {
	ctx := context.Background()
	healer := objectmock.NewHealer(t)
	pools := []object.PoolSpec{
		{Sets: [][]storage.StorageAPI{{
			volDrive(t, true, false, "alpha", "beta", ".nexo.sys"),
			volDrive(t, false, false, "never-listed"),
			volDrive(t, true, true),
			nil,
		}}},
		{Sets: [][]storage.StorageAPI{{
			volDrive(t, true, false, "alpha", "ghost", "broken"),
			volDrive(t, true, false),
		}}},
	}
	broken := errors.New("write quorum")
	healer.EXPECT().HealBucket(mock.Anything, "alpha", object.HealOptions{}).Return(object.HealResult{Healed: 2}, nil).Once()
	healer.EXPECT().HealBucket(mock.Anything, "beta", object.HealOptions{}).Return(object.HealResult{}, nil).Once()
	healer.EXPECT().HealBucket(mock.Anything, "ghost", object.HealOptions{}).Return(object.HealResult{}, domain.ErrBucketNotFound).Once()
	healer.EXPECT().HealBucket(mock.Anything, "broken", object.HealOptions{}).Return(object.HealResult{}, broken).Once()
	svc, err := heal.New(heal.Deps{Healer: healer, Pools: pools, Logger: quiet()}, heal.WithListVolsTimeout(50*time.Millisecond))
	require.NoError(t, err)

	start := time.Now()
	n, err := svc.HealPools(ctx)
	assert.ErrorIs(t, err, broken)
	assert.Equal(t, 1, n, "alpha was missing somewhere")
	assert.Less(t, time.Since(start), 5*time.Second, "the unresponsive drive is bounded")

	// A single pool has nothing to spread its buckets to.
	single, err := heal.New(heal.Deps{Healer: objectmock.NewHealer(t), Pools: pools[:1], Logger: quiet()})
	require.NoError(t, err)
	n, err = single.HealPools(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
}
