package storage_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/storage/storagetest"
)

// TestLocalBehavior runs the StorageAPI behavior suite against the local xl
// storage. The RPC client of a remote drive runs the same suite
// (internal/transport/rpc).
func TestLocalBehavior(t *testing.T) {
	storagetest.Run(t, func(t *testing.T, dir string) storage.StorageAPI {
		ep, err := domain.ParseEndpoint(dir)
		require.NoError(t, err)
		d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		return d
	})
}

// TestNewLocalURLEndpoint opens a drive of this node in a distributed
// deployment: a URL endpoint marked local keeps its URL as Endpoint.
func TestNewLocalURLEndpoint(t *testing.T) {
	ep := domain.Endpoint{Scheme: "http", Host: "127.0.0.1:9000", Path: t.TempDir() + "/d1", IsLocal: true, SetIdx: 1, DiskIdx: 2}
	d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	require.Equal(t, ep, d.Endpoint())
	info, err := d.DiskInfo(context.Background(), storage.DiskInfoOptions{})
	require.NoError(t, err)
	require.Equal(t, ep.String(), info.Endpoint)
	require.Equal(t, 2, info.DiskIdx)

	ep.IsLocal = false
	_, err = storage.NewLocal(storage.Deps{}, ep)
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "a remote drive is not opened locally")
}
