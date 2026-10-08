package config_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func newDocs(t *testing.T, n int) sysstore.Store {
	t.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		drives[i] = testutil.NewLocalDriveIndex(t, "", i)
	}
	docs, err := sysstore.New(sysstore.Deps{Drives: drives})
	require.NoError(t, err)
	return docs
}

// TestDocsStoreSharedByNodes: two nodes over the same documents; Reload
// makes the changes of the other node visible and notifies the watchers.
func TestDocsStoreSharedByNodes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	docs := newDocs(t, 4)
	storeA, err := config.NewDocsStore(docs)
	require.NoError(t, err)
	storeB, err := config.NewDocsStore(docs)
	require.NoError(t, err)
	a := config.New(config.Deps{Store: storeA})
	b := config.New(config.Deps{Store: storeB})

	empty, err := b.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty)

	require.NoError(t, a.Set(ctx, "api", config.KV{"root_access": "off"}))
	require.NoError(t, a.Set(ctx, "scanner", config.KV{"speed": "slow"}))
	kv, err := b.Get(ctx, "api")
	require.NoError(t, err)
	assert.Empty(t, kv, "cached until reloaded")

	changes := b.Watch(ctx)
	require.NoError(t, b.Reload(ctx))
	kv, err = b.Get(ctx, "api")
	require.NoError(t, err)
	assert.Equal(t, config.KV{"root_access": "off"}, kv)
	got := map[string]bool{}
	for range 2 {
		select {
		case c := <-changes:
			got[c.Subsystem] = c.Deleted
		case <-time.After(time.Second):
			t.Fatal("no change notified")
		}
	}
	assert.Equal(t, map[string]bool{"api": false, "scanner": false}, got)

	require.NoError(t, a.Delete(ctx, "scanner"))
	require.NoError(t, b.Reload(ctx))
	select {
	case c := <-changes:
		assert.Equal(t, config.Change{Subsystem: "scanner", Deleted: true}, c)
	case <-time.After(time.Second):
		t.Fatal("no deletion notified")
	}
	require.NoError(t, b.Reload(ctx))
	select {
	case c := <-changes:
		t.Fatalf("unexpected change %+v: nothing changed", c)
	default:
	}

	_, err = config.NewDocsStore(nil)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
