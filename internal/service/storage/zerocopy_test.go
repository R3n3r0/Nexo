package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// fileSections is a writer that copies from readers itself and records
// whether it was handed file sections (zero-copy, see WriteTo).
type fileSections struct {
	bytes.Buffer
	files int
}

func (w *fileSections) ReadFrom(r io.Reader) (int64, error) {
	if lr, ok := r.(*io.LimitedReader); ok {
		if _, ok := lr.R.(*os.File); ok {
			w.files++
		}
	}
	return w.Buffer.ReadFrom(r)
}

// TestReadFileStreamWriteTo: a stream of ReadFileStream hands the rest of
// its section to a writer with io.ReaderFrom as a file section and copies
// it to any other writer; the bytes are those of the section, from where
// the reads stopped, and the stream ends at the section end.
func TestReadFileStreamWriteTo(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	payload := randomPayload(t, 300000)
	require.NoError(t, d.CreateFile(ctx, domain.SystemBucket, "tmp/z/part.1", int64(len(payload)), bytes.NewReader(payload)))
	for _, tc := range []struct{ off, length, read int64 }{
		{0, -1, 0}, {1000, 5000, 0}, {1000, 5000, 100}, {299999, 1, 0}, {300000, 0, 0}, {10, 200000, 200000},
	} {
		for _, plain := range []bool{false, true} {
			rc, err := d.ReadFileStream(ctx, domain.SystemBucket, "tmp/z/part.1", tc.off, tc.length)
			require.NoError(t, err)
			end := int64(len(payload))
			if tc.length >= 0 {
				end = tc.off + tc.length
			}
			head := make([]byte, tc.read)
			_, err = io.ReadFull(rc, head)
			require.NoError(t, err)
			w := &fileSections{}
			var n int64
			if plain {
				n, err = rc.(io.WriterTo).WriteTo(struct{ io.Writer }{w})
			} else {
				n, err = io.Copy(w, rc)
			}
			require.NoError(t, err)
			assert.Equal(t, end-tc.off-tc.read, n, "%+v", tc)
			assert.True(t, bytes.Equal(payload[tc.off+tc.read:end], w.Bytes()), "%+v plain=%v", tc, plain)
			if !plain && n > 0 {
				assert.Equal(t, 1, w.files)
			} else {
				assert.Zero(t, w.files)
			}
			// The stream is at its end: nothing more to read.
			m, err := rc.Read(make([]byte, 1))
			assert.Zero(t, m)
			assert.ErrorIs(t, err, io.EOF)
			require.NoError(t, rc.Close())
		}
	}
}
