package media_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// photoVariants are the variants of a 1000x750 photo with the widths 320
// and 640, in every format.
func photoVariants() []domain.MediaImageOutput {
	var out []domain.MediaImageOutput
	for _, f := range domain.MediaImageFormats() {
		out = append(out,
			domain.MediaImageOutput{Format: f, Width: 320, PixelWidth: 320, PixelHeight: 240, Size: 10},
			domain.MediaImageOutput{Format: f, Width: 0, PixelWidth: 1000, PixelHeight: 750, Size: 50},
			domain.MediaImageOutput{Format: f, Width: 640, PixelWidth: 640, PixelHeight: 480, Size: 20},
		)
	}
	return out
}

func TestImageAccepts(t *testing.T) {
	const chrome = "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8"
	const safari = "image/webp,image/avif,image/jxl,image/heic,image/heic-sequence,video/*;q=0.8,image/png,image/svg+xml,image/*;q=0.8,*/*;q=0.5"
	cases := []struct {
		accept string
		want   map[string]bool
	}{
		{chrome, map[string]bool{"avif": true, "webp": true, "jpeg": true, "png": true}},
		{safari, map[string]bool{"avif": true, "webp": true, "jpeg": true, "png": true}},
		{"", map[string]bool{"jpeg": true, "png": true}},
		{"*/*", map[string]bool{"jpeg": true, "png": true}},
		{"image/*", map[string]bool{"jpeg": true, "png": true}},
		{"image/png", map[string]bool{"png": true}},
		{"image/jpg", map[string]bool{"jpeg": true}},
		{"image/webp;q=0, image/*", map[string]bool{"jpeg": true, "png": true}},
		{"image/avif;q=0.5, image/jpeg;q=0", map[string]bool{"avif": true}},
		{"image/*;q=0, image/webp", map[string]bool{"webp": true}},
		{"text/html, garbage;;", map[string]bool{}},
	}
	for _, tc := range cases {
		for _, f := range domain.MediaImageFormats() {
			assert.Equal(t, tc.want[f], media.ImageAccepts(tc.accept, f), "%q accepts %s", tc.accept, f)
		}
	}
}

