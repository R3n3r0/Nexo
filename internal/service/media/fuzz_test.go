package media_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// bytesObjects is an object layer of one object, for the fuzzer (no mock:
// the expectations of a mock are per test, not per input).
type bytesObjects struct {
	object.Service
	data []byte
	info domain.ObjectInfo
}

func newBytesObjects(data []byte) *bytesObjects {
	sum := md5.Sum(data)
	return &bytesObjects{data: data, info: domain.ObjectInfo{Bucket: "videos", Key: "v.mp4", Size: int64(len(data)), ETag: hex.EncodeToString(sum[:])}}
}

func (o *bytesObjects) GetObjectInfo(context.Context, string, string, object.GetObjectOptions) (domain.ObjectInfo, error) {
	return o.info, nil
}

func (o *bytesObjects) GetObject(_ context.Context, _, _ string, opts object.GetObjectOptions) (*object.Reader, error) {
	lo, hi := int64(0), int64(len(o.data))
	if opts.Range != nil {
		if opts.Range.Start >= hi {
			return nil, domain.ErrInvalidRange
		}
		lo, hi = opts.Range.Start, min(opts.Range.End+1, hi)
	}
	return &object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(o.data[lo:hi])), Info: o.info, Offset: lo, Length: hi - lo}, nil
}

// FuzzPresentation feeds arbitrary bytes as an MP4 object: describing it,
// its playlists, its init segment and its first segments never panics.
func FuzzPresentation(f *testing.F) {
	for _, name := range []string{"h264_aac_faststart.mp4", "h264_only.mp4", "fragmented.mp4"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		// The head holds ftyp and, faststart, the moov box.
		f.Add(data[:min(len(data), 8192)])
	}
	f.Add([]byte("\x00\x00\x00\x08ftyp"))
	// One service for every input (the inputs of a worker run one at a
	// time): the cache keys hold the ETag of each input.
	objects := &bytesObjects{}
	svc, err := media.New(media.Deps{Objects: objects}, media.WithMaxIndex(1<<20), media.WithMaxSegment(1<<20), media.WithCacheSize(8<<20))
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		*objects = *newBytesObjects(data)
		src := media.Source{Bucket: "videos", Key: "v.mp4"}
		p, err := svc.Describe(context.Background(), src)
		if err != nil {
			return
		}
		_, _ = svc.MasterPlaylist(context.Background(), src, links{})
		_, _ = svc.MediaPlaylist(context.Background(), src, links{})
		_, _ = svc.Init(context.Background(), src)
		for n := range min(len(p.Segments), 3) {
			_, _ = svc.Segment(context.Background(), src, n)
		}
	})
}
