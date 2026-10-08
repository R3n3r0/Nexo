package testutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func TestNewLocalDrive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "explicit")
	s := testutil.NewLocalDrive(t, dir)
	assert.Equal(t, dir, s.Endpoint().Path)
	_, err := os.Stat(filepath.Join(dir, domain.SystemBucket, storage.ConfigDir))
	assert.NoError(t, err)
	assert.True(t, testutil.NewLocalDrive(t, "").IsOnline())
	assert.Equal(t, 3, testutil.NewLocalDriveIndex(t, "", 3).Endpoint().DiskIdx)
}
