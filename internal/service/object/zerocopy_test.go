package object_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// sectionSpy is a writer that copies from readers itself (io.ReaderFrom,
// like the response writer of a download) and records the file sections
// it is handed (zero-copy, see httpx.FileSection) and the other readers.
type sectionSpy struct {
	bytes.Buffer
	files, others int
}

func (s *sectionSpy) ReadFrom(r io.Reader) (int64, error) {
	if lr, ok := r.(*io.LimitedReader); ok {
		if _, ok := lr.R.(*os.File); ok {
			s.files++
			return s.Buffer.ReadFrom(r)
		}
	}
	s.others++
	return s.Buffer.ReadFrom(r)
}

// TestGetObjectWriteTo: the body of a single-drive version hands its part
// files to a writer that takes them, whole or ranged, pinned or read under
// the lock, across parts; an erasure-coded or encrypted body is copied
// through Read. The bytes are the same either way.
func TestGetObjectWriteTo(t *testing.T) {
	ctx := context.Background()
	sizes := []int{20000, 30000, 12345}
	var whole []byte
	var bodies [][]byte
	for _, n := range sizes {
		b := make([]byte, n)
		_, err := rand.Read(b)
		require.NoError(t, err)
		bodies = append(bodies, b)
		whole = append(whole, b...)
	}
	ranges := []struct {
		name       string
		rng        *domain.Range
		part       int
		start, end int
	}{
		{"whole", nil, 0, 0, len(whole)},
		{"head", &domain.Range{Start: 0, End: 99}, 0, 0, 100},
		{"across parts", &domain.Range{Start: 19990, End: 50009}, 0, 19990, 50010},
		{"suffix", &domain.Range{Start: 25000, End: -1}, 0, 25000, len(whole)},
		{"part 2", nil, 2, 20000, 50000},
	}
	for _, kind := range []string{"fs", "erasure"} {
		for _, pinned := range []int{8, 0} {
			for _, sse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/pinned=%d/sse=%v", kind, pinned, sse), func(t *testing.T) {
					fx := newFixture(t, kind, object.WithPinnedReadParts(pinned))
					require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
					var req domain.SSERequest
					if sse {
						req = domain.SSERequest{Type: domain.SSES3}
					}
					up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp", SSE: req})
					require.NoError(t, err)
					var parts []domain.CompletePart
					for i, b := range bodies {
						p := uploadPart(t, fx.svc, "bkt", "mp", up.UploadID, i+1, b)
						parts = append(parts, domain.CompletePart{Number: i + 1, ETag: p.ETag})
					}
					_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, Parts: parts})
					require.NoError(t, err)

					for _, rc := range ranges {
						opts := object.GetObjectOptions{Range: rc.rng, PartNumber: rc.part}
						rd, err := fx.svc.GetObject(ctx, "bkt", "mp", opts)
						require.NoError(t, err, rc.name)
						spy := &sectionSpy{}
						n, err := io.Copy(spy, rd)
						require.NoError(t, err, rc.name)
						require.NoError(t, rd.Close())
						assert.Equal(t, int64(rc.end-rc.start), n, rc.name)
						assert.True(t, bytes.Equal(whole[rc.start:rc.end], spy.Bytes()), rc.name)
						if kind == "fs" && !sse {
							assert.Positive(t, spy.files, "%s: file sections", rc.name)
							assert.Zero(t, spy.others, rc.name)
						} else {
							assert.Zero(t, spy.files, "%s: no file section", rc.name)
						}
					}

					// Read then WriteTo: the copy goes on from where Read
					// stopped.
					rd, err := fx.svc.GetObject(ctx, "bkt", "mp", object.GetObjectOptions{})
					require.NoError(t, err)
					head := make([]byte, 25000)
					_, err = io.ReadFull(rd, head)
					require.NoError(t, err)
					spy := &sectionSpy{}
					_, err = io.Copy(spy, rd)
					require.NoError(t, err)
					require.NoError(t, rd.Close())
					assert.True(t, bytes.Equal(whole, append(head, spy.Bytes()...)))
				})
			}
		}
	}
}

// TestGetObjectWriteToPlainWriter: a writer without io.ReaderFrom gets the
// same bytes through Write.
func TestGetObjectWriteToPlainWriter(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, "fs")
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	data := make([]byte, 100000)
	_, err := rand.Read(data)
	require.NoError(t, err)
	_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: int64(len(data))})
	require.NoError(t, err)
	rd, err := fx.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{Range: &domain.Range{Start: 10, End: 99989}})
	require.NoError(t, err)
	defer func() { _ = rd.Close() }()
	var out bytes.Buffer
	n, err := rd.WriteTo(struct{ io.Writer }{&out})
	require.NoError(t, err)
	assert.Equal(t, int64(99980), n)
	assert.True(t, bytes.Equal(data[10:99990], out.Bytes()))
}
