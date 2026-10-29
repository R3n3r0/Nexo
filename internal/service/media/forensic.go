package media

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// The forensic watermark (docs/proposals/video-protection.md §11): the
// media worker derives two variants of every rendition of a marked video,
// A and B, differing by an invisible pattern; the segment n of a stream
// comes from the variant bit n mod 64 of the code of its playback token
// selects (traceBit). The code derives from the session id of the token
// and the protection key: no one else computes it. At the issue of a
// token, its trace record (whom the code names) is stored in the bucket,
// encrypted: Trace finds it back from the bits read from a leaked copy.

// Labels of the keys of the forensic watermark.
const (
	traceCodeLabel   = "nexo/media/forensic-code-v1\x00"
	traceRecordLabel = "nexo/media/forensic-record-v1\x00"
)

// maxTraceScan bounds the trace records a search reads.
const maxTraceScan = 1_000_000

// traceCode is the code of the streams of a playback token.
func (s *service) traceCode(sessionID string) uint64 {
	mac := hmac.New(sha256.New, s.opts.protectionKey)
	mac.Write([]byte(traceCodeLabel + sessionID))
	return binary.BigEndian.Uint64(mac.Sum(nil)[:8])
}

// traceBit is the bit of a code segment n carries: 1 serves the B variant.
func traceBit(code uint64, n int) uint64 {
	return code >> (uint(n) % domain.MediaTraceBits) & 1 // #nosec G115 -- n is a non-negative segment number
}

// forensicViewer reports whether a request plays the forensic watermark:
// a playback token of a video its protection marks.
func forensicViewer(src Source, prot *domain.MediaProtection) bool {
	return src.Viewer != "" && prot != nil && prot.Forensic
}

// errForensicNotReady refuses the stream of a marked video before the
// media worker derived its variants: it never streams unmarked.
func errForensicNotReady(src Source) error {
	return fmt.Errorf("%w: the forensic watermark of %s/%s is not ready (the media worker derives its renditions)", domain.ErrServiceUnavailable, src.Bucket, src.Key)
}

// variantB returns the presentation of the B variant of a resolved
// rendition when it lines up with the A variant a (the same segments, the
// same init segment): nil when the rendition has none or it does not.
func (s *service) variantB(ctx context.Context, src Source, a *cached) *cached {
	if src.variantB == "" {
		return nil
	}
	b, err := s.load(ctx, Source{Bucket: src.Bucket, Key: src.variantB})
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "media: forensic variant unreadable", "bucket", src.Bucket, "key", src.variantB, "err", err)
		return nil
	}
	if !slices.EqualFunc(a.p.segments, b.p.segments, func(x, y segment) bool { return x.start == y.start && x.end == y.end }) {
		s.deps.Logger.WarnContext(ctx, "media: forensic variants do not line up", "bucket", src.Bucket, "key", src.Key)
		return nil
	}
	a.initOnce.Do(func() { a.init, a.initErr = a.p.initSegment() })
	b.initOnce.Do(func() { b.init, b.initErr = b.p.initSegment() })
	if a.initErr != nil || b.initErr != nil || !bytes.Equal(a.init, b.init) {
		s.deps.Logger.WarnContext(ctx, "media: forensic variants differ in their init segment", "bucket", src.Bucket, "key", src.Key)
		return nil
	}
	return b
}

// traceRecordKey is the key of the trace record of a token.
func traceRecordKey(code uint64, sessionID string) string {
	return fmt.Sprintf("%s%04x/%016x.%s", domain.MediaTracePrefix, code&0xffff, code, sessionID)
}

// traceAEAD is the cipher of the trace records.
func (s *service) traceAEAD() (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, s.opts.protectionKey)
	mac.Write([]byte(traceRecordLabel))
	block, err := aes.NewCipher(mac.Sum(nil)[:16])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// RecordTrace implements Service.
func (s *service) RecordTrace(ctx context.Context, t domain.MediaTrace) error {
	if len(s.opts.protectionKey) == 0 {
		return fmt.Errorf("%w: no protection key", domain.ErrInvalidArgument)
	}
	if t.SessionID == "" || strings.Trim(t.SessionID, "0123456789abcdef") != "" {
		return fmt.Errorf("%w: invalid session id %q", domain.ErrInvalidArgument, t.SessionID)
	}
	code := s.traceCode(t.SessionID)
	t.Code = fmt.Sprintf("%016x", code)
	plain, err := json.Marshal(t)
	if err != nil {
		return err
	}
	aead, err := s.traceAEAD()
	if err != nil {
		return err
	}
	key := traceRecordKey(code, t.SessionID)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plain)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	// Bound to its key: a record cannot be passed off as another.
	sealed := aead.Seal(nonce, nonce, plain, []byte(key))
	_, err = s.deps.Objects.PutObject(ctx, object.PutObjectInput{
		Bucket: t.Bucket, Key: key, Reader: bytes.NewReader(sealed), Size: int64(len(sealed)),
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("media: trace record: %w", err)
	}
	return nil
}

// Trace implements Service.
func (s *service) Trace(ctx context.Context, bucket, read string, limit int) ([]domain.MediaTraceMatch, error) {
	want, mask, err := domain.ParseMediaTraceBits(read)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	aead, err := s.traceAEAD()
	if err != nil {
		return nil, err
	}
	// The low 16 bits name the directory of the records: known, one
	// directory is read, else every record.
	prefix := domain.MediaTracePrefix
	if mask&0xffff == 0xffff {
		prefix = fmt.Sprintf("%s%04x/", domain.MediaTracePrefix, want&0xffff)
	}
	var matches []domain.MediaTraceMatch
	in := domain.ListObjectsInput{Bucket: bucket, Prefix: prefix}
	for scanned := 0; scanned < maxTraceScan; {
		page, err := s.deps.Objects.ListObjects(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Objects {
			scanned++
			name := strings.TrimPrefix(o.Key, domain.MediaTracePrefix)
			_, file, ok := strings.Cut(name, "/")
			codeHex, _, ok2 := strings.Cut(file, ".")
			if !ok || !ok2 {
				continue
			}
			code, err := strconv.ParseUint(codeHex, 16, 64)
			if err != nil {
				continue
			}
			compared := bits.OnesCount64(mask)
			agree := compared - bits.OnesCount64((code^want)&mask)
			// More than an eighth of the bits wrong: not this token.
			if (compared-agree)*8 > compared {
				continue
			}
			t, err := s.readTrace(ctx, aead, bucket, o.Key)
			if err != nil {
				s.deps.Logger.WarnContext(ctx, "media: trace record unreadable", "bucket", bucket, "key", o.Key, "err", err)
				continue
			}
			matches = append(matches, domain.MediaTraceMatch{MediaTrace: *t, Agree: agree, Compared: compared})
		}
		if !page.IsTruncated {
			break
		}
		in.ContinuationToken = page.NextContinuationToken
	}
	slices.SortFunc(matches, func(a, b domain.MediaTraceMatch) int {
		if d := (b.Agree - b.Compared) - (a.Agree - a.Compared); d != 0 {
			return d
		}
		return int(b.IssuedAt - a.IssuedAt)
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// readTrace reads and opens a trace record.
func (s *service) readTrace(ctx context.Context, aead cipher.AEAD, bucket, key string) (*domain.MediaTrace, error) {
	data, err := s.readSmall(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("record too short")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(key))
	if err != nil {
		return nil, errors.New("record not sealed by this deployment")
	}
	var t domain.MediaTrace
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