func TestSelectImage(t *testing.T) {
	all := photoVariants()
	cases := []struct {
		name     string
		variants []domain.MediaImageOutput
		req      media.ImageRequest
		format   string
		width    int
	}{
		{"auto, a modern browser, own size", all, media.ImageRequest{Accept: "image/avif,image/webp,*/*"}, "avif", 0},
		{"auto, webp only", all, media.ImageRequest{Accept: "image/webp,*/*"}, "webp", 0},
		{"auto, no Accept", all, media.ImageRequest{}, "jpeg", 0},
		{"auto keyword", all, media.ImageRequest{Format: "AUTO", Accept: "*/*"}, "jpeg", 0},
		{"png when asked by Accept", all, media.ImageRequest{Accept: "image/png"}, "png", 0},
		{"png for an image without jpeg", withoutFormat(all, "jpeg"), media.ImageRequest{Accept: "*/*"}, "png", 0},
		{"a client taking nothing produced", all, media.ImageRequest{Accept: "text/html"}, "jpeg", 0},
		{"explicit format", all, media.ImageRequest{Format: "png", Accept: "image/avif"}, "png", 0},
		{"jpg alias", all, media.ImageRequest{Format: "jpg"}, "jpeg", 0},
		{"the exact width", all, media.ImageRequest{Format: "webp", Width: 640}, "webp", 640},
		{"the narrowest wide enough", all, media.ImageRequest{Format: "webp", Width: 400}, "webp", 640},
		{"a small width", all, media.ImageRequest{Format: "webp", Width: 16}, "webp", 320},
		{"between the widths and the own size", all, media.ImageRequest{Format: "webp", Width: 700}, "webp", 0},
		{"wider than the image", all, media.ImageRequest{Format: "webp", Width: 4000}, "webp", 0},
		{"no full size: the widest", []domain.MediaImageOutput{
			{Format: "jpeg", Width: 160, PixelWidth: 160}, {Format: "jpeg", Width: 640, PixelWidth: 640}, {Format: "jpeg", Width: 320, PixelWidth: 320},
		}, media.ImageRequest{}, "jpeg", 640},
		{"no wide enough: the widest", []domain.MediaImageOutput{
			{Format: "jpeg", Width: 160, PixelWidth: 160}, {Format: "jpeg", Width: 320, PixelWidth: 320},
		}, media.ImageRequest{Width: 1000}, "jpeg", 320},
	}
	for _, tc := range cases {
		got, err := media.SelectImage(tc.variants, false, tc.req)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.format, got.Format, tc.name)
		assert.Equal(t, tc.width, got.Width, tc.name)
	}

	_, err := media.SelectImage(withoutFormat(all, "avif"), false, media.ImageRequest{Format: "avif"})
	assert.ErrorIs(t, err, domain.ErrNotFound, "a format not produced")
	_, err = media.SelectImage(nil, false, media.ImageRequest{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "no variant")
	_, err = media.SelectImage(all, false, media.ImageRequest{Format: "gif"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = media.SelectImage(all, false, media.ImageRequest{Width: -1})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestParseImageWidth(t *testing.T) {
	for in, want := range map[string]int{"": 0, "0": 0, "640": 640} {
		got, err := media.ParseImageWidth(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"-1", "x", "1.5"} {
		_, err := media.ParseImageWidth(in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, in)
	}
}

func withoutFormat(variants []domain.MediaImageOutput, format string) []domain.MediaImageOutput {
	var out []domain.MediaImageOutput
	for _, v := range variants {
		if v.Format != format {
			out = append(out, v)
		}
	}
	return out
}

// withImages stores the status of the worker for photo.jpg with the
// variants of photoVariants, each file holding its name.
func withImages(t *testing.T, s *store, state domain.MediaJobState, etag string) {
	t.Helper()
	dir := domain.MediaSidecarDir("photo.jpg", "")
	for _, v := range photoVariants() {
		s.put(dir+domain.MediaImageFile(v.Width, v.Format), []byte(domain.MediaImageFile(v.Width, v.Format)))
	}
	st := domain.MediaStatus{State: state, ETag: etag, Rule: "images", Width: 1000, Height: 750, Images: photoVariants(),
		Updated: time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)}
	data, err := json.Marshal(st)
	require.NoError(t, err)
	s.put(dir+domain.MediaStatusFile, data)
}

func TestImage(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t)
	s.put("photo.jpg", []byte("a photo"))
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "photo.jpg"}
	etag := s.info["photo.jpg"].ETag

	_, err := svc.Image(ctx, src, media.ImageRequest{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "never processed")
	withImages(t, s, domain.MediaStateProcessing, etag)
	_, err = svc.Image(ctx, src, media.ImageRequest{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "still processing")

	withImages(t, s, domain.MediaStateDone, `"`+etag+`"`)
	img, err := svc.Image(ctx, src, media.ImageRequest{Width: 300, Accept: "image/webp,*/*"})
	require.NoError(t, err)
	defer func() { _ = img.Content.Close() }()
	assert.Equal(t, "webp", img.Format)
	assert.Equal(t, 320, img.Width)
	assert.Equal(t, "image/webp", img.ContentType)
	assert.Equal(t, `"`+etag+`-webp-320"`, img.ETag)
	assert.Equal(t, time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), img.Modified)
	assert.Equal(t, int64(len("images/320.webp")), img.Size, "the size of the file")
	data, err := io.ReadAll(img.Content)
	require.NoError(t, err)
	assert.Equal(t, "images/320.webp", string(data))

	// Seeking reads from the new offset (ranges).
	_, err = img.Content.Seek(7, io.SeekStart)
	require.NoError(t, err)
	data, err = io.ReadAll(img.Content)
	require.NoError(t, err)
	assert.Equal(t, "320.webp", string(data))
	end, err := img.Content.Seek(0, io.SeekEnd)
	require.NoError(t, err)
	assert.Equal(t, img.Size, end)

	_, err = svc.Image(ctx, src, media.ImageRequest{Format: "bmp"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// A missing file of the worker.
	delete(s.objects, domain.MediaSidecarDir("photo.jpg", "")+"images/full.png")
	delete(s.info, domain.MediaSidecarDir("photo.jpg", "")+"images/full.png")
	_, err = svc.Image(ctx, src, media.ImageRequest{Format: "png"})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	// The photo is overwritten: the variants no longer apply.
	s.put("photo.jpg", []byte("another photo"))
	_, err = svc.Image(ctx, src, media.ImageRequest{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestSelectImageWithAlpha(t *testing.T) {
	all := photoVariants()
	for name, tc := range map[string]struct {
		accept, format string
	}{
		"an old browser gets PNG, not a flattened JPEG": {"*/*", "png"},
		"no Accept":                        {"", "png"},
		"a modern browser still gets AVIF": {"image/avif,image/webp,*/*", "avif"},
		"a client asking for JPEG gets it": {"image/jpeg", "jpeg"},
		"nothing accepted: PNG first":      {"text/html", "png"},
	} {
		got, err := media.SelectImage(all, true, media.ImageRequest{Accept: tc.accept})
		require.NoError(t, err, name)
		assert.Equal(t, tc.format, got.Format, name)
	}
}
