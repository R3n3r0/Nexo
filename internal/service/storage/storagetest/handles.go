package storagetest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// testOpenFile: a handle reads its file at any offset, in any order and
// with several streams at once, after the file was removed (the shards a
// read pinned), and fails cleanly once closed.
func testOpenFile(t *testing.T, f Factory) {
	if runtime.GOOS == "windows" {
		t.Skip("an open file cannot be removed on Windows")
	}
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/h/part.1", 10, strings.NewReader("0123456789")))
	h, err := s.OpenFile(ctx, domain.SystemBucket, "tmp/h/part.1")
	require.NoError(t, err)
	assert.Equal(t, int64(10), h.Size())

	readAll := func(off, length int64) string {
		t.Helper()
		rc, err := h.ReadStream(ctx, off, length)
		require.NoError(t, err)
		defer func() { _ = rc.Close() }()
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		return string(data)
	}
	assert.Equal(t, "789", readAll(7, -1))
	assert.Equal(t, "234", readAll(2, 3))
	// Removed from the drive: the handle still reads it.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, domain.SystemBucket, "tmp", "h")))
	_, err = s.ReadFileStream(ctx, domain.SystemBucket, "tmp/h/part.1", 0, -1)
	require.ErrorIs(t, err, domain.ErrObjectNotFound)
	first, err := h.ReadStream(ctx, 0, 5)
	require.NoError(t, err)
	assert.Equal(t, "56789", readAll(5, -1))
	_, err = h.ReadStream(ctx, 11, 1)
	require.ErrorIs(t, err, domain.ErrInvalidRange)
	_, err = h.ReadStream(ctx, 8, 5)
	require.ErrorIs(t, err, domain.ErrFaultyDisk)

	// Closing the handle keeps the open streams readable, refuses new ones.
	require.NoError(t, h.Close(ctx))
	require.NoError(t, h.Close(ctx), "closing twice")
	data, err := io.ReadAll(first)
	require.NoError(t, err)
	assert.Equal(t, "01234", string(data))
	require.NoError(t, first.Close())
	_, err = h.ReadStream(ctx, 0, 1)
	require.Error(t, err)

	_, err = s.OpenFile(ctx, domain.SystemBucket, "tmp/h/missing")
	require.ErrorIs(t, err, domain.ErrObjectNotFound)
}
