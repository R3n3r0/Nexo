package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/h265/heic"
	gwebp "github.com/gen2brain/webp"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

// Image is a decoded image, the source of the variants.
type Image struct {
	// Format of the source (JPEG, PNG...).
	Format string
	// Width and Height are the size of the upright picture.
	Width, Height int

	// The stored picture, one of the two, and the orientation still to
	// apply to it (EXIF of JPEG, TIFF, PNG and WebP; AVIF and HEIC come
	// upright). The picture is in sRGB, converted when decoded.
	rgba        *image.RGBA // premultiplied
	ycc         *image.YCbCr
	orientation int
	opaque      bool
}

// Config is what the header of an image says, before any decoding.
type Config struct {
	Format        string
	Width, Height int
}

// DecodeConfig reads the format and the stored size of an image from its
// header, and checks them against the limits.
func DecodeConfig(data []byte, lim Limits) (Config, error) {
	lim = lim.WithDefaults()
	if int64(len(data)) > lim.MaxBytes {
		return Config{}, fmt.Errorf("%w: %d bytes, more than %d", ErrTooLarge, len(data), lim.MaxBytes)
	}
	format, err := Detect(data)
	if err != nil {
		return Config{}, err
	}
	var cfg image.Config
	err = guard(format, func() error {
		cfg, err = decodeConfig(format, data)
		return err
	})
	if err != nil {
		return Config{}, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Config{}, fmt.Errorf("%w: %s of %dx%d pixels", ErrUnsupported, format, cfg.Width, cfg.Height)
	}
	if px := int64(cfg.Width) * int64(cfg.Height); px > lim.MaxPixels {
		return Config{}, fmt.Errorf("%w: %dx%d pixels, more than %d", ErrTooLarge, cfg.Width, cfg.Height, lim.MaxPixels)
	}
	return Config{Format: format, Width: cfg.Width, Height: cfg.Height}, nil
}

func decodeConfig(format string, data []byte) (image.Config, error) {
	r := bytes.NewReader(data)
	switch format {
	case JPEG:
		return jpeg.DecodeConfig(r)
	case PNG:
		return png.DecodeConfig(r)
	case GIF:
		return gif.DecodeConfig(r)
	case WebP:
		cfg, err := webp.DecodeConfig(r)
		if err != nil {
			return gwebp.DecodeConfig(bytes.NewReader(data))
		}
		return cfg, nil
	case BMP:
		return bmp.DecodeConfig(r)
	case TIFF:
		return tiff.DecodeConfig(r)
	case AVIF:
		return avif.DecodeConfig(r)
	case HEIC:
		return heic.DecodeConfig(r)
	}
	return image.Config{}, errors.New("unknown format")
}

// Decode decodes an image, refusing those above the limits from their
// header, before decoding them (ErrTooLarge), and the files that are not
// images of a supported format or are corrupt (ErrUnsupported).
func Decode(data []byte, lim Limits) (*Image, error) {
	lim = lim.WithDefaults()
	cfg, err := DecodeConfig(data, lim)
	if err != nil {
		return nil, err
	}
	var m image.Image
	var md meta
	err = guard(cfg.Format, func() error {
		m, md, err = decode(cfg.Format, data, lim)
		return err
	})
	if err != nil {
		return nil, err
	}
	b := m.Bounds()
	if b.Empty() || int64(b.Dx())*int64(b.Dy()) > lim.MaxPixels {
		return nil, fmt.Errorf("%w: %s decoded to %dx%d pixels", ErrTooLarge, cfg.Format, b.Dx(), b.Dy())
	}
	im := &Image{Format: cfg.Format, orientation: md.orientation, Width: b.Dx(), Height: b.Dy()}
	if im.orientation < 1 || im.orientation > 8 {
		im.orientation = 1
	}
	if im.orientation >= 5 {
		im.Width, im.Height = im.Height, im.Width
	}
	// A picture of another color space than sRGB is converted to sRGB, in
	// RGBA: a JPEG is kept in YCbCr (resized faster) only when it is sRGB.
	tr := newTransform(sourceProfile(md.color))
	if y, ok := m.(*image.YCbCr); ok && tr == nil && y.Rect.Min == (image.Point{}) && fastRatio(y.SubsampleRatio) {
		im.ycc, im.opaque = y, true
		return im, nil
	}
	im.rgba = toRGBA(m)
	if tr != nil {
		tr.apply(im.rgba)
	}
	_, ycc := m.(*image.YCbCr)
	im.opaque = ycc || opaque(im.rgba)
	return im, nil
}

// Opaque reports whether every pixel of the image is opaque (checked on
// the pixels, not the color model): the JPEG variants of an image with
// transparency are flattened onto white.
func (im *Image) Opaque() bool { return im.opaque }

// decode decodes a picture, and reads its orientation and color space.
func decode(format string, data []byte, lim Limits) (image.Image, meta, error) {
	if format == HEIC {
		m, ci, err := heic.DecodeColor(bytes.NewReader(data), heic.Options{AutoRotate: true, FrameSizeLimit: int(min(lim.MaxPixels, 1<<40))})
		return m, meta{orientation: 1, color: colorInfo{icc: ci.ICCP, hasCICP: true, primaries: ci.Primaries, transfer: ci.Transfer}}, err
	}
	m, err := decodePixels(format, data)
	return m, sourceMeta(format, data), err
}

// decodePixels decodes the picture of a format other than HEIC.
func decodePixels(format string, data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	switch format {
	case JPEG:
		return jpeg.Decode(r)
	case PNG:
		return png.Decode(r)
	case GIF:
		return gif.Decode(r) // the first frame
	case WebP:
		m, err := webp.Decode(r)
		if err != nil {
			// An animation (the first frame), or what x/image does not
			// decode.
			return gwebp.Decode(bytes.NewReader(data))
		}
		return m, nil
	case BMP:
		return bmp.Decode(r)
	case TIFF:
		return tiff.Decode(r)
	case AVIF:
		return avif.Decode(r, avif.Options{AutoRotate: true})
	}
	return nil, errors.New("unknown format")
}

// guard runs a decoder, turning its errors and panics (a corrupt file)
// into ErrUnsupported.
func guard(format string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %s: %v", ErrUnsupported, format, r)
		}
	}()
	if err = fn(); err != nil && !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrTooLarge) {
		err = fmt.Errorf("%w: %s: %w", ErrUnsupported, format, err)
	}
	return err
}

// fastRatio reports whether the YCbCr pictures of a subsampling convert
// to RGBA on the fast path of image/draw.
func fastRatio(r image.YCbCrSubsampleRatio) bool {
	switch r {
	case image.YCbCrSubsampleRatio444, image.YCbCrSubsampleRatio422, image.YCbCrSubsampleRatio420, image.YCbCrSubsampleRatio440:
		return true
	}
	return false
}
