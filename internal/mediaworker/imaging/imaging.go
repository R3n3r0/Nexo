// Package imaging derives the variants of an image for the media worker:
// it decodes JPEG, PNG, GIF (first frame), WebP, BMP, TIFF, AVIF and HEIC
// (recognized by their content, not their name), converts it to sRGB from
// the color space it declares (ICC profile, nclx or cICP code points, PNG
// gAMA and cHRM: color.go), turns it upright (EXIF orientation of JPEG,
// TIFF, PNG and WebP, irot/imir of AVIF and HEIC), resizes it with area
// averaging on premultiplied alpha, and encodes AVIF, WebP, JPEG and PNG
// variants in parallel, in sRGB and without any metadata of the source
// (no EXIF, no GPS position, no ICC profile).
//
// Everything is Go or WebAssembly run by wazero: the package builds with
// CGO_ENABLED=0. HEIC is decoded by github.com/gen2brain/h265 (pure Go),
// AVIF by github.com/gen2brain/avif (libavif, dav1d and aom in WASM) and
// WebP is encoded by github.com/gen2brain/webp (libwebp translated to Go).
// When a system libavif or libwebp is installed, those two libraries load
// it instead (purego), which is faster and gives the same formats.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
)

// Formats of the sources.
const (
	JPEG = "jpeg"
	PNG  = "png"
	GIF  = "gif"
	WebP = "webp"
	BMP  = "bmp"
	TIFF = "tiff"
	AVIF = "avif"
	HEIC = "heic"
)

// EncodeFormats are the formats of the variants, the most compact first
// (domain.MediaImageFormats).
func EncodeFormats() []string { return []string{AVIF, WebP, JPEG, PNG} }

var (
	// ErrUnsupported reports a source that is not an image of a supported
	// format, or a corrupt one: another attempt would fail the same way.
	ErrUnsupported = errors.New("imaging: unsupported image")
	// ErrTooLarge reports a source above the Limits: a file too large, or
	// a picture of too many pixels (a decompression bomb).
	ErrTooLarge = errors.New("imaging: image too large")
)

// Default limits.
const (
	DefaultMaxBytes  = 256 << 20
	DefaultMaxPixels = 100_000_000
)

// Limits bound what Decode accepts; the zero value is the defaults.
type Limits struct {
	// MaxBytes bounds the size of the file (default DefaultMaxBytes).
	MaxBytes int64
	// MaxPixels bounds the pixels of the picture, read from its header
	// before any decoding (default DefaultMaxPixels: 100 megapixels, 400
	// MB decoded).
	MaxPixels int64
}

// WithDefaults returns the limits with the defaults of the unset fields.
func (l Limits) WithDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = DefaultMaxPixels
	}
	return l
}

// Detect returns the format of an image from its first bytes (32 are
// enough for every format but an ISO BMFF file listing many brands).
func Detect(data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return JPEG, nil
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return PNG, nil
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return GIF, nil
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return WebP, nil
	case bytes.HasPrefix(data, []byte("BM")) && len(data) >= 26:
		return BMP, nil
	case bytes.HasPrefix(data, []byte("II*\x00")), bytes.HasPrefix(data, []byte("MM\x00*")):
		return TIFF, nil
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		if f := isobmffFormat(data); f != "" {
			return f, nil
		}
	}
	return "", fmt.Errorf("%w: unknown format", ErrUnsupported)
}

// isobmffFormat returns the image format of the brands of an ftyp box:
// AVIF when one is an AVIF brand, HEIC when one is a HEVC brand or the
// generic HEIF brand.
func isobmffFormat(data []byte) string {
	size := int(uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3]))
	if size < 16 || size > len(data) {
		size = min(len(data), 256)
	}
	// The major brand, the minor version, then the compatible brands.
	brands := []string{string(data[8:12])}
	for i := 16; i+4 <= size; i += 4 {
		brands = append(brands, string(data[i:i+4]))
	}
	heif := false
	for _, b := range brands {
		switch b {
		case "avif", "avis":
			return AVIF
		case "heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1":
			heif = true
		}
	}
	if heif {
		return HEIC
	}
	return ""
}
