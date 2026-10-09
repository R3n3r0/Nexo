package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/sys/cpu"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Package format of the encrypted data of a part:
//
//	package i = AEAD(part key, nonce(i), plaintext[i*PackageSize : (i+1)*PackageSize], aad(final))
//
// Every package but the last holds PackageSize bytes of plaintext and is
// PackageSize+TagSize bytes long; the last one holds the rest (possibly
// nothing: an empty part is one empty final package) and is marked final
// in its additional data. The nonce is the big-endian sequence number of
// the package: the part key is unique (object key, part number and the
// random salt of the upload of the part, see NewPartSalt), so a nonce is
// never reused under a key.
const (
	// PackageSize is the plaintext size of a full package (64 KiB).
	PackageSize = 64 << 10
	// TagSize is the authentication tag of a package.
	TagSize = 16
	// packageStored is the stored size of a full package.
	packageStored = PackageSize + TagSize
)

// Cipher is the AEAD of the data packages, recorded with the object.
type Cipher string

// Data ciphers.
const (
	CipherAESGCM           Cipher = "AES-256-GCM"
	CipherChaCha20Poly1305 Cipher = "CHACHA20-POLY1305"
)

// DefaultCipher returns the cipher of new objects: AES-256-GCM on a CPU
// with AES instructions, ChaCha20-Poly1305 otherwise (faster in software,
// constant time).
func DefaultCipher() Cipher {
	switch runtime.GOARCH {
	case "amd64", "386":
		if cpu.X86.HasAES && cpu.X86.HasPCLMULQDQ {
			return CipherAESGCM
		}
	case "arm64":
		if cpu.ARM64.HasAES && cpu.ARM64.HasPMULL {
			return CipherAESGCM
		}
	case "s390x":
		if cpu.S390X.HasAES && cpu.S390X.HasAESGCM {
			return CipherAESGCM
		}
	case "ppc64le":
		return CipherAESGCM
	}
	return CipherChaCha20Poly1305
}

// newAEAD returns the AEAD of a cipher keyed with a 32-byte key.
func newAEAD(c Cipher, key []byte) (cipher.AEAD, error) {
	switch c {
	case CipherAESGCM:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("crypto: %w", err)
		}
		return cipher.NewGCM(block)
	case CipherChaCha20Poly1305:
		return chacha20poly1305.New(key)
	default:
		return nil, fmt.Errorf("crypto: %w: unknown cipher %q", domain.ErrDecryption, c)
	}
}

// EncryptedSize returns the stored size of a part of plain bytes (-1 for
// an unknown size).
func EncryptedSize(plain int64) int64 {
	if plain < 0 {
		return -1
	}
	if plain == 0 {
		return TagSize
	}
	packages := (plain + PackageSize - 1) / PackageSize
	return plain + packages*TagSize
}

// DecryptedSize returns the plaintext size of a stored part of enc bytes;
// an impossible size is domain.ErrDecryption.
func DecryptedSize(enc int64) (int64, error) {
	if enc < TagSize {
		return 0, fmt.Errorf("crypto: %w: encrypted size %d", domain.ErrDecryption, enc)
	}
	full, rest := enc/packageStored, enc%packageStored
	switch {
	case rest == 0:
		return full * PackageSize, nil
	case rest < TagSize, rest == TagSize && full > 0:
		return 0, fmt.Errorf("crypto: %w: encrypted size %d", domain.ErrDecryption, enc)
	default:
		return full*PackageSize + rest - TagSize, nil
	}
}

// lastSeq returns the sequence number of the last package of a part of
// plain bytes.
func lastSeq(plain int64) uint64 {
	if plain <= 0 {
		return 0
	}
	return uint64((plain - 1) / PackageSize)
}

// nonce returns the nonce of package seq.
func nonce(aead cipher.AEAD, seq uint64) []byte {
	n := make([]byte, aead.NonceSize())
	binary.BigEndian.PutUint64(n[len(n)-8:], seq)
	return n
}

// additional data of the packages: the final flag.
func aad(final bool) []byte {
	if final {
		return []byte{1}
	}
	return []byte{0}
}

// encReader encrypts a plaintext stream package by package. It reads one
// byte ahead to know whether a full package is the last one, so that the
// stream needs no declared size.
type encReader struct {
	src     io.Reader
	aead    cipher.AEAD
	seq     uint64
	plain   []byte
	ahead   []byte
	out     []byte
	pending []byte
	done    bool
	// srcEOF records that the source reached its end along with the byte
	// read ahead: the next package is the final one.
	srcEOF bool
	err    error
}

