package media_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

// store is an object layer of fixed objects, counting the reads.
type store struct {
	objects map[string][]byte
	info    map[string]domain.ObjectInfo
	reads   atomic.Int64
}

func newStore(t testing.TB, files ...string) (*store, *objectmock.Service) {
	t.Helper()
	s := &store{objects: map[string][]byte{}, info: map[string]domain.ObjectInfo{}}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join("testdata", f))
		require.NoError(t, err)
		s.put(f, data)
	}
	m := objectmock.NewService(t)
	m.EXPECT().GetObjectInfo(tmock.Anything, "videos", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, _ object.GetObjectOptions) (domain.ObjectInfo, error) {
			info, ok := s.info[key]
			if !ok {
				return domain.ObjectInfo{}, domain.ErrObjectNotFound
			}
			return info, nil
		}).Maybe()
	m.EXPECT().GetObject(tmock.Anything, "videos", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, opts object.GetObjectOptions) (*object.Reader, error) {
			data, ok := s.objects[key]
			if !ok {
				return nil, domain.ErrObjectNotFound
			}
			if opts.Conditions.IfMatch != "" && opts.Conditions.IfMatch != s.info[key].ETag {
				return nil, domain.ErrPreconditionFailed
			}
			s.reads.Add(1)
			lo, hi := int64(0), int64(len(data))
			if opts.Range != nil {
				lo = opts.Range.Start
				if opts.Range.End >= 0 {
					hi = min(opts.Range.End+1, int64(len(data)))
				}
			}
			return &object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(data[lo:hi])), Info: s.info[key], Offset: lo, Length: hi - lo}, nil
		}).Maybe()
	return s, m
}

func (s *store) put(key string, data []byte) {
	sum := md5.Sum(data)
	s.objects[key] = data
	s.info[key] = domain.ObjectInfo{Bucket: "videos", Key: key, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:]), ContentType: "video/mp4"}
}

func newService(t testing.TB, objects object.Service) media.Service {
	t.Helper()
	svc, err := media.New(media.Deps{Objects: objects}, media.WithTargetSegment(time.Second))
	require.NoError(t, err)
	return svc
}

// links names the URIs like the console routes, readable in assertions.
type links struct{}

func (links) Media(rev string) string           { return "media.m3u8?rev=" + rev }
func (links) Init(rev string) string            { return "init.mp4?rev=" + rev }
func (links) Segment(rev string, n int) string  { return fmt.Sprintf("segment.m4s?rev=%s&n=%d", rev, n) }
func (links) Rendition(name, rev string) string { return "media.m3u8?r=" + name + "&rev=" + rev }

