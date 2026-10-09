package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Server-side encryption in the object layer (docs/02 S-OBJ-13). The data
// is encrypted by the crypto package before it reaches the store: the
// erasure coding and the bitrot hashes cover the ciphertext, the drives
// never see the plaintext. The stored geometry of an encrypted version is
// the one of its ciphertext (FileInfo.Size, PartInfo.Size) while the
// object API speaks plaintext: PartInfo.ActualSize and the actual size of
// the version (metaActualSize) are the plaintext sizes, which objectInfo,
// the listings, the ranges and the quota use. The checksums, Content-MD5
// and the ETag of SSE-S3 are computed over the plaintext; SSE-C and SSE-KMS
// versions have opaque ETags (crypto.ObjectKey.ETag).
//
// Every part is encrypted with the key of its number in the upload: a
// completion renumbers the parts it selects, so the encryption number of
// a part of a multipart version is recorded in PartInfo.Index.

// partIndexVersion starts the encryption index of a part.
const partIndexVersion = 1

// encodePartIndex returns the encryption index of a part encrypted as
// part n of its upload.
func encodePartIndex(n int) []byte {
	out := []byte{partIndexVersion}
	return binary.AppendUvarint(out, uint64(n))
}

// encryptionPart returns the number a part (the i-th of the version,
// 0-based) was encrypted as: the number recorded in its index, or its
// position.
func encryptionPart(p storage.PartInfo, i int) int {
	if len(p.Index) > 1 && p.Index[0] == partIndexVersion {
		if n, k := binary.Uvarint(p.Index[1:]); k > 0 && n > 0 && n <= maxPartNumber {
			return int(n)
		}
	}
	return i + 1
}

// errSSENotConfigured is a request for encryption on a layer built
// without the crypto service.
func errSSENotConfigured() error {
	return domain.NewS3Error(domain.ErrNotImplemented, "NotImplemented", "Server side encryption is not configured on this server")
}

// effectiveSSE returns the encryption of a new version: the request's, or
// the default encryption of the bucket when the request asks for none.
func effectiveSSE(cfg BucketConfig, req domain.SSERequest) domain.SSERequest {
	if req.IsZero() {
		return cfg.Encryption.Request()
	}
	return req
}

