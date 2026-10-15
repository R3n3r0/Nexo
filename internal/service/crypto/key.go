package crypto

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Domain separation strings of the keys derived from an object key.
const (
	// partKeyLabel derives the key of a part from its number alone: the
	// parts written before part salts existed, and the single part of a
	// single-request upload, whose object key is fresh.
	partKeyLabel = "nexo-sse-part-key-v1"
	// partKeySaltedLabel derives the key of a part from its number and
	// the random salt of its upload (PartSaltSize bytes).
	partKeySaltedLabel = "nexo-sse-part-key-v2"
	etagLabel          = "nexo-sse-etag-v1"
	// sealedValueLabel derives the key of the sealed metadata values.
	sealedValueLabel = "nexo-sse-sealed-value-v1"
)

// PartSaltSize is the size of the random salt of a part upload.
const PartSaltSize = 32

// sealedValuePrefix starts a sealed metadata value (SealValue): no clear
// checksum or digest contains a colon.
const sealedValuePrefix = "sse1:"

// NewPartSalt returns a fresh random salt for the key of one upload of a
// part (ObjectKey.EncryptPart). The parts of a multipart upload share
// their object key and a part number can be uploaded again (retries,
// concurrent uploads, UploadPartCopy over UploadPart): the salt makes
// every upload encrypt under its own key, so that a keystream is never
// reused, whatever the part number.
func NewPartSalt() ([]byte, error) {
	salt := make([]byte, PartSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("crypto: part salt: %w", err)
	}
	return salt, nil
}

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

// String implements fmt.Stringer without the key: the encryption type and
// the cipher only, so that an object key printed or logged by mistake
// discloses nothing.
func (k ObjectKey) String() string {
	return "ObjectKey{Type:" + string(k.info.Type) + " Cipher:" + string(k.cipher) + "}"
}

// GoString implements fmt.GoStringer (%#v) without the key.
func (k ObjectKey) GoString() string { return k.String() }

// Format implements fmt.Formatter: every verb (%v, %+v, %#v, %x...) prints
// the redacted form (%+v would otherwise print the unexported key).
func (k ObjectKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue implements slog.LogValuer without the key.
func (k ObjectKey) LogValue() slog.Value {
	return slog.GroupValue(slog.String("type", string(k.info.Type)), slog.String("cipher", string(k.cipher)))
}

// MarshalJSON keeps the key out of any JSON form (a log record, an audit
// entry): the encryption type and the cipher only.
func (k ObjectKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type   string `json:"type"`
		Cipher string `json:"cipher"`
	}{string(k.info.Type), string(k.cipher)})
}

// partKey derives the data key of part n (1-based) uploaded with salt
// (nil: the unsalted derivation of the single part of a single-request
// upload and of the parts written before part salts existed).
func (k ObjectKey) partKey(n int, salt []byte) ([]byte, error) {
	// The number is derived on 32 bits: a larger one would share the key
	// of a smaller one.
	if n < 1 || n > math.MaxUint32 {
		return nil, fmt.Errorf("%w: crypto: part number %d", domain.ErrInvalidArgument, n)
	}
	if salt != nil && len(salt) != PartSaltSize {
		return nil, fmt.Errorf("%w: crypto: part salt of %d bytes", domain.ErrDecryption, len(salt))
	}
	mac := hmac.New(sha256.New, k.key[:])
	label := partKeyLabel
	if salt != nil {
		label = partKeySaltedLabel
	}
	_, _ = mac.Write([]byte(label))
	var num [4]byte
	binary.BigEndian.PutUint32(num[:], uint32(n)) // #nosec G115 -- 1 <= n <= math.MaxUint32 is checked above
	_, _ = mac.Write(num[:])
	_, _ = mac.Write(salt)
	return mac.Sum(nil), nil
}

// EncryptPart returns the encrypted stream of part n (1-based; a
// single-request upload is part 1) of the plaintext r, under the key of
// the part and its salt: a fresh NewPartSalt for every upload of a part of
// a multipart upload, nil only for the single part of a single-request
// upload (its object key is fresh). The stream ends after the final
// package, when r is exhausted: its length is EncryptedSize of the
// plaintext length.
func (k ObjectKey) EncryptPart(n int, salt []byte, r io.Reader) (io.Reader, error) {
	key, err := k.partKey(n, salt)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(k.cipher, key)
	if err != nil {
		return nil, err
	}
	return newEncReader(r, aead), nil
}

// DecryptPart returns the plaintext of the range rng (PackageRange) of
// part n encrypted with salt (EncryptPart), read from r, the stored bytes
// [rng.StoredOffset, +rng.StoredLength) of the part. Packages that do not
// authenticate fail the read with domain.ErrDecryption.
func (k ObjectKey) DecryptPart(n int, salt []byte, r io.Reader, rng Range) (io.Reader, error) {
	key, err := k.partKey(n, salt)
	if err != nil {
		return nil, err
	}
	aead, err := newAEAD(k.cipher, key)
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

// IsSealedValue reports a metadata value sealed with SealValue.
func IsSealedValue(v string) bool { return strings.HasPrefix(v, sealedValuePrefix) }

// valueAEAD returns the AEAD of the sealed metadata values.
func (k ObjectKey) valueAEAD() (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, k.key[:])
	_, _ = mac.Write([]byte(sealedValueLabel))
	return newAEAD(k.cipher, mac.Sum(nil))
}

// SealValue encrypts a short metadata value of the object, bound to its
// name: the checksums of the plaintext, which would otherwise confirm
// guesses of the content without the key (a CRC32 even reveals a content
// of up to 4 bytes). The result (a random nonce, the ciphertext and its
// tag, prefixed with "sse1:") is opened by OpenValue with the same object
// key, so only a request able to unseal the object key reads the value.
func (k ObjectKey) SealValue(name, value string) (string, error) {
	aead, err := k.valueAEAD()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(value)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: seal %s: %w", name, err)
	}
	out := aead.Seal(nonce, nonce, []byte(value), []byte(sealedValueLabel+"/"+name))
	return sealedValuePrefix + base64.RawStdEncoding.EncodeToString(out), nil
}

// OpenValue decrypts a value sealed by SealValue under the same name; a
// value that is not sealed is returned as it is (versions written before
// the values were sealed). A damaged value or another key fails with
// domain.ErrDecryption.
func (k ObjectKey) OpenValue(name, sealed string) (string, error) {
	if !IsSealedValue(sealed) {
		return sealed, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, sealedValuePrefix))
	if err != nil {
		return "", fmt.Errorf("crypto: %w: sealed %s: %w", domain.ErrDecryption, name, err)
	}
	aead, err := k.valueAEAD()
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", fmt.Errorf("crypto: %w: sealed %s is truncated", domain.ErrDecryption, name)
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(sealedValueLabel+"/"+name))
	if err != nil {
		return "", fmt.Errorf("crypto: %w: sealed %s: %w", domain.ErrDecryption, name, err)
	}
	return string(plain), nil
}