func TestNewRequiresObjects(t *testing.T) {
	_, err := media.New(media.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestDescribe(t *testing.T) {
	cases := []struct {
		file     string
		codec    string
		audio    bool
		duration time.Duration
	}{
		{"h264_aac.mp4", "avc1.", true, 4 * time.Second},
		{"h264_aac_faststart.mp4", "avc1.", true, 4 * time.Second},
		{"h264_only.mp4", "avc1.", false, 3 * time.Second},
		{"hevc_aac.mp4", "hvc1.", true, 4 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			s, objects := newStore(t, tc.file)
			p, err := newService(t, objects).Describe(context.Background(), media.Source{Bucket: "videos", Key: tc.file})
			require.NoError(t, err)
			assert.Equal(t, s.info[tc.file].ETag, p.Revision)
			assert.Equal(t, 160, p.Width)
			assert.Equal(t, 90, p.Height)
			assert.InDelta(t, 25, p.FrameRate, 0.01)
			assert.True(t, strings.HasPrefix(p.Codecs[0], tc.codec), "codecs %v", p.Codecs)
			assert.Equal(t, tc.audio, p.HasAudio)
			if tc.audio {
				assert.Equal(t, "mp4a.40.2", p.Codecs[1])
			}
			assert.InDelta(t, tc.duration.Seconds(), p.Duration.Seconds(), 0.05)
			// A keyframe every second, segments of a second.
			assert.Len(t, p.Segments, int(tc.duration/time.Second))
			for _, d := range p.Segments {
				assert.InDelta(t, 1, d.Seconds(), 0.05)
			}
			assert.Positive(t, p.AverageBandwidth)
			assert.GreaterOrEqual(t, p.PeakBandwidth, p.AverageBandwidth)
		})
	}
}

func TestPlaylists(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	rev := s.info[src.Key].ETag

	master, err := svc.MasterPlaylist(context.Background(), src, links{})
	require.NoError(t, err)
	text := string(master)
	assert.True(t, strings.HasPrefix(text, "#EXTM3U\n"))
	assert.Contains(t, text, `CODECS="avc1.`)
	assert.Contains(t, text, `,mp4a.40.2"`)
	assert.Contains(t, text, "RESOLUTION=160x90")
	assert.Contains(t, text, "FRAME-RATE=25.000")
	assert.Contains(t, text, "\nmedia.m3u8?rev="+rev+"\n")

	playlist, err := svc.MediaPlaylist(context.Background(), src, links{})
	require.NoError(t, err)
	text = string(playlist)
	assert.Contains(t, text, "#EXT-X-VERSION:7\n")
	assert.Contains(t, text, "#EXT-X-TARGETDURATION:1\n")
	assert.Contains(t, text, "#EXT-X-PLAYLIST-TYPE:VOD\n")
	assert.Contains(t, text, `#EXT-X-MAP:URI="init.mp4?rev=`+rev+`"`)
	assert.Equal(t, 4, strings.Count(text, "#EXTINF:"))
	assert.Contains(t, text, "segment.m4s?rev="+rev+"&n=3\n")
	assert.True(t, strings.HasSuffix(text, "#EXT-X-ENDLIST\n"))
}

// TestSegmentsDecode writes the presentation of every fixture to disk and
// decodes it with ffmpeg: every frame and audio packet of the source is
// played back, in sync.
func TestSegmentsDecode(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	for _, file := range []string{"h264_aac.mp4", "h264_aac_faststart.mp4", "h264_only.mp4", "hevc_aac.mp4"} {
		t.Run(file, func(t *testing.T) {
			_, objects := newStore(t, file)
			svc := newService(t, objects)
			src := media.Source{Bucket: "videos", Key: file}
			dir := t.TempDir()
			p, err := svc.Describe(context.Background(), src)
			require.NoError(t, err)
			playlist, err := svc.MediaPlaylist(context.Background(), src, fileLinks{})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "index.m3u8"), playlist, 0o600))
			init, err := svc.Init(context.Background(), src)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "init.mp4"), init, 0o600))
			for n := range p.Segments {
				seg, err := svc.Segment(context.Background(), src, n)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("seg%d.m4s", n)), seg, 0o600))
			}
			got := probe(t, filepath.Join(dir, "index.m3u8"))
			want := probe(t, filepath.Join("testdata", file))
			require.Equal(t, len(want), len(got), "streams")
			for codec, w := range want {
				g, ok := got[codec]
				require.True(t, ok, "stream %s", codec)
				assert.Equal(t, w.frames, g.frames, "%s frames", codec)
				// AAC priming may show as a slightly negative start.
				assert.InDelta(t, w.start, g.start, 0.05, "%s start", codec)
			}
		})
	}
}

// fileLinks names the files TestSegmentsDecode writes.
type fileLinks struct{}

func (fileLinks) Media(string) string             { return "index.m3u8" }
func (fileLinks) Init(string) string              { return "init.mp4" }
func (fileLinks) Segment(_ string, n int) string  { return fmt.Sprintf("seg%d.m4s", n) }
func (fileLinks) Rendition(name, _ string) string { return name + ".m3u8" }

// stream is what ffprobe reports of a stream.
type stream struct {
	frames string
	start  float64
}

// probe returns the streams of a file by codec: decoded frames and start
// time.
func probe(t *testing.T, path string) map[string]stream {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-allowed_extensions", "ALL", "-count_frames",
		"-show_entries", "stream=codec_name,start_time,nb_read_frames", "-of", "csv=p=0", path).CombinedOutput() // #nosec G204 -- test fixture paths
	require.NoError(t, err, string(out))
	streams := map[string]stream{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 3 {
			continue
		}
		start, err := strconv.ParseFloat(f[1], 64)
		require.NoError(t, err, line)
		streams[f[0]] = stream{frames: f[2], start: start}
	}
	return streams
}

func TestUnsupported(t *testing.T) {
	_, objects := newStore(t, "fragmented.mp4", "audio_only.m4a")
	svc := newService(t, objects)
	for _, key := range []string{"fragmented.mp4", "audio_only.m4a"} {
		_, err := svc.Describe(context.Background(), media.Source{Bucket: "videos", Key: key})
		assert.ErrorIs(t, err, media.ErrUnsupported, key)
	}
}

