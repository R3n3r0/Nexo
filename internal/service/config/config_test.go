package config_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
)

func TestServiceGetSetDelete(t *testing.T) {
	ctx := context.Background()
	store := config.NewMemoryStore(map[string]config.KV{"scanner": {"speed": "fast"}})
	svc := config.New(config.Deps{Store: store})

	kv, err := svc.Get(ctx, "scanner")
	require.NoError(t, err)
	assert.Equal(t, config.KV{"speed": "fast"}, kv)

	missing, err := svc.Get(ctx, "nope")
	require.NoError(t, err)
	assert.Equal(t, config.KV{}, missing)

	require.NoError(t, svc.Set(ctx, "scanner", config.KV{"delay": "10"}))
	kv, err = svc.Get(ctx, "scanner")
	require.NoError(t, err)
	assert.Equal(t, config.KV{"speed": "fast", "delay": "10"}, kv)

	// Mutating the returned map does not affect the service.
	kv["speed"] = "slow"
	again, _ := svc.Get(ctx, "scanner")
	assert.Equal(t, "fast", again["speed"])

	// Persisted to the store.
	persisted, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, "10", persisted["scanner"]["delay"])

	require.NoError(t, svc.Delete(ctx, "scanner", "delay"))
	kv, _ = svc.Get(ctx, "scanner")
	assert.Equal(t, config.KV{"speed": "fast"}, kv)
	require.NoError(t, svc.Delete(ctx, "scanner"))
	kv, _ = svc.Get(ctx, "scanner")
	assert.Empty(t, kv)
	require.NoError(t, svc.Delete(ctx, "scanner")) // idempotent

	assert.ErrorIs(t, svc.Set(ctx, "", config.KV{"a": "b"}), domain.ErrInvalidArgument)
}

func TestServiceListExportImport(t *testing.T) {
	ctx := context.Background()
	svc := config.New(config.Deps{})
	require.NoError(t, svc.Set(ctx, "b", config.KV{"k": "v"}))
	require.NoError(t, svc.Set(ctx, "a", config.KV{"x": "1"}))
	names, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, names)

	exported, err := svc.Export(ctx)
	require.NoError(t, err)

	other := config.New(config.Deps{})
	require.NoError(t, other.Import(ctx, exported))
	kv, err := other.Get(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, config.KV{"k": "v"}, kv)

	assert.ErrorIs(t, other.Import(ctx, []byte("{bad")), domain.ErrInvalidArgument)
}

func TestServiceWatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := config.New(config.Deps{})
	ch := svc.Watch(ctx)

	require.NoError(t, svc.Set(ctx, "notify", config.KV{"endpoint": "http://x", "enable": "on"}))
	select {
	case c := <-ch:
		assert.Equal(t, config.Change{Subsystem: "notify", Keys: []string{"enable", "endpoint"}}, c)
	case <-time.After(time.Second):
		t.Fatal("no change received")
	}
	require.NoError(t, svc.Delete(ctx, "notify"))
	select {
	case c := <-ch:
		assert.True(t, c.Deleted)
	case <-time.After(time.Second):
		t.Fatal("no delete received")
	}
	cancel()
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel should be closed")
	case <-time.After(time.Second):
		t.Fatal("channel not closed")
	}
}

func TestServiceStoreFailure(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk on fire")
	store := configmock.NewStore(t)
	store.EXPECT().Load(ctx).Return(map[string]config.KV{}, nil).Maybe()
	store.EXPECT().Save(ctx, mock.Anything).Return(boom).Once()
	svc := config.New(config.Deps{Store: store})
	err := svc.Set(ctx, "s", config.KV{"a": "b"})
	assert.ErrorIs(t, err, boom)
	kv, _ := svc.Get(ctx, "s")
	assert.Empty(t, kv, "failed writes must not be cached")
}
