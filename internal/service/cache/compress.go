package cache

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// compress encodes body with an encoding of the compressed variants.
func compress(enc string, body []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(body) / 2)
	switch enc {
	case EncodingGzip:
		w, err := gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case EncodingBrotli:
		w := brotli.NewWriterLevel(&buf, 5)
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	case EncodingZstd:
		w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: encoding %q", ErrInvalid, enc)
	}
	return buf.Bytes(), nil
}

// negotiate picks the first encoding of the policy the client accepts
// (Accept-Encoding with its q-values, RFC 9110 §12.5.3); "" for none.
func negotiate(accept string, algorithms []string) string {
	if strings.TrimSpace(accept) == "" {
		return ""
	}
	q := map[string]float64{}
	star, hasStar := 0.0, false
	for _, part := range strings.Split(accept, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		weight := 1.0
		for _, p := range strings.Split(params, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					weight = f
				}
			}
		}
		if name == "*" {
			star, hasStar = weight, true
			continue
		}
		q[name] = weight
	}
	for _, a := range algorithms {
		w, ok := q[a]
		if !ok && hasStar {
			w, ok = star, true
		}
		if ok && w > 0 {
			return a
		}
	}
	return ""
}

// variantETag is the ETag of a compressed variant: the one of the object
// with the encoding.
func variantETag(etag, enc string) string { return etag + "-" + enc }