func TestNotMP4(t *testing.T) {
	s, objects := newStore(t)
	s.put("notes.txt", []byte("hello, this is not a video at all"))
	s.put("tiny", []byte("abc"))
	svc := newService(t, objects)
	for _, key := range []string{"notes.txt", "tiny"} {
		_, err := svc.Describe(context.Background(), media.Source{Bucket: "videos", Key: key})
		assert.ErrorIs(t, err, media.ErrUnsupported, key)
	}
}

func TestRevision(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4", Revision: s.info["h264_aac.mp4"].ETag}
	_, err := svc.Segment(context.Background(), src, 0)
	require.NoError(t, err)

	// The key is overwritten: the links of the old playlist fail.
	data, err := os.ReadFile(filepath.Join("testdata", "h264_aac_faststart.mp4"))
	require.NoError(t, err)
	s.put("h264_aac.mp4", data)
	_, err = svc.Segment(context.Background(), src, 0)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	_, err = svc.Init(context.Background(), src)
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	// A playlist without revision names the new content.
	p, err := svc.Describe(context.Background(), media.Source{Bucket: "videos", Key: "h264_aac.mp4"})
	require.NoError(t, err)
	assert.Equal(t, s.info["h264_aac.mp4"].ETag, p.Revision)
}

func TestSegmentOutOfRange(t *testing.T) {
	_, objects := newStore(t, "h264_aac.mp4")
	svc := newService(t, objects)
	for _, n := range []int{-1, 4, 1000} {
		_, err := svc.Segment(context.Background(), media.Source{Bucket: "videos", Key: "h264_aac.mp4"}, n)
		assert.ErrorIs(t, err, domain.ErrNotFound, "segment %d", n)
	}
}

func TestMissingObject(t *testing.T) {
	_, objects := newStore(t)
	_, err := newService(t, objects).Describe(context.Background(), media.Source{Bucket: "videos", Key: "nope.mp4"})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

func TestSSECustomerKeyObject(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	info := s.info["h264_aac.mp4"]
	info.Encryption.Type = domain.SSEC
	s.info["h264_aac.mp4"] = info
	_, err := newService(t, objects).Describe(context.Background(), media.Source{Bucket: "videos", Key: "h264_aac.mp4"})
	assert.ErrorIs(t, err, domain.ErrSSECustomerKeyRequired)
}

// TestIndexCached: the moov box is read once; a segment then costs one
// read of its bytes.
func TestIndexCached(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	_, err := svc.Describe(context.Background(), src)
	require.NoError(t, err)
	parsed := s.reads.Load()
	for range 3 {
		_, err = svc.MediaPlaylist(context.Background(), src, links{})
		require.NoError(t, err)
	}
	assert.Equal(t, parsed, s.reads.Load(), "the playlists read nothing")
	_, err = svc.Segment(context.Background(), src, 2)
	require.NoError(t, err)
	assert.Equal(t, parsed+1, s.reads.Load(), "one read per segment")
}

func TestLimits(t *testing.T) {
	_, objects := newStore(t, "h264_aac.mp4")
	svc, err := media.New(media.Deps{Objects: objects}, media.WithMaxIndex(64))
	require.NoError(t, err)
	_, err = svc.Describe(context.Background(), media.Source{Bucket: "videos", Key: "h264_aac.mp4"})
	assert.ErrorIs(t, err, media.ErrUnsupported)

	svc, err = media.New(media.Deps{Objects: objects}, media.WithMaxSegment(100))
	require.NoError(t, err)
	_, err = svc.Segment(context.Background(), media.Source{Bucket: "videos", Key: "h264_aac.mp4"}, 0)
	assert.ErrorIs(t, err, media.ErrUnsupported)
}

// TestDeclaredSamplesBeyondTheObject: a uniform sample size in stsz
// declares millions of samples in a few bytes; the service refuses them
// at once instead of planning their segments.
func TestDeclaredSamplesBeyondTheObject(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "h264_only.mp4"))
	require.NoError(t, err)
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	require.NoError(t, err)
	stsz := f.Moov.Traks[0].Mdia.Minf.Stbl.Stsz
	stsz.SampleUniformSize, stsz.SampleNumber, stsz.SampleSize = 1, 5_000_000, nil
	var out bytes.Buffer
	require.NoError(t, f.Encode(&out))

	s, objects := newStore(t)
	s.put("forged.mp4", out.Bytes())
	start := time.Now()
	_, err = newService(t, objects).Describe(context.Background(), media.Source{Bucket: "videos", Key: "forged.mp4"})
	assert.ErrorIs(t, err, media.ErrUnsupported)
	assert.Less(t, time.Since(start), time.Second)
}
