package middleware

import (
	"bytes"
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

// TestResponseWriterReadFrom: a file section goes on to the wrapped
// writer through ReadFrom (zero-copy, httpx.FileSection), any other reader
// through Write; both are counted and imply a 200.
func TestResponseWriterReadFrom(t *testing.T) {
	name := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(name, []byte("0123456789"), 0o600))
	f, err := os.Open(name) // #nosec G304 -- a test file
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })

	rec := httptest.NewRecorder()
	inner := &readFromRecorder{ResponseWriter: rec}
	w := wrapWriter(inner)
	n, err := io.Copy(w, &io.LimitedReader{R: f, N: 4})
	require.NoError(t, err)
	assert.Equal(t, int64(4), n)
	n, err = io.Copy(w, strings.NewReader("abc"))
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.Equal(t, "0123abc", rec.Body.String())
	assert.Equal(t, int64(7), w.bytes)
	assert.Equal(t, http.StatusOK, w.Status())
	assert.Equal(t, 1, inner.sections, "only the file section reaches ReadFrom")
}

type readFromRecorder struct {
	http.ResponseWriter
	sections int
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.sections++
	var b bytes.Buffer
	if _, err := b.ReadFrom(src); err != nil {
		return 0, err
	}
	n, err := r.Write(b.Bytes())
	return int64(n), err
}
