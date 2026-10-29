package media_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// forensicFixture is a video with a forensic rendition: its A variant
// (the fixture) and its B variant (the same file with its media bytes
// altered: the same boxes, the same segments, other pictures).
type forensicFixture struct {
	s          *store
	protected  media.Service
	clear      media.Service
	src        media.Source
	renditionA string
	renditionB string
}

func newForensicFixture(t *testing.T, forensic bool) *forensicFixture {
	t.Helper()
	s, objects := newStore(t, "h264_aac.mp4")
	src := media.Source{Bucket: "videos", Key: "lesson.mp4"}
	s.put(src.Key, s.objects["h264_aac.mp4"])
	dir := domain.MediaSidecarDir(src.Key, "")
	a := s.objects["h264_aac.mp4"]
	b := append([]byte(nil), a...)
	mdat := bytes.Index(b, []byte("mdat"))
	require.Positive(t, mdat)
	size := int(binary.BigEndian.Uint32(b[mdat-4:]))
	for i := mdat + 64; i < mdat-4+size; i += 97 {
		b[i] ^= 0x5a
	}
	f := &forensicFixture{s: s, src: src,
		renditionA: dir + domain.MediaRenditionsDir + domain.MediaRenditionFile("360p", false),
		renditionB: dir + domain.MediaRenditionsDir + domain.MediaRenditionFile("360p", true)}
	s.put(f.renditionA, a)
	s.put(f.renditionB, b)
	status, err := json.Marshal(domain.MediaStatus{State: domain.MediaStateDone, ETag: s.info[src.Key].ETag,
		Renditions: []domain.MediaRenditionOutput{{Name: "360p", Width: 640, Height: 360, Bandwidth: 1, Forensic: forensic}}})
	require.NoError(t, err)
	s.put(dir+domain.MediaStatusFile, status)

	// The trace records, stored and listed like objects.
	objects.EXPECT().PutObject(tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
			data, err := io.ReadAll(in.Reader)
			require.NoError(t, err)
			s.put(in.Key, data)
			return s.info[in.Key], nil
		}).Maybe()
	objects.EXPECT().ListObjects(tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error) {
			var out domain.ListObjectsResult
			for k := range s.objects {
				if strings.HasPrefix(k, in.Prefix) {
					out.Objects = append(out.Objects, s.info[k])
				}
			}
			slices.SortFunc(out.Objects, func(x, y domain.ObjectInfo) int { return strings.Compare(x.Key, y.Key) })
			return out, nil
		}).Maybe()

	p := domain.MediaProtection{Enabled: true, Forensic: true}
	require.NoError(t, p.Validate())
	f.protected, err = media.New(media.Deps{Objects: objects, Protections: protections{p}},
		media.WithTargetSegment(time.Second), media.WithProtectionKey([]byte("0123456789abcdef0123456789abcdef")))
	require.NoError(t, err)
	f.clear = newService(t, objects)
	return f
}

// bitsOf plays every segment of the rendition as viewer and tells, for
// each, the variant it came from (1 for B).
func (f *forensicFixture) bitsOf(t *testing.T, viewer string) string {
	t.Helper()
	ctx := context.Background()
	src := f.src
	src.Rendition, src.Viewer = "360p", viewer
	key, err := f.protected.Key(ctx, src, 0)
	require.NoError(t, err)
	periods, err := f.protected.Periods(ctx, src)
	require.NoError(t, err)
	p, err := f.clear.Describe(ctx, media.Source{Bucket: "videos", Key: f.renditionA})
	require.NoError(t, err)
	var bits strings.Builder
	for n := range len(p.Segments) {
		if n/media.KeyPeriod != 0 {
			key, err = f.protected.Key(ctx, src, n/media.KeyPeriod)
			require.NoError(t, err)
		}
		got, err := f.protected.Segment(ctx, src, n)
		require.NoError(t, err)
		plain := decrypt(t, key, got, n)
		a, err := f.clear.Segment(ctx, media.Source{Bucket: "videos", Key: f.renditionA}, n)
		require.NoError(t, err)
		b, err := f.clear.Segment(ctx, media.Source{Bucket: "videos", Key: f.renditionB}, n)
		require.NoError(t, err)
		require.NotEqual(t, a, b, "segment %d differs between the variants", n)
		switch {
		case bytes.Equal(plain, a):
			bits.WriteByte('0')
		case bytes.Equal(plain, b):
			bits.WriteByte('1')
		default:
			t.Fatalf("segment %d is neither variant", n)
		}
	}
	require.GreaterOrEqual(t, periods, 1)
	return bits.String()
}

