package heal_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// corruptMeta overwrites the xl.meta of bucket/key on drive i and returns
// its path.
func (e *env) corruptMeta(t *testing.T, i int, bucket, key string) string {
	t.Helper()
	name := filepath.Join(e.roots[i], bucket, filepath.FromSlash(key), storage.MetaFile)
	require.NoError(t, os.WriteFile(name, []byte("garbage, not an xl.meta"), 0o600))
	return name
}

// metaHealed reports whether the xl.meta at name is decodable again.
func (e *env) metaHealed(t *testing.T, i int, bucket, key string) bool {
	t.Helper()
	_, err := e.drives[i].ReadVersion(context.Background(), bucket, key, "", storage.ReadOptions{})
	return err == nil
}

// TestHealCorruptMetadata (TS-U-06, metadata bitrot): an undecodable
// xl.meta is repaired by every healing path: the MRF worker after a
// degraded read, HealObject and the healing of a whole drive.
func TestHealCorruptMetadata(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "", 4)
	objects := e.seed(t, "bkt", 6)
	keys := []string{"dir0/obj-000", "dir1/obj-001"} // inline, erasure coded

	t.Run("HealObject", func(t *testing.T) {
		for _, key := range keys {
			e.corruptMeta(t, 1, "bkt", key)
			res, err := e.svc.HealObject(ctx, "bkt", key, "", heal.Options{})
			require.NoError(t, err)
			before, after := states(res.Drives)
			assert.Equal(t, []object.HealDriveState{ok, corrupt, ok, ok}, before)
			assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, after)
			assert.True(t, e.metaHealed(t, 1, "bkt", key))
		}
	})

	t.Run("HealDisk", func(t *testing.T) {
		for _, key := range keys {
			e.corruptMeta(t, 2, "bkt", key)
		}
		res, err := e.svc.HealDisk(ctx, e.drives[2].Endpoint().String())
		require.NoError(t, err)
		assert.Zero(t, res.Failed)
		for _, key := range keys {
			assert.True(t, e.metaHealed(t, 2, "bkt", key), key)
		}
	})

	t.Run("MRF", func(t *testing.T) {
		e.start(t)
		for _, key := range keys {
			e.corruptMeta(t, 3, "bkt", key)
			assert.Equal(t, objects[key], e.get(t, "bkt", key), "a degraded read serves the object and queues it")
		}
		require.Eventually(t, func() bool {
			return e.metaHealed(t, 3, "bkt", keys[0]) && e.metaHealed(t, 3, "bkt", keys[1])
		}, 10*time.Second, 20*time.Millisecond, "the MRF worker repairs the metadata")
	})

	// Every drive is whole again: any two of them serve every object.
	defer e.takeOffline(t, 0)()
	defer e.takeOffline(t, 2)()
	for key, want := range objects {
		assert.Equal(t, want, e.get(t, "bkt", key), key)
	}
}
