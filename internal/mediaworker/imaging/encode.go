package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/webp"
)

// Encoding settings, chosen with the benchmarks of docs/bench/media.md:
// sizes close to what image CDNs serve, at a cost a worker can afford.
const (
	// JPEGQuality is the quality of the JPEG variants.
	JPEGQuality = 85
	// WebPQuality and WebPMethod are the quality and the effort (0-6) of
	// the WebP variants (lossy, alpha kept).
	WebPQuality = 80
	WebPMethod  = 2
	// AVIFQuality and AVIFSpeed are the quality (0-100, libavif) and the
	// speed (0-10, aom) of the AVIF variants, 4:2:0.
	AVIFQuality      = 60
	AVIFAlphaQuality = 80
	AVIFSpeed        = 9
)

// pngEncoder returns the encoder of the PNG variants: the lossless
// fallback, compressed at the default level.
func pngEncoder() *png.Encoder { return &png.Encoder{CompressionLevel: png.DefaultCompression} }

// Encode writes a picture (premultiplied, tight) in a format of
// EncodeFormats; opaque says the picture has no transparent pixel. No
// metadata is written.
func Encode(format string, m *image.RGBA, opaque bool) ([]byte, error) {
	var b bytes.Buffer
	var err error
	switch format {
	case JPEG:
		if !opaque {
			m = flatten(m)
		}
		err = jpeg.Encode(&b, m, &jpeg.Options{Quality: JPEGQuality})
	case PNG:
		if opaque {
			err = pngEncoder().Encode(&b, m) // an opaque RGBA is written as RGB
		} else {
			err = pngEncoder().Encode(&b, unpremultiply(m))
		}
	case WebP:
		var src image.Image = m // opaque: the samples are the same as NRGBA
		if !opaque {
			src = unpremultiply(m)
		}
		err = webp.Encode(&b, src, webp.Options{Quality: WebPQuality, Method: WebPMethod})
	case AVIF:
		// The encoder takes premultiplied RGBA.
		err = avif.Encode(&b, m, avif.Options{Quality: AVIFQuality, QualityAlpha: AVIFAlphaQuality, Speed: AVIFSpeed,
			ChromaSubsampling: image.YCbCrSubsampleRatio420})
	default:
		return nil, fmt.Errorf("%w: cannot encode %q", ErrUnsupported, format)
	}
	if err != nil {
		return nil, fmt.Errorf("imaging: encode %s: %w", format, err)
	}
	return b.Bytes(), nil
}
