package crypto

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"maps"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Domain separation strings of the keys derived from an object key.
const (
	partKeyLabel = "nexo-sse-part-key-v1"
	etagLabel    = "nexo-sse-etag-v1"
)

// Info describes the encryption of the object, as the S3 responses report
// it (the SSE-C key MD5 is the one of the request).
func (k ObjectKey) Info() domain.EncryptionInfo {
	info := k.info
	if len(info.Context) > 0 {
		info.Context = maps.Clone(info.Context)
	}
	return info
}

// Metadata returns the metadata describing the sealed key, to store with
// the version or the multipart upload.
func (k ObjectKey) Metadata() map[string]string { return maps.Clone(k.meta) }

// Cipher returns the cipher of the data packages.
func (k ObjectKey) Cipher() Cipher { return k.cipher }

// partKey derives the data key of part n (1-based).
func (k ObjectKey) partKey(n int) []byte {
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write([]byte(partKeyLabel))
	var num [4]byte
	binary.BigEndian.PutUint32(num[:], uint32(n))
	_, _ = mac.Write(num[:])
	return mac.Sum(nil)
}

// EncryptPart returns the encrypted stream of part n (1-based; a
// single-request upload is part 1) of the plaintext r. The stream ends
// after the final package, when r is exhausted: its length is
// EncryptedSize of the plaintext length.
func (k ObjectKey) EncryptPart(n int, r io.Reader) (io.Reader, error) {
	if n < 1 {
		return nil, fmt.Errorf("%w: crypto: part number %d", domain.ErrInvalidArgument, n)
	}
	aead, err := newAEAD(k.cipher, k.partKey(n))
	if err != nil {
		return nil, err
	}
	return newEncReader(r, aead), nil
}

// DecryptPart returns the plaintext of the range rng (PackageRange) of
// part n read from r, the stored bytes [rng.StoredOffset,
// +rng.StoredLength) of the part. Packages that do not authenticate fail
// the read with domain.ErrDecryption.
func (k ObjectKey) DecryptPart(n int, r io.Reader, rng Range) (io.Reader, error) {
	if n < 1 {
		return nil, fmt.Errorf("%w: crypto: part number %d", domain.ErrInvalidArgument, n)
	}
	aead, err := newAEAD(k.cipher, k.partKey(n))
	if err != nil {
		return nil, err
	}
	return &decReader{
		src:  r,
		aead: aead,
		rng:  rng,
		seq:  rng.first,
		buf:  make([]byte, packageStored),
		skip: rng.skip,
		left: rng.length,
	}, nil
}

// ETag returns the ETag of encrypted content whose plaintext MD5 is
// md5Hex: the MD5 itself for SSE-S3 (as S3), an opaque value of the same
// form for SSE-C and SSE-KMS, which never reveal a digest of the
// plaintext (a keyed hash of it under the object key).
func (k ObjectKey) ETag(md5Hex string) string {
	if k.info.Type == domain.SSES3 {
		return md5Hex
	}
	raw, err := hex.DecodeString(md5Hex)
	if err != nil {
		raw = []byte(md5Hex)
	}
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write([]byte(etagLabel))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)[:16])
}
