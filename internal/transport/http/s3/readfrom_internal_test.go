package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHeaderWriterReadFrom: a body copied through ReadFrom (a file section
// or any reader) writes the header first, with its before hook.
func TestHeaderWriterReadFrom(t *testing.T) {
	name := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(name, []byte("0123456789"), 0o600))
	f, err := os.Open(name) // #nosec G304 -- a test file
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	for _, src := range []io.Reader{&io.LimitedReader{R: f, N: 4}, strings.NewReader("0123")} {
		rec := httptest.NewRecorder()
		called := 0
		w := &headerWriter{ResponseWriter: rec, before: func(int) { called++; rec.Header().Set("X-Test", "1") }}
		n, err := io.Copy(w, src)
		require.NoError(t, err)
		assert.Equal(t, int64(4), n)
		assert.Equal(t, "0123", rec.Body.String())
		assert.Equal(t, 1, called)
		assert.Equal(t, http.StatusOK, w.statusCode())
		assert.Equal(t, "1", rec.Result().Header.Get("X-Test"))
	}
}
