package httpx

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempFile returns an open file of n random bytes and its content.
func tempFile(t *testing.T, n int) (*os.File, []byte) {
	t.Helper()
	data := make([]byte, n)
	_, err := rand.Read(data)
	require.NoError(t, err)
	name := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(name, data, 0o600))
	f, err := os.Open(name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f, data
}

func TestFileSection(t *testing.T) {
	f, _ := tempFile(t, 10)
	lr, ok := FileSection(&io.LimitedReader{R: f, N: 5})
	assert.True(t, ok)
	assert.Equal(t, int64(5), lr.N)
	for _, r := range []io.Reader{f, &io.LimitedReader{R: strings.NewReader("x"), N: 1}, strings.NewReader("x")} {
		_, ok := FileSection(r)
		assert.False(t, ok, "%T", r)
	}
}

// pieceWriter records the pieces handed to its ReadFrom; fail, when set,
// is the outcome of each piece (bytes taken, error).
type pieceWriter struct {
	bytes.Buffer
	pieces []int64
	fail   func(i int, piece *io.LimitedReader) (int64, error)
}

func (w *pieceWriter) ReadFrom(r io.Reader) (int64, error) {
	lr := r.(*io.LimitedReader)
	w.pieces = append(w.pieces, lr.N)
	if w.fail != nil {
		return w.fail(len(w.pieces)-1, lr)
	}
	return w.Buffer.ReadFrom(lr)
}

func TestCopyFileSection(t *testing.T) {
	f, data := tempFile(t, 3*filePiece+100)
	_, err := f.Seek(50, io.SeekStart)
	require.NoError(t, err)
	w := &pieceWriter{}
	arms := 0
	lr := &io.LimitedReader{R: f, N: int64(len(data)) - 60}
	n, err := CopyFileSection(w, lr, func() { arms++ })
	require.NoError(t, err)
	assert.Equal(t, int64(len(data))-60, n)
	assert.Zero(t, lr.N)
	assert.Equal(t, data[50:len(data)-10], w.Bytes())
	assert.Equal(t, []int64{filePiece, filePiece, filePiece, 40}, w.pieces)
	assert.Equal(t, 4, arms, "one deadline per piece")
}

func TestCopyFileSectionShortFile(t *testing.T) {
	f, data := tempFile(t, 1000)
	w := &pieceWriter{}
	n, err := CopyFileSection(w, &io.LimitedReader{R: f, N: 5000}, func() {})
	require.NoError(t, err, "EOF ends the copy like a reader")
	assert.Equal(t, int64(1000), n)
	assert.Equal(t, data, w.Bytes())
}

// TestCopyFileSectionDeadlines: a piece whose deadline expires after
// minFileProgress bytes is resumed under a new deadline; one that expires
// with less fails the copy.
func TestCopyFileSectionDeadlines(t *testing.T) {
	f, data := tempFile(t, 2*filePiece)
	timeout := &os.PathError{Op: "write", Err: os.ErrDeadlineExceeded}
	w := &pieceWriter{}
	w.fail = func(i int, lr *io.LimitedReader) (int64, error) {
		if i == 0 {
			// Slow: a part of the piece within the deadline.
			n, _ := w.Buffer.ReadFrom(&io.LimitedReader{R: lr, N: minFileProgress})
			return n, timeout
		}
		return w.Buffer.ReadFrom(lr)
	}
	arms := 0
	n, err := CopyFileSection(w, &io.LimitedReader{R: f, N: int64(len(data))}, func() { arms++ })
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), n)
	assert.Equal(t, data, w.Bytes())
	assert.Equal(t, 3, arms)

	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	stalled := &pieceWriter{}
	stalled.fail = func(int, *io.LimitedReader) (int64, error) { return minFileProgress - 1, timeout }
	n, err = CopyFileSection(stalled, &io.LimitedReader{R: f, N: int64(len(data))}, func() {})
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "stalled: below the progress of one write")
	assert.Equal(t, int64(minFileProgress-1), n)

	other := &pieceWriter{}
	other.fail = func(int, *io.LimitedReader) (int64, error) { return filePiece / 2, errors.New("broken pipe") }
	_, err = CopyFileSection(other, &io.LimitedReader{R: f, N: int64(len(data))}, func() {})
	require.EqualError(t, err, "broken pipe")
}

// TestSendGuardReadFrom: CopyBody hands a file section to the ReadFrom of
// the response writer piece by piece and any other body through Write;
// the client gets the same bytes either way.
func TestSendGuardReadFrom(t *testing.T) {
	f, data := tempFile(t, filePiece+12345)
	var got []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy := &readFromSpy{ResponseWriter: w, got: &got}
		var src io.Reader = bytes.NewReader(data[7:])
		if r.URL.Path == "/file" {
			_, err := f.Seek(7, io.SeekStart)
			require.NoError(t, err)
			src = &io.LimitedReader{R: f, N: int64(len(data) - 7)}
		}
		_, err := CopyBody(spy, src, DefaultSendTimeout)
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	for _, path := range []string{"/file", "/memory"} {
		got = nil
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, data[7:], body, path)
		if path == "/file" {
			assert.Equal(t, []int64{filePiece, int64(len(data) - 7 - filePiece)}, got, "file sections reach ReadFrom")
		} else {
			assert.Empty(t, got, "other bodies go through Write")
		}
	}
}

// readFromSpy records the file sections handed to its ReadFrom.
type readFromSpy struct {
	http.ResponseWriter
	got *[]int64
}

func (s *readFromSpy) ReadFrom(r io.Reader) (int64, error) {
	if lr, ok := FileSection(r); ok {
		*s.got = append(*s.got, lr.N)
	}
	return s.ResponseWriter.(io.ReaderFrom).ReadFrom(r)
}

func (s *readFromSpy) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// BenchmarkCopyBodyFile serves a 16 MiB file section over HTTP/1.1 on
// loopback with CopyBody, as a file section (sendfile) and hidden behind a
// plain reader (read and write through user space, the copy before
// T-9.2), to a client that discards it:
//
//	go test -run '^$' -bench CopyBodyFile -count 10 ./internal/transport/http/httpx/ > out.txt
//	benchstat -col /src out.txt
func BenchmarkCopyBodyFile(b *testing.B) {
	const size = 16 << 20
	name := filepath.Join(b.TempDir(), "f")
	data := make([]byte, size)
	_, _ = rand.Read(data)
	require.NoError(b, os.WriteFile(name, data, 0o600))
	for _, mode := range []string{"file", "reader"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f, err := os.Open(name) // #nosec G304 -- a benchmark file
			if err != nil {
				b.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			w.Header().Set("Content-Length", strconv.Itoa(size))
			var src io.Reader = &io.LimitedReader{R: f, N: size}
			if mode == "reader" {
				src = struct{ io.Reader }{src}
			}
			if _, err := CopyBody(w, src, DefaultSendTimeout); err != nil {
				b.Error(err)
			}
		}))
		client := srv.Client()
		b.Run("src="+mode, func(b *testing.B) {
			b.SetBytes(size)
			b.RunParallel(func(pb *testing.PB) {
				buf := make([]byte, 256<<10)
				for pb.Next() {
					resp, err := client.Get(srv.URL)
					if err != nil {
						b.Error(err)
						return
					}
					_, err = io.CopyBuffer(io.Discard, struct{ io.Reader }{resp.Body}, buf)
					_ = resp.Body.Close()
					if err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
		srv.Close()
	}
}
