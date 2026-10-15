//go:build upgrade

package upgrade_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// formats are the on-disk formats of a drive that decide whether a
// release reads it: the version of format.json and the major version of
// the xl.meta documents (a release reads the minor versions it does not
// know, never another major one).
type formats struct {
	deployment    string
	formatVersion int
	xlMetaMajor   uint16
}

// driveFormats reads the formats of drive, the xl.meta being the one of
// bucket/key.
func driveFormats(t *testing.T, drive, bucket, key string) formats {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(drive, domain.SystemBucket, storage.FormatFile))
	require.NoError(t, err)
	var doc struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(data, &doc))
	meta, err := os.ReadFile(filepath.Join(drive, bucket, key, storage.MetaFile))
	require.NoError(t, err, "xl.meta of %s/%s", bucket, key)
	require.GreaterOrEqual(t, len(meta), 8)
	require.True(t, bytes.HasPrefix(meta, []byte("NXLM")), "xl.meta magic of %s/%s", bucket, key)
	return formats{deployment: doc.ID, formatVersion: doc.Version, xlMetaMajor: binary.BigEndian.Uint16(meta[4:6])}
}

// serverVersion returns the version the node announces.
func serverVersion(t *testing.T, n *node) string {
	t.Helper()
	info, err := n.admin().ServerInfo(context.Background())
	require.NoError(t, err)
	return info.Version
}
