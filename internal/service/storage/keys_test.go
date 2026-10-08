package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func TestKeyNames(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := testutil.NewLocalDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()

	valid := []string{
		"a b/c d.txt",
		"ünïcødé/文件 名.txt",
		"emoji/😀.png",
		"100%/x%2Fy",
		"%2e%2e/escaped",
		"a+b=c&d",
		"..a",
		"a..",
		"...",
		"dots/.../x",
		"#hash?query",
		"tab\tkey",
		"trailing space ",
		"folder/",
		"folder/inside",
		"back\\slash",
		strings.Repeat("x", 255),
	}
	for _, key := range valid {
		t.Run("valid "+key, func(t *testing.T) {
			payload := []byte("content of " + key)
			_, err := putObject(ctx, s, "b", key, "", now, payload)
			require.NoError(t, err)
			got, err := getObject(ctx, s, "b", key, "")
			require.NoError(t, err)
			assert.Equal(t, payload, got)
		})
	}
	sorted := append([]string(nil), valid...)
	sort.Strings(sorted)
	assert.Equal(t, sorted, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}),
		"listings return the keys as written, in byte order")

	invalid := []string{
		"", "..", ".", "../x", "a/../b", "a/./b", "a//b", "//", "/abs", "x\x00y",
		"k__XLDIR__", "a/b__XLDIR__/c",
	}
	for _, key := range invalid {
		t.Run("invalid "+key, func(t *testing.T) {
			err := s.WriteMetadata(ctx, "b", key, storage.FileInfo{ModTime: now, Data: []byte("x")})
			assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
			_, err = s.ReadVersion(ctx, "b", key, "", storage.ReadOptions{})
			assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
		})
	}
	err := s.WriteMetadata(ctx, "b", strings.Repeat("y", 256), storage.FileInfo{ModTime: now, Data: []byte("x")})
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName, "a segment longer than the file system allows")

	// Nothing escaped the volume.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{domain.SystemBucket, "b"}, names)
}

func TestObjectSizes(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	sizes := map[string]int{
		"0 B": 0, "1 B": 1, "127 KiB": 127 << 10, "128 KiB - 1": storage.SmallFileThreshold - 1,
		"128 KiB": storage.SmallFileThreshold, "129 KiB": 129 << 10, "10 MiB": 10 << 20,
	}
	for name, size := range sizes {
		t.Run(name, func(t *testing.T) {
			payload := make([]byte, size)
			_, _ = rand.Read(payload)
			key := "sizes/" + strings.ReplaceAll(name, " ", "_")
			_, err := putObject(ctx, s, "b", key, "", time.Now(), payload)
			require.NoError(t, err)
			fi, err := s.ReadVersion(ctx, "b", key, "", storage.ReadOptions{})
			require.NoError(t, err)
			assert.Equal(t, int64(size), fi.Size)
			assert.Equal(t, size < storage.SmallFileThreshold, fi.DataDir == "", "objects below 128 KiB are inline")
			got, err := getObject(ctx, s, "b", key, "")
			require.NoError(t, err)
			assert.Equal(t, sha256.Sum256(payload), sha256.Sum256(got))
			if fi.DataDir != "" {
				check, err := s.CheckParts(ctx, "b", key, fi)
				require.NoError(t, err)
				assert.Equal(t, []int{storage.CheckPartSuccess}, check.Results)
			}
		})
	}

	// Empty data files go through CreateFile as well.
	require.NoError(t, s.CreateFile(ctx, domain.SystemBucket, "tmp/empty/part.1", 0, bytes.NewReader(nil)))
	rc, err := s.ReadFileStream(ctx, domain.SystemBucket, "tmp/empty/part.1", 0, -1)
	require.NoError(t, err)
	n, err := rc.Read(make([]byte, 1))
	assert.Zero(t, n)
	assert.Error(t, err)
	require.NoError(t, rc.Close())
}