// readFull reads into buf until it is full or src returns an error. Unlike
// io.ReadFull it returns the error of src unchanged: io.EOF (with the
// bytes read before it, possibly a full buffer) is the end of the stream,
// any other error, io.ErrUnexpectedEOF included, a failure of the source.
// io.ReadFull turns an end after a partial read into io.ErrUnexpectedEOF,
// which a source cut short returns too: the two must not be confused.
func readFull(src io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := src.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func newEncReader(src io.Reader, aead cipher.AEAD) *encReader {
	return &encReader{
		src:   src,
		aead:  aead,
		plain: make([]byte, 0, PackageSize),
		ahead: make([]byte, 0, 1),
		out:   make([]byte, 0, packageStored),
	}
}

// Read implements io.Reader.
func (e *encReader) Read(p []byte) (int, error) {
	for len(e.pending) == 0 {
		if e.err != nil {
			return 0, e.err
		}
		if e.done {
			return 0, io.EOF
		}
		e.err = e.next()
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	return n, nil
}

// next seals the next package into pending. Only the end of the source
// (a plain io.EOF) makes a package the final one: any other error of the
// source, io.ErrUnexpectedEOF included (a body cut short), fails the
// stream, so that a truncated plaintext is never sealed as a complete
// one.
func (e *encReader) next() error {
	e.plain = append(e.plain[:0], e.ahead...)
	e.ahead = e.ahead[:0]
	final := e.srcEOF
	if !final {
		n, err := readFull(e.src, e.plain[len(e.plain):PackageSize])
		e.plain = e.plain[:len(e.plain)+n]
		switch {
		case err == io.EOF:
			final = true
		case err != nil:
			return err
		default:
			// A full package: it is the last one when nothing follows.
			var one [1]byte
			m, err := readFull(e.src, one[:])
			switch {
			case err != nil && err != io.EOF:
				return err
			case m == 1:
				e.ahead = append(e.ahead, one[0])
				e.srcEOF = err == io.EOF
			default:
				final = true
			}
		}
	}
	e.out = e.aead.Seal(e.out[:0], nonce(e.aead, e.seq), e.plain, aad(final))
	e.pending = e.out
	e.seq++
	e.done = final
	return nil
}

// Range is the part of a stored (encrypted) part that serves a plaintext
// range: read StoredLength bytes from StoredOffset and decrypt them with
// ObjectKey.DecryptPart.
type Range struct {
	StoredOffset int64
	StoredLength int64
	// first is the sequence number of the first package read, last the
	// one of the last package of the part, lastPlain the plaintext size
	// of that package.
	first     uint64
	last      uint64
	lastPlain int64
	// skip is the plaintext to drop from the first package, length the
	// plaintext to return.
	skip   int64
	length int64
}

// PackageRange returns the packages of a part of plainSize bytes that hold
// the plaintext range [offset, offset+length).
func PackageRange(plainSize, offset, length int64) (Range, error) {
	if plainSize < 0 || offset < 0 || length < 0 || offset+length > plainSize {
		return Range{}, fmt.Errorf("%w: range [%d,+%d) of a part of %d bytes", domain.ErrInvalidRange, offset, length, plainSize)
	}
	last := lastSeq(plainSize)
	lastPlain := plainSize - int64(last)*PackageSize
	r := Range{last: last, lastPlain: lastPlain, length: length}
	if length == 0 {
		return r, nil
	}
	r.first = uint64(offset / PackageSize)
	end := uint64((offset + length - 1) / PackageSize)
	r.skip = offset - int64(r.first)*PackageSize
	r.StoredOffset = int64(r.first) * packageStored
	storedEnd := int64(end+1) * packageStored
	if end == last {
		storedEnd = int64(last)*packageStored + lastPlain + TagSize
	}
	r.StoredLength = storedEnd - r.StoredOffset
	return r, nil
}

// decReader decrypts the packages of a Range.
type decReader struct {
	src     io.Reader
	aead    cipher.AEAD
	rng     Range
	seq     uint64
	buf     []byte
	pending []byte
	skip    int64
	left    int64
	err     error
}

// Read implements io.Reader.
func (d *decReader) Read(p []byte) (int, error) {
	if d.left <= 0 {
		return 0, io.EOF
	}
	for len(d.pending) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		d.err = d.next()
	}
	if int64(len(p)) > d.left {
		p = p[:d.left]
	}
	n := copy(p, d.pending)
	d.pending = d.pending[n:]
	d.left -= int64(n)
	return n, nil
}

// next reads and opens the next package.
func (d *decReader) next() error {
	final := d.seq == d.rng.last
	size := int64(packageStored)
	if final {
		size = d.rng.lastPlain + TagSize
	}
	if d.seq > d.rng.last {
		return fmt.Errorf("crypto: %w: read beyond the last package", domain.ErrDecryption)
	}
	buf := d.buf[:size]
	if _, err := io.ReadFull(d.src, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("crypto: %w: package %d truncated", domain.ErrDecryption, d.seq)
		}
		return err
	}
	plain, err := d.aead.Open(buf[:0], nonce(d.aead, d.seq), buf, aad(final))
	if err != nil {
		return fmt.Errorf("crypto: %w: package %d does not authenticate", domain.ErrDecryption, d.seq)
	}
	d.seq++
	if d.skip > 0 {
		if d.skip > int64(len(plain)) {
			return fmt.Errorf("crypto: %w: offset beyond package %d", domain.ErrDecryption, d.seq-1)
		}
		plain = plain[d.skip:]
		d.skip = 0
	}
	d.pending = plain
	return nil
}
