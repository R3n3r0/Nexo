package media_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// protections is a fixed protection for every bucket.
type protections struct{ p domain.MediaProtection }

func (f protections) GetMediaProtection(context.Context, string) (domain.MediaProtection, error) {
	return f.p, nil
}

// noKeys is a route that serves no keys (a distribution).
type noKeys struct{ links }

func (noKeys) Key(string, int) string { return "" }

func newProtectedService(t testing.TB, m object.Service, prefix string, key []byte) (media.Service, media.Service) {
	t.Helper()
	p := domain.MediaProtection{Enabled: true, Prefix: prefix}
	require.NoError(t, p.Validate())
	protected, err := media.New(media.Deps{Objects: m, Protections: protections{p}},
		media.WithTargetSegment(time.Second), media.WithProtectionKey(key))
	require.NoError(t, err)
	return protected, newService(t, m)
}

// decrypt reverses HLS METHOD=AES-128: AES-128-CBC, the media sequence
// number as IV, PKCS#7 padding.
func decrypt(t *testing.T, key, data []byte, n int) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	require.Zero(t, len(data)%aes.BlockSize)
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(n))
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	require.True(t, pad >= 1 && pad <= aes.BlockSize)
	for _, b := range out[len(out)-pad:] {
		require.Equal(t, byte(pad), b)
	}
	return out[:len(out)-pad]
}

// TestProtectedSegments: the segments of a protected video are the clear
// segments encrypted with the key of their period and their sequence
// number as IV; the init segment stays clear; the keys depend on the
// deployment key and the content.
//
// covers: S-OBJ-17
func TestProtectedSegments(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	key := []byte("0123456789abcdef0123456789abcdef")
	protected, clear := newProtectedService(t, objects, "", key)
	ctx := context.Background()
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}

	periods, err := protected.Periods(ctx, src)
	require.NoError(t, err)
	require.Equal(t, 1, periods)
	k, err := protected.Key(ctx, src, 0)
	require.NoError(t, err)
	require.Len(t, k, 16)
	_, err = protected.Key(ctx, src, 1)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = clear.Key(ctx, src, 0)
	require.ErrorIs(t, err, domain.ErrNotFound, "no key without protection")

	for n := range 4 {
		want, err := clear.Segment(ctx, src, n)
		require.NoError(t, err)
		got, err := protected.Segment(ctx, src, n)
		require.NoError(t, err)
		assert.NotEqual(t, want, got[:min(len(got), len(want))])
		assert.Equal(t, want, decrypt(t, k, got, n), "segment %d", n)
	}
	initClear, err := clear.Init(ctx, src)
	require.NoError(t, err)
	initProtected, err := protected.Init(ctx, src)
	require.NoError(t, err)
	assert.Equal(t, initClear, initProtected)

	// Another deployment key, or another content, gives another key.
	other, _ := newProtectedService(t, objects, "", []byte("another deployment key, 32 bytes"))
	k2, err := other.Key(ctx, src, 0)
	require.NoError(t, err)
	assert.NotEqual(t, k, k2)
	data := append([]byte(nil), s.objects[src.Key]...)
	s.put(src.Key, append(data, 0))
	k3, err := protected.Key(ctx, src, 0)
	require.NoError(t, err)
	assert.NotEqual(t, k, k3, "overwriting a video changes its keys")
}

// TestProtectedPlaylist: the media playlist of a protected video names
// the key of each period after the clear init segment; a route without
// keys is refused it; a video outside the prefix stays clear.
//
// covers: S-OBJ-17
func TestProtectedPlaylist(t *testing.T) {
	s, objects := newStore(t, "h264_aac.mp4")
	s.put("courses/intro.mp4", s.objects["h264_aac.mp4"])
	protected, _ := newProtectedService(t, objects, "courses/", []byte("0123456789abcdef"))
	ctx := context.Background()
	src := media.Source{Bucket: "videos", Key: "courses/intro.mp4"}
	rev := s.info[src.Key].ETag

	playlist, err := protected.MediaPlaylist(ctx, src, links{})
	require.NoError(t, err)
	text := string(playlist)
	keyTag := `#EXT-X-KEY:METHOD=AES-128,URI="key?rev=` + rev + `&i=0"` + "\n"
	assert.Equal(t, 1, strings.Count(text, "#EXT-X-KEY:"))
	assert.Less(t, strings.Index(text, "#EXT-X-MAP:"), strings.Index(text, keyTag))
	assert.Less(t, strings.Index(text, keyTag), strings.Index(text, "#EXTINF:"))

	_, err = protected.MediaPlaylist(ctx, src, noKeys{})
	require.ErrorIs(t, err, domain.ErrAccessDenied)

	clearText, err := protected.MediaPlaylist(ctx, media.Source{Bucket: "videos", Key: "h264_aac.mp4"}, noKeys{})
	require.NoError(t, err)
	assert.NotContains(t, string(clearText), "#EXT-X-KEY")
	prot, err := protected.Protection(ctx, media.Source{Bucket: "videos", Key: "h264_aac.mp4"})
	require.NoError(t, err)
	assert.Nil(t, prot)
}

