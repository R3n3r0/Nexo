package media

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// KeyPeriod is the number of segments encrypted with one key of a
// protected video: about a minute of video with the default segments.
const KeyPeriod = 10

// keyLabel separates the keys of the segments from the other uses of the
// media protection key.
const keyLabel = "nexo/media/aes128\x00"

// Protections returns the download protection of the videos of a bucket
// (implemented by the bucket service): a disabled protection when the
// bucket has none.
type Protections interface {
	GetMediaProtection(ctx context.Context, bucket string) (domain.MediaProtection, error)
}

// DeriveProtectionKey derives the media protection key of a deployment
// from its root credentials and its id: every node derives the same key,
// no other deployment does, and changing the root credentials replaces
// every segment key and every playback token at once.
func DeriveProtectionKey(rootUser, rootPassword, deploymentID string) []byte {
	mac := hmac.New(sha256.New, []byte(rootPassword))
	mac.Write([]byte("nexo-media-protection-v1\x00" + rootUser + "\x00" + deploymentID))
	return mac.Sum(nil)
}

// Protection returns the protection of the video src names, nil when the
// video is not protected (a bucket without protection, a key outside its
// prefix, or a service without protection key).
func (s *service) Protection(ctx context.Context, src Source) (*domain.MediaProtection, error) {
	if s.deps.Protections == nil || len(s.opts.protectionKey) == 0 {
		return nil, nil
	}
	p, err := s.deps.Protections.GetMediaProtection(ctx, src.Bucket)
	if err != nil {
		return nil, err
	}
	// A rendition (resolved or not) is protected with its video.
	if !p.Protects(src.Key) {
		return nil, nil
	}
	return &p, nil
}

// periods returns the number of key periods of n segments.
func periods(n int) int { return (n + KeyPeriod - 1) / KeyPeriod }

// segmentKey derives the key of a period of a presentation: it names the
// object (bucket, key, version, ETag), so that overwriting a video or
// another rendition never reuses a key.
func (s *service) segmentKey(bucket, key string, info presentationID, period int) []byte {
	mac := hmac.New(sha256.New, s.opts.protectionKey)
	mac.Write([]byte(keyLabel + bucket + "\x00" + key + "\x00" + info.versionID + "\x00" + info.etag + "\x00" + strconv.Itoa(period)))
	return mac.Sum(nil)[:16]
}

// presentationID names the content of a presentation.
type presentationID struct{ versionID, etag string }

func (p *presentation) id() presentationID {
	return presentationID{versionID: p.info.VersionID, etag: p.info.ETag}
}

// segmentIV is the IV of segment n: its media sequence number as a
// 128-bit big-endian integer (RFC 8216 §5.2, a key tag without IV).
func segmentIV(n int) []byte {
	iv := make([]byte, aes.BlockSize)
	binary.BigEndian.PutUint64(iv[8:], uint64(n)) // #nosec G115 -- n is a non-negative segment number
	return iv
}

// encryptSegment encrypts a media segment with AES-128-CBC and PKCS#7
// padding (HLS METHOD=AES-128).
func encryptSegment(key, iv, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("media: segment key: %w", err)
	}
	pad := aes.BlockSize - len(data)%aes.BlockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, out)
	return out, nil
}

// Key implements Service.
func (s *service) Key(ctx context.Context, src Source, period int) ([]byte, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	prot, err := s.Protection(ctx, src)
	if err != nil {
		return nil, err
	}
	if prot == nil {
		return nil, fmt.Errorf("%w: %s/%s is not a protected video", domain.ErrNotFound, src.Bucket, src.Key)
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	if period < 0 || period >= periods(len(c.p.segments)) {
		return nil, fmt.Errorf("%w: key %d of %s/%s (%d keys)", domain.ErrNotFound, period, src.Bucket, src.Key, periods(len(c.p.segments)))
	}
	return s.segmentKey(src.Bucket, src.Key, c.p.id(), period), nil
}

// Periods implements Service.
func (s *service) Periods(ctx context.Context, src Source) (int, error) {
	src, err := s.resolve(ctx, src)
	if err != nil {
		return 0, err
	}
	c, err := s.load(ctx, src)
	if err != nil {
		return 0, err
	}
	return periods(len(c.p.segments)), nil
}

// PeriodLength returns the duration of a key period of a presentation:
// that of its KeyPeriod segments (the last period may be shorter).
func PeriodLength(p *Presentation, period int) time.Duration {
	var d time.Duration
	for i := period * KeyPeriod; i >= 0 && i < len(p.Segments) && i < (period+1)*KeyPeriod; i++ {
		d += p.Segments[i]
	}
	return d
}
