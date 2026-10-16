package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// setCalls records the O_DIRECT switches of a file and forwards them to
// next (nil: no syscall, the switch only recorded).
type setCalls struct {
	mu    sync.Mutex
	calls []bool
	next  func(*os.File, bool) error
	err   error
}

func (c *setCalls) set(f *os.File, on bool) error {
	c.mu.Lock()
	c.calls = append(c.calls, on)
	c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.next != nil {
		return c.next(f, on)
	}
	return nil
}

func (c *setCalls) got() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bool(nil), c.calls...)
}

func randomPayload(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestAlignedBuffer(t *testing.T) {
	for _, align := range []int{1, 512, 4096, 65536} {
		for _, size := range []int{1, 4096, copyBufferSize} {
			b := alignedBuffer(size, align)
			require.Len(t, b, size)
			require.Equal(t, size, cap(b), "appends must not grow past the aligned window")
			assert.Zero(t, uintptr(unsafe.Pointer(unsafe.SliceData(b)))%uintptr(align), "align %d size %d", align, size)
		}
	}
	b := alignedBuffer(8192, 4096)
	assert.True(t, isAligned(b, 4096))
	assert.False(t, isAligned(b[1:4097], 4096), "unaligned address")
	assert.False(t, isAligned(b[:4000], 4096), "unaligned length")
	assert.False(t, isAligned(b[:0], 4096), "empty")
}

// writeThrough copies payload to a new file of dir with a directWriter the
// way CreateFile does, and returns the file content and the writer.
func writeThrough(t *testing.T, dio *directIO, payload []byte, size int64) ([]byte, *directWriter) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "part")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	s := &xlStorage{}
	s.dio.align = dio.align
	w := newDirectWriter(f, dio, nil)
	n, err := s.copyFull(w, bytes.NewReader(payload), size)
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), n)
	require.NoError(t, f.Sync())
	got, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	return got, w
}

// TestDirectWriterSwitches: a file of at least a buffer goes direct at its
// first whole buffer and back to the page cache for its unaligned tail; a
// smaller one never goes direct.
func TestDirectWriterSwitches(t *testing.T) {
	cases := []struct {
		name  string
		size  int
		known bool
		want  []bool
	}{
		{"small", 4096, true, nil},
		{"under a buffer", copyBufferSize - 1, true, nil},
		{"one buffer", copyBufferSize, true, []bool{true}},
		{"aligned multiple", 3 * copyBufferSize, true, []bool{true}},
		{"unaligned tail", 2*copyBufferSize + 12345, true, []bool{true, false}},
		{"aligned short tail", copyBufferSize + 8192, true, []bool{true}},
		{"unknown size, tail", copyBufferSize + 1, false, []bool{true, false}},
		{"unknown size, small", 100, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &setCalls{next: setDirectIO}
			if !directIOSupported {
				rec.next = nil
			}
			dio := &directIO{align: defaultDirectIOAlign, set: rec.set}
			payload := randomPayload(t, tc.size)
			size := int64(-1)
			if tc.known {
				size = int64(tc.size)
			}
			got, w := writeThrough(t, dio, payload, size)
			assert.True(t, bytes.Equal(payload, got), "content")
			assert.Equal(t, tc.want, rec.got())
			assert.False(t, dio.failed.Load())
			assert.Equal(t, int64(tc.size), w.off)
		})
	}
}

// TestDirectWriterFallbackOnSet: a file system that refuses O_DIRECT at
// fcntl (EINVAL, tmpfs before Linux 6.6) gets the file through the page
// cache, and the drive stops trying.
func TestDirectWriterFallbackOnSet(t *testing.T) {
	rec := &setCalls{err: syscall.EINVAL}
	dio := &directIO{align: defaultDirectIOAlign, set: rec.set}
	payload := randomPayload(t, 2*copyBufferSize+100)
	got, _ := writeThrough(t, dio, payload, -1)
	assert.True(t, bytes.Equal(payload, got))
	assert.True(t, dio.failed.Load())
	assert.False(t, dio.enabled())
	assert.Equal(t, []bool{true}, rec.got(), "one attempt, nothing to clear")

	// The next file does not try again.
	got, _ = writeThrough(t, dio, payload, int64(len(payload)))
	assert.True(t, bytes.Equal(payload, got))
	assert.Equal(t, []bool{true}, rec.got())
}