// TestForensicSegments: every segment of a stream comes from the variant
// the code of its token selects (with the keys of the A variant): two
// tokens get two sequences; the source and the renditions without both
// variants never play for a token; the owner plays them all.
//
// covers: S-OBJ-19
func TestForensicSegments(t *testing.T) {
	f := newForensicFixture(t, true)
	ctx := context.Background()
	one := f.bitsOf(t, "0123456789abcdef0123456789abcdef")
	assert.Equal(t, one, f.bitsOf(t, "0123456789abcdef0123456789abcdef"), "the same token, the same sequence")
	differ := false
	for _, sid := range []string{"aa", "bb", "cc", "dd", "ee", "ff"} {
		if f.bitsOf(t, sid) != one {
			differ = true
		}
	}
	assert.True(t, differ, "other tokens, other sequences")

	viewer := f.src
	viewer.Viewer = "aa"
	master, err := f.protected.MasterPlaylist(ctx, viewer, links{})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(master), "#EXT-X-STREAM-INF"), "only the marked rendition: %s", master)
	assert.Contains(t, string(master), "r=360p")
	_, err = f.protected.MediaPlaylist(ctx, viewer, links{})
	require.ErrorIs(t, err, domain.ErrServiceUnavailable, "the source is not marked")
	_, err = f.protected.Segment(ctx, viewer, 0)
	require.ErrorIs(t, err, domain.ErrServiceUnavailable)

	owner, err := f.protected.MasterPlaylist(ctx, f.src, links{})
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(owner), "#EXT-X-STREAM-INF"), "the owner plays the source too")

	unmarked := newForensicFixture(t, false)
	_, err = unmarked.protected.MasterPlaylist(ctx, viewer, links{})
	require.ErrorIs(t, err, domain.ErrServiceUnavailable, "not ready: never unmarked")
}

// TestForensicTrace: the trace record of a token is stored encrypted and
// found back from the bits its streams carry; a record not sealed by the
// deployment is ignored.
//
// covers: S-OBJ-19
func TestForensicTrace(t *testing.T) {
	f := newForensicFixture(t, true)
	ctx := context.Background()
	sids := []string{"0123456789abcdef0123456789abcdef", "aa", "bb", "cc"}
	for i, sid := range sids {
		require.NoError(t, f.protected.RecordTrace(ctx, domain.MediaTrace{Bucket: "videos", Key: "lesson.mp4", SessionID: sid,
			Subject: "student-" + sid, Watermark: "mario@example.com", IssuedAt: int64(1000 + i), ExpiresAt: 9999}))
	}
	var records []string
	for k, v := range f.s.objects {
		if strings.HasPrefix(k, domain.MediaTracePrefix) {
			records = append(records, k)
			assert.NotContains(t, string(v), "student-", "encrypted")
		}
	}
	require.Len(t, records, len(sids))
	// A forged record next to them.
	f.s.put(domain.MediaTracePrefix+"0000/0000000000000000.ff", []byte("forged record, not sealed by this deployment"))

	read := f.bitsOf(t, sids[0])
	matches, err := f.protected.Trace(ctx, "videos", read, 10)
	require.NoError(t, err)
	require.NotEmpty(t, matches)
	found := slices.IndexFunc(matches, func(m domain.MediaTraceMatch) bool { return m.SessionID == sids[0] })
	require.GreaterOrEqual(t, found, 0, "%+v", matches)
	m := matches[found]
	assert.Equal(t, "student-"+sids[0], m.Subject)
	assert.Equal(t, m.Compared, m.Agree)
	assert.Equal(t, len(read), m.Compared)
	assert.Len(t, m.Code, 16)

	_, err = f.protected.Trace(ctx, "videos", "???", 10)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = f.protected.Trace(ctx, "videos", "01x2", 10)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
