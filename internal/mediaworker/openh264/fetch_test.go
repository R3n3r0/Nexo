package openh264

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fake is the build of testdata/fake.so.bz2.
var fake = build{name: "fake.so", sha256: "56a4c769086bfba18ebd6ba0b8653aa7332a2acf0e32f63e457ac76c35e294e4"}

// fakeServer serves testdata and counts the downloads.
func fakeServer(t *testing.T) (base string, downloads *atomic.Int32) {
	t.Helper()
	downloads = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		http.ServeFile(w, r, filepath.Join("testdata", filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/", downloads
}

func TestFetchDownloadsVerifiesAndCaches(t *testing.T) {
	base, downloads := fakeServer(t)
	dir := t.TempDir()
	path, err := fetch(context.Background(), http.DefaultClient, base, dir, fake)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "fake.so"), path)
	data, err := os.ReadFile(path) // #nosec G304 -- test file
	require.NoError(t, err)
	assert.Equal(t, "not really a library\n", string(data))

	_, err = fetch(context.Background(), http.DefaultClient, base, dir, fake)
	require.NoError(t, err)
	assert.Equal(t, int32(1), downloads.Load(), "a verified file is reused")

	require.NoError(t, os.WriteFile(path, []byte("tampered"), 0o600))
	_, err = fetch(context.Background(), http.DefaultClient, base, dir, fake)
	require.NoError(t, err)
	assert.Equal(t, int32(2), downloads.Load(), "a damaged file is downloaded again")
}

func TestFetchRejectsWrongChecksum(t *testing.T) {
	base, _ := fakeServer(t)
	b := fake
	b.sha256 = "00" + b.sha256[2:]
	dir := t.TempDir()
	_, err := fetch(context.Background(), http.DefaultClient, base, dir, b)
	require.ErrorIs(t, err, ErrChecksum)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is kept")
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := fetch(context.Background(), http.DefaultClient, srv.URL+"/", t.TempDir(), fake)
	require.ErrorContains(t, err, "404")
}

func TestPlatformsArePinned(t *testing.T) {
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"} {
		b, ok := binaries()[p]
		require.True(t, ok, p)
		assert.Len(t, b.sha256, 64, p)
		assert.Contains(t, b.name, Version, p)
	}
}
