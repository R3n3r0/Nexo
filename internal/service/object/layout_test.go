package object_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// The suites run with the metadata in xl.meta files or in the metadata
// log of the drives (NEXO_TEST_METADATA_LOG=1): the helpers below look at
// the drives in a way that holds for both.

// hasMeta reports whether drive i holds a version of bucket/key.
func (c *cluster) hasMeta(i int, bucket, key string) bool {
	_, err := c.drives[i].ReadVersion(context.Background(), bucket, key, "", storage.ReadOptions{})
	return err == nil
}

// dataEntries lists what the directory of an object holds besides its
// xl.meta file: the data directories (a missing directory holds none).
func dataEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if e.Name() != storage.MetaFile {
			out = append(out, e.Name())
		}
	}
	return out
}

// corruptStored flips a bit of needle wherever the drive at root stores it:
// in the xl.meta file of an object or in a segment of the metadata log.
func corruptStored(t *testing.T, root string, needle []byte) {
	t.Helper()
	found := false
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- a file of the test drive
		if err != nil {
			return err
		}
		changed := false
		for at := bytes.Index(raw, needle); at >= 0; {
			raw[at] ^= 0x01
			changed = true
			next := bytes.Index(raw[at+1:], needle)
			if next < 0 {
				break
			}
			at += 1 + next
		}
		if changed {
			found = true
			return os.WriteFile(path, raw, 0o600)
		}
		return nil
	}))
	require.True(t, found, "the content is stored on the drive")
}

// skipMetadataLog skips a test that plants or damages xl.meta files.
func skipMetadataLog(t *testing.T) {
	t.Helper()
	if testutil.MetadataLogTests() {
		t.Skip("damages xl.meta files; the records of the metadata log are covered by internal/service/storage")
	}
}
