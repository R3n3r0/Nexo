package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// TestDeleteVolRace covers the removal of a volume racing with a write:
// the volume is moved aside atomically, so a write committed between the
// emptiness check and the removal is never destroyed by a removal without
// force (the volume is put back, ErrBucketNotEmpty), and the moved tree of
// a forced removal does not stay in tmp.
func TestDeleteVolRace(t *testing.T) {
	tests := []struct {
		name  string
		force bool
		// wantErr is the error of DeleteVol, nil for a removal.
		wantErr error
	}{
		{name: "without force the late write keeps the volume", wantErr: domain.ErrBucketNotEmpty},
		{name: "forced removal", force: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "drive")
			s := openDrive(t, root)
			require.NoError(t, s.MakeVol(ctx, "b"))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "b", "leftover"), 0o755))

			// A write lands right before the volume is moved aside.
			late := filepath.Join(root, "b", "late", MetaFile)
			s.rename = func(oldpath, newpath string) error {
				if oldpath == filepath.Join(root, "b") {
					require.NoError(t, os.MkdirAll(filepath.Dir(late), 0o755))
					require.NoError(t, os.WriteFile(late, []byte("committed"), 0o600))
				}
				return os.Rename(oldpath, newpath)
			}
			t.Cleanup(func() { s.rename = os.Rename })

			err := s.DeleteVol(ctx, "b", tt.force)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				data, rerr := os.ReadFile(late)
				require.NoError(t, rerr, "the late write is back in the volume")
				assert.Equal(t, "committed", string(data))
				_, serr := s.StatVol(ctx, "b")
				assert.NoError(t, serr)
			} else {
				require.NoError(t, err)
				_, serr := s.StatVol(ctx, "b")
				assert.ErrorIs(t, serr, domain.ErrBucketNotFound)
			}
			assert.Empty(t, tmpEntries(t, root), "nothing of the volume is left in tmp")
		})
	}
}

// TestDeleteVolWritesFailAfterRemoval checks that a write reaching a
// volume removed under it fails instead of recreating the volume.
func TestDeleteVolWritesFailAfterRemoval(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "drive")
	s := openDrive(t, root)
	require.NoError(t, s.MakeVol(ctx, "b"))
	fi := stageData(t, s, "up", "data")
	require.NoError(t, s.DeleteVol(ctx, "b", true))

	_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up", fi, "b", "k", RenameOptions{})
	require.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = os.Stat(filepath.Join(root, "b"))
	assert.True(t, os.IsNotExist(err), "the volume is not recreated")
}