// sealFor creates the object key of a new encrypted version (or upload)
// of bucket/key, nil when the version is not encrypted.
func (l *layer) sealFor(ctx context.Context, cfg BucketConfig, bucket, key string, req domain.SSERequest) (*crypto.ObjectKey, error) {
	req = effectiveSSE(cfg, req)
	if req.IsZero() {
		return nil, nil
	}
	if l.deps.Crypto == nil {
		return nil, errSSENotConfigured()
	}
	k, err := l.deps.Crypto.Seal(ctx, crypto.SealInput{Bucket: bucket, Object: key, Request: req})
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// unsealFor recovers the object key of a version (or upload) from its
// metadata, nil for an unencrypted one. The SSE-C key of the request must
// be the one of an SSE-C object, and only of one.
func (l *layer) unsealFor(ctx context.Context, bucket, key string, meta map[string]string, ckey domain.SSECustomerKey) (*crypto.ObjectKey, error) {
	if !crypto.IsEncrypted(meta) {
		if !ckey.IsZero() {
			return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
				"The encryption parameters are not applicable to this object.")
		}
		return nil, nil
	}
	if l.deps.Crypto == nil {
		return nil, errSSENotConfigured()
	}
	k, err := l.deps.Crypto.Unseal(ctx, crypto.UnsealInput{Bucket: bucket, Object: key, Metadata: meta, CustomerKey: ckey})
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// checkCustomerKey verifies the SSE-C key of a request that does not read
// the data (HEAD, attributes): an SSE-C version needs its key, the others
// refuse one. Only SSE-C keys are checked: SSE-S3 and SSE-KMS versions
// need no KMS call to be described.
func (l *layer) checkCustomerKey(ctx context.Context, bucket, key string, fi storage.FileInfo, ckey domain.SSECustomerKey) (domain.EncryptionInfo, error) {
	info := crypto.InfoOf(fi.Metadata)
	if info.Type != domain.SSEC && ckey.IsZero() {
		return info, nil
	}
	k, err := l.unsealFor(ctx, bucket, key, fi.Metadata, ckey)
	if err != nil || k == nil {
		return info, err
	}
	return k.Info(), nil
}

// setActualSize records the plaintext size of an encrypted version.
func setActualSize(meta map[string]string, size int64) {
	meta[metaActualSize] = strconv.FormatInt(size, 10)
}

// actualSize returns the plaintext size of a version: the recorded size
// of an encrypted one, the stored size otherwise.
func actualSize(fi storage.FileInfo) int64 {
	if v, ok := fi.Metadata[metaActualSize]; ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return fi.Size
}

// partPlainSize returns the plaintext size of a part of a version.
func partPlainSize(fi storage.FileInfo, p storage.PartInfo) int64 {
	if crypto.IsEncrypted(fi.Metadata) {
		return p.ActualSize
	}
	if p.ActualSize > 0 {
		return p.ActualSize
	}
	return p.Size
}

// encryptedReader reads a plaintext range of an encrypted version: it
// decrypts the packages of every part the range crosses, one part at a
// time, reading from the store only the stored packages it needs.
type encryptedReader struct {
	ctx       context.Context
	l         *layer
	st        store
	bucket    string
	key       string
	fi        storage.FileInfo
	okey      *crypto.ObjectKey
	offset    int64
	remaining int64
	cur       io.Reader
	closer    io.Closer
}

// openEncrypted returns a reader over the plaintext [offset,
// offset+length) of an encrypted version.
func (l *layer) openEncrypted(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, okey *crypto.ObjectKey, offset, length int64) io.ReadCloser {
	return &encryptedReader{ctx: ctx, l: l, st: st, bucket: bucket, key: key, fi: fi, okey: okey, offset: offset, remaining: length}
}

func (r *encryptedReader) Read(p []byte) (int, error) {
	for {
		if r.remaining <= 0 {
			return 0, io.EOF
		}
		if r.cur == nil {
			if err := r.openNext(); err != nil {
				return 0, err
			}
		}
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.cur.Read(p)
		r.offset += int64(n)
		r.remaining -= int64(n)
		if errors.Is(err, io.EOF) {
			r.closeCur()
			if n == 0 {
				continue
			}
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("object: read %s/%s: %w", r.bucket, r.key, lockErr(r.ctx, err))
		}
		return n, nil
	}
}

// openNext opens the part holding the current offset, from the offset to
// the end of the range or of the part.
func (r *encryptedReader) openNext() error {
	var start int64
	for i, part := range r.fi.Parts {
		plain := part.ActualSize
		if r.offset >= start+plain {
			start += plain
			continue
		}
		partOff := r.offset - start
		partLen := min(plain-partOff, r.remaining)
		rng, err := crypto.PackageRange(plain, partOff, partLen)
		if err != nil {
			return fmt.Errorf("object: %s/%s: %w", r.bucket, r.key, err)
		}
		if rng.StoredOffset+rng.StoredLength > part.Size {
			return fmt.Errorf("object: %s/%s: %w: part %d is shorter than its packages", r.bucket, r.key, domain.ErrDecryption, part.Number)
		}
		var raw io.Reader
		if r.fi.DataDir == "" {
			if rng.StoredOffset+rng.StoredLength > int64(len(r.fi.Data)) {
				return fmt.Errorf("%w: inline data of %d bytes, want [%d,+%d)", domain.ErrFaultyDisk, len(r.fi.Data), rng.StoredOffset, rng.StoredLength)
			}
			raw = bytes.NewReader(r.fi.Data[rng.StoredOffset : rng.StoredOffset+rng.StoredLength])
		} else {
			rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, part, rng.StoredOffset, rng.StoredLength)
			if err != nil {
				return fmt.Errorf("object: open part %d of %s/%s: %w", part.Number, r.bucket, r.key, lockErr(r.ctx, err))
			}
			raw, r.closer = rc, rc
		}
		dec, err := r.okey.DecryptPart(encryptionPart(part, i), raw, rng)
		if err != nil {
			r.closeCur()
			return err
		}
		r.cur = dec
		return nil
	}
	return fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrFaultyDisk, r.offset, r.bucket, r.key)
}

func (r *encryptedReader) closeCur() {
	if r.closer != nil {
		_ = r.closer.Close()
	}
	r.cur, r.closer = nil, nil
}

func (r *encryptedReader) Close() error {
	r.closeCur()
	return nil
}