// TestDirectWriterFallbackOnWrite: a file system that accepts O_DIRECT at
// fcntl and refuses the I/O (EINVAL, some ZFS and overlay mounts) gets the
// rest of the write through the page cache, with no byte lost or doubled.
func TestDirectWriterFallbackOnWrite(t *testing.T) {
	for _, partial := range []int{0, 4096} {
		rec := &setCalls{}
		dio := &directIO{align: defaultDirectIOAlign, set: rec.set}
		f, err := os.CreateTemp(t.TempDir(), "part")
		require.NoError(t, err)
		var failures int
		w := newDirectWriter(f, dio, func(error) { failures++ })
		w.write = func(p []byte) (int, error) {
			if w.direct {
				// Refused after partial bytes.
				n, _ := f.Write(p[:partial])
				return n, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.EINVAL}
			}
			return f.Write(p)
		}
		s := &xlStorage{}
		payload := randomPayload(t, 3*copyBufferSize+7)
		n, err := s.copyFull(w, bytes.NewReader(payload), int64(len(payload)))
		require.NoError(t, err)
		require.Equal(t, int64(len(payload)), n)
		require.NoError(t, f.Close())
		got, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		assert.True(t, bytes.Equal(payload, got), "partial %d", partial)
		assert.Equal(t, 1, failures)
		assert.True(t, dio.failed.Load())
		assert.Equal(t, []bool{true, false}, rec.got())
	}
}

// TestDirectWriterOtherErrors: an error other than EINVAL is the error of
// the write (ENOSPC is not a reason to retry through the page cache).
func TestDirectWriterOtherErrors(t *testing.T) {
	rec := &setCalls{}
	dio := &directIO{align: defaultDirectIOAlign, set: rec.set}
	f, err := os.CreateTemp(t.TempDir(), "part")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	w := newDirectWriter(f, dio, nil)
	w.write = func([]byte) (int, error) { return 0, syscall.ENOSPC }
	_, err = (&xlStorage{}).copyFull(w, bytes.NewReader(make([]byte, copyBufferSize)), copyBufferSize)
	require.ErrorIs(t, err, syscall.ENOSPC)
	assert.False(t, dio.failed.Load())

	// A failure to clear O_DIRECT for the tail fails the write.
	rec = &setCalls{}
	dio = &directIO{align: defaultDirectIOAlign, set: func(f *os.File, on bool) error {
		if !on {
			return syscall.EBADF
		}
		return rec.set(f, on)
	}}
	w = newDirectWriter(f, dio, nil)
	_, err = (&xlStorage{}).copyFull(w, bytes.NewReader(make([]byte, copyBufferSize+1)), -1)
	require.ErrorIs(t, err, syscall.EBADF)
}

func newDirectDrive(t *testing.T, opts ...Option) *xlStorage {
	t.Helper()
	ep, err := domain.ParseEndpoint(filepath.Join(t.TempDir(), "drive"))
	require.NoError(t, err)
	d, err := NewLocal(Deps{Logger: slog.New(slog.DiscardHandler)}, ep, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	return d.(*xlStorage)
}

// TestCreateFileDirectIO: CreateFile of the drive writes its large files
// with O_DIRECT (when the platform has it), falls back on a refusal and
// writes everything through the page cache with WithoutDirectIO; the
// content read back is the same in every case.
func TestCreateFileDirectIO(t *testing.T) {
	ctx := context.Background()
	payload := randomPayload(t, 5*copyBufferSize/2+3)
	check := func(t *testing.T, d *xlStorage, path string) {
		t.Helper()
		require.NoError(t, d.CreateFile(ctx, domain.SystemBucket, path, -1, bytes.NewReader(payload)))
		rc, err := d.ReadFileStream(ctx, domain.SystemBucket, path, 0, -1)
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.True(t, bytes.Equal(payload, got))
	}

	t.Run("default", func(t *testing.T) {
		d := newDirectDrive(t)
		if runtime.GOOS != "linux" {
			assert.Zero(t, d.dio.align, "direct I/O is Linux only")
			check(t, d, "tmp/a/part.1")
			return
		}
		rec := &setCalls{next: setDirectIO}
		d.dio.set = rec.set
		check(t, d, "tmp/a/part.1")
		if d.dio.align == 0 {
			t.Skip("the file system of TMPDIR reports no direct I/O")
		}
		assert.Equal(t, []bool{true, false}, rec.got())
	})
	t.Run("refused", func(t *testing.T) {
		d := newDirectDrive(t)
		if d.dio.align == 0 {
			d.dio.align = defaultDirectIOAlign
		}
		rec := &setCalls{err: syscall.EINVAL}
		d.dio.set = rec.set
		check(t, d, "tmp/b/part.1")
		check(t, d, "tmp/c/part.1")
		assert.Equal(t, []bool{true}, rec.got(), "the drive stops trying")
		assert.True(t, d.dio.failed.Load())
	})
	for name, opt := range map[string]Option{"disabled": WithoutDirectIO(), "without fsync": WithoutFsync()} {
		t.Run(name, func(t *testing.T) {
			d := newDirectDrive(t, opt)
			assert.Zero(t, d.dio.align)
			rec := &setCalls{}
			d.dio.set = rec.set
			check(t, d, "tmp/d/part.1")
			assert.Empty(t, rec.got())
		})
	}
}

// TestSetDirectIOErrors: setDirectIO fails on a closed file.
func TestSetDirectIOErrors(t *testing.T) {
	if !directIOSupported {
		require.ErrorIs(t, setDirectIO(nil, true), errors.ErrUnsupported)
		return
	}
	f, err := os.CreateTemp(t.TempDir(), "x")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Error(t, setDirectIO(f, true))
}