// TestSessionSegments: the segments of a session of the player are
// encrypted with AES-128-GCM under the key of that session (bound to the
// segment number), every session has its own keys, and its playlist names
// no key.
//
// covers: S-OBJ-18
func TestSessionSegments(t *testing.T) {
	_, objects := newStore(t, "h264_aac.mp4")
	protected, clear := newProtectedService(t, objects, "", []byte("0123456789abcdef0123456789abcdef"))
	ctx := context.Background()
	std := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	one, two := std, std
	one.Session, two.Session = "aaaa", "bbbb"

	k1, err := protected.Key(ctx, one, 0)
	require.NoError(t, err)
	k2, err := protected.Key(ctx, two, 0)
	require.NoError(t, err)
	kStd, err := protected.Key(ctx, std, 0)
	require.NoError(t, err)
	assert.NotEqual(t, k1, k2, "a key per session")
	assert.NotEqual(t, k1, kStd)

	open := func(key []byte, n int, sealed []byte) ([]byte, error) {
		block, err := aes.NewCipher(key)
		require.NoError(t, err)
		aead, err := cipher.NewGCM(block)
		require.NoError(t, err)
		return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(fmt.Sprint(n)))
	}
	for n := range 3 {
		want, err := clear.Segment(ctx, std, n)
		require.NoError(t, err)
		got, err := protected.Segment(ctx, one, n)
		require.NoError(t, err)
		plain, err := open(k1, n, got)
		require.NoError(t, err)
		assert.Equal(t, want, plain, "segment %d", n)
		_, err = open(k2, n, got)
		require.Error(t, err, "another session cannot open it")
		_, err = open(k1, n+1, got)
		require.Error(t, err, "nor as another segment")
	}

	playlist, err := protected.MediaPlaylist(ctx, one, noKeys{})
	require.NoError(t, err)
	assert.NotContains(t, string(playlist), "#EXT-X-KEY")
	assert.Contains(t, string(playlist), "#EXTINF:")
}

// TestDeriveProtectionKey: the key depends on every input.
//
// covers: S-OBJ-17
func TestDeriveProtectionKey(t *testing.T) {
	k := media.DeriveProtectionKey("root", "secret", "dep")
	assert.Len(t, k, 32)
	assert.Equal(t, k, media.DeriveProtectionKey("root", "secret", "dep"))
	assert.NotEqual(t, k, media.DeriveProtectionKey("root", "secret2", "dep"))
	assert.NotEqual(t, k, media.DeriveProtectionKey("root2", "secret", "dep"))
	assert.NotEqual(t, k, media.DeriveProtectionKey("root", "secret", "dep2"))
}

// TestProtectedSegmentsDecode: ffmpeg plays the encrypted presentation
// with its keys, every frame of the source; without them it decodes
// nothing.
//
// covers: S-OBJ-17
func TestProtectedSegmentsDecode(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
	for _, file := range []string{"h264_aac.mp4", "hevc_aac.mp4"} {
		t.Run(file, func(t *testing.T) {
			_, objects := newStore(t, file)
			svc, _ := newProtectedService(t, objects, "", []byte("0123456789abcdef"))
			ctx := context.Background()
			src := media.Source{Bucket: "videos", Key: file}
			dir := t.TempDir()
			p, err := svc.Describe(ctx, src)
			require.NoError(t, err)
			playlist, err := svc.MediaPlaylist(ctx, src, fileLinks{})
			require.NoError(t, err)
			require.Contains(t, string(playlist), `#EXT-X-KEY:METHOD=AES-128,URI="key0.bin"`)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "index.m3u8"), playlist, 0o600))
			init, err := svc.Init(ctx, src)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "init.mp4"), init, 0o600))
			for n := range p.Segments {
				seg, err := svc.Segment(ctx, src, n)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("seg%d.m4s", n)), seg, 0o600))
			}
			periods, err := svc.Periods(ctx, src)
			require.NoError(t, err)
			for i := range periods {
				k, err := svc.Key(ctx, src, i)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("key%d.bin", i)), k, 0o600))
			}
			got := probe(t, filepath.Join(dir, "index.m3u8"))
			want := probe(t, filepath.Join("testdata", file))
			require.Equal(t, len(want), len(got), "streams")
			for codec, w := range want {
				assert.Equal(t, w.frames, got[codec].frames, "%s frames", codec)
			}

			// A downloader without the keys saves noise.
			for i := range periods {
				require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("key%d.bin", i)), make([]byte, 16), 0o600))
			}
			out, _ := exec.Command("ffprobe", "-v", "error", "-allowed_extensions", "ALL", "-count_frames",
				"-show_entries", "stream=codec_name,nb_read_frames", "-of", "csv=p=0", filepath.Join(dir, "index.m3u8")).CombinedOutput() // #nosec G204 -- test paths
			for codec, w := range want {
				assert.NotContains(t, string(out), codec+","+w.frames, "decoded with wrong keys")
			}
		})
	}
}
