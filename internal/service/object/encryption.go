package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"

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
// Every part of a multipart upload is encrypted with the key of its number
// in the upload and of the random salt of that upload of the part
// (crypto.NewPartSalt: a part number uploaded twice never reuses a
// keystream). A completion renumbers the parts it selects, so the
// encryption number and the salt of a part of a multipart version are
// recorded in PartInfo.Index.
//
// The checksums of the plaintext of an encrypted version (and of its
// parts) are stored sealed under its object key (crypto.ObjectKey.
// SealValue). With the opaque ETags of SSE-C and SSE-KMS, the drives
// never hold a digest of their plaintext; the ETag of an SSE-S3 version
// and of its parts is the MD5 of the plaintext, as in S3, stored in clear
// like every ETag (docs/07 §7.2). The
// algorithm and the checksum type stay readable (listings report them);
// the value is opened with the object key (openChecksums), by the reads
// that unseal it anyway (GET, SSE-C requests) or that need the value
// (HEAD with the checksum mode, GetObjectAttributes, ListParts,
// CompleteMultipartUpload).

// Versions of the encryption index of a part.
const (
	// partIndexNumber is the part number alone (parts written before the
	// part salts existed).
	partIndexNumber = 1
	// partIndexSalted is the part number followed by the part salt.
	partIndexSalted = 2
)

// encodePartIndex returns the encryption index of a part encrypted as
// part n of its upload with salt (nil for the parts without salt).
func encodePartIndex(n int, salt []byte) []byte {
	if salt == nil {
		return binary.AppendUvarint([]byte{partIndexNumber}, uint64(n)) // #nosec G115 -- a part number, 1 to 10000
	}
	out := binary.AppendUvarint([]byte{partIndexSalted}, uint64(n)) // #nosec G115 -- a part number, 1 to 10000
	return append(out, salt...)
}

// encryptionPart returns the number and the salt a part (the i-th of the
// version, 0-based) was encrypted with: the ones recorded in its index,
// or its position without salt (the single part of a single-request
// upload).
func encryptionPart(p storage.PartInfo, i int) (int, []byte) {
	if len(p.Index) > 1 && (p.Index[0] == partIndexNumber || p.Index[0] == partIndexSalted) {
		if n, k := binary.Uvarint(p.Index[1:]); k > 0 && n > 0 && n <= maxPartNumber {
			if p.Index[0] == partIndexNumber {
				return int(n), nil
			}
			if salt := p.Index[1+k:]; len(salt) == crypto.PartSaltSize {
				return int(n), salt
			}
		}
	}
	return i + 1, nil
}

// metaSealedChecksumPrefix starts the key of the sealed checksum of the
// plaintext of an encrypted version (followed by the lower-case
// algorithm), which replaces x-amz-checksum-<algorithm>. It is internal.
const metaSealedChecksumPrefix = "x-nexo-internal-sealed-checksum-"

// Names the sealed checksums are bound to (crypto.ObjectKey.SealValue).
func objectChecksumName(algo string) string { return "checksum/" + strings.ToLower(algo) }

func partChecksumName(algo string) string { return "part-checksum/" + strings.ToLower(algo) }

// sealPartChecksum returns c with its value sealed under okey (c itself
// for an unencrypted part or no checksum).
func sealPartChecksum(okey *crypto.ObjectKey, c domain.Checksum) (domain.Checksum, error) {
	if okey == nil || c.Value == "" || crypto.IsSealedValue(c.Value) {
		return c, nil
	}
	sealed, err := okey.SealValue(partChecksumName(string(c.Algorithm)), c.Value)
	if err != nil {
		return domain.Checksum{}, err
	}
	c.Value = sealed
	return c, nil
}

// openPartChecksum returns c with its value opened: with okey nil, a
// sealed value is dropped (the algorithm stays).
func openPartChecksum(okey *crypto.ObjectKey, c domain.Checksum) (domain.Checksum, error) {
	if !crypto.IsSealedValue(c.Value) {
		return c, nil
	}
	if okey == nil {
		c.Value = ""
		return c, nil
	}
	v, err := okey.OpenValue(partChecksumName(string(c.Algorithm)), c.Value)
	if err != nil {
		return domain.Checksum{}, err
	}
	c.Value = v
	return c, nil
}

// sealChecksums seals, in place, the checksums of the plaintext of a new
// encrypted version: the checksum of the object moves from
// x-amz-checksum-<algorithm> to metaSealedChecksumPrefix, those of the
// parts are sealed in their maps (the parts of a completed upload are
// already).
func sealChecksums(okey *crypto.ObjectKey, fi *storage.FileInfo) error {
	if okey == nil {
		return nil
	}
	var algos []string
	for k := range fi.Metadata {
		if k != metaChecksumType && strings.HasPrefix(k, metaChecksumPrefix) {
			algos = append(algos, strings.TrimPrefix(k, metaChecksumPrefix))
		}
	}
	for _, algo := range algos {
		sealed, err := okey.SealValue(objectChecksumName(algo), fi.Metadata[metaChecksumPrefix+algo])
		if err != nil {
			return err
		}
		delete(fi.Metadata, metaChecksumPrefix+algo)
		fi.Metadata[metaSealedChecksumPrefix+algo] = sealed
	}
	for i, p := range fi.Parts {
		if len(p.Checksums) == 0 {
			continue
		}
		sums := make(map[string]string, len(p.Checksums))
		for algo, v := range p.Checksums {
			c, err := sealPartChecksum(okey, domain.Checksum{Algorithm: domain.ChecksumAlgorithm(algo), Value: v})
			if err != nil {
				return err
			}
			sums[algo] = c.Value
		}
		fi.Parts[i].Checksums = sums
	}
	return nil
}

// hasSealedChecksums reports a version whose checksums are sealed.
func hasSealedChecksums(fi storage.FileInfo) bool {
	for k := range fi.Metadata {
		if strings.HasPrefix(k, metaSealedChecksumPrefix) {
			return true
		}
	}
	for _, p := range fi.Parts {
		for _, v := range p.Checksums {
			if crypto.IsSealedValue(v) {
				return true
			}
		}
	}
	return false
}

// openChecksums returns fi with the sealed checksums of an encrypted
// version opened with its object key: the checksum of the object back
// under x-amz-checksum-<algorithm>, those of the parts in clear. The
// metadata and the parts are copies: fi is left untouched.
func openChecksums(okey *crypto.ObjectKey, fi storage.FileInfo) (storage.FileInfo, error) {
	if okey == nil || !hasSealedChecksums(fi) {
		return fi, nil
	}
	meta := make(map[string]string, len(fi.Metadata))
	for k, v := range fi.Metadata {
		algo, sealed := strings.CutPrefix(k, metaSealedChecksumPrefix)
		if !sealed {
			meta[k] = v
			continue
		}
		clear, err := okey.OpenValue(objectChecksumName(algo), v)
		if err != nil {
			return storage.FileInfo{}, err
		}
		meta[metaChecksumPrefix+algo] = clear
	}
	fi.Metadata = meta
	parts := make([]storage.PartInfo, len(fi.Parts))
	for i, p := range fi.Parts {
		if len(p.Checksums) > 0 {
			sums := make(map[string]string, len(p.Checksums))
			for algo, v := range p.Checksums {
				c, err := openPartChecksum(okey, domain.Checksum{Algorithm: domain.ChecksumAlgorithm(algo), Value: v})
				if err != nil {
					return storage.FileInfo{}, err
				}
				sums[algo] = c.Value
			}
			p.Checksums = sums
		}
		parts[i] = p
	}
	fi.Parts = parts
	return fi, nil
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

// describeKey verifies the SSE-C key of a request that does not read the
// data (HEAD, attributes, part listings): an SSE-C version (or upload)
// needs its key, the others refuse one. It returns the object key when it
// unsealed it: always for SSE-C, for SSE-S3 and SSE-KMS only with open (a
// KMS call: the caller needs the sealed checksums), so that describing
// them needs no KMS otherwise.
func (l *layer) describeKey(ctx context.Context, bucket, key string, meta map[string]string, ckey domain.SSECustomerKey, open bool) (*crypto.ObjectKey, domain.EncryptionInfo, error) {
	info := crypto.InfoOf(meta)
	if info.Type != domain.SSEC && ckey.IsZero() && !open {
		return nil, info, nil
	}
	k, err := l.unsealFor(ctx, bucket, key, meta, ckey)
	if err != nil || k == nil {
		return nil, info, err
	}
	return k, k.Info(), nil
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
	pinned    []pinnedStream
}

// openEncrypted returns a reader over the plaintext [offset,
// offset+length) of an encrypted version.
func (l *layer) openEncrypted(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, okey *crypto.ObjectKey, offset, length int64) *encryptedReader {
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

// encryptedRead is the read of the part holding a plaintext offset: the
// part (index i), the plaintext range in it and the packages that hold it.
type encryptedRead struct {
	i       int
	part    storage.PartInfo
	partLen int64
	rng     crypto.Range
}

// locate returns the read of the part holding the plaintext offset off,
// bounded by remaining.
func (r *encryptedReader) locate(off, remaining int64) (encryptedRead, error) {
	var start int64
	for i, part := range r.fi.Parts {
		plain := part.ActualSize
		if off >= start+plain {
			start += plain
			continue
		}
		partOff := off - start
		partLen := min(plain-partOff, remaining)
		rng, err := crypto.PackageRange(plain, partOff, partLen)
		if err != nil {
			return encryptedRead{}, fmt.Errorf("object: %s/%s: %w", r.bucket, r.key, err)
		}
		if rng.StoredOffset+rng.StoredLength > part.Size {
			return encryptedRead{}, fmt.Errorf("object: %s/%s: %w: part %d is shorter than its packages", r.bucket, r.key, domain.ErrDecryption, part.Number)
		}
		return encryptedRead{i: i, part: part, partLen: partLen, rng: rng}, nil
	}
	return encryptedRead{}, fmt.Errorf("%w: offset %d beyond the parts of %s/%s", domain.ErrFaultyDisk, off, r.bucket, r.key)
}

// openNext opens the part holding the current offset, from the offset to
// the end of the range or of the part.
func (r *encryptedReader) openNext() error {
	rd, err := r.locate(r.offset, r.remaining)
	if err != nil {
		return err
	}
	rng := rd.rng
	var raw io.Reader
	if r.fi.DataDir == "" {
		if rng.StoredOffset+rng.StoredLength > int64(len(r.fi.Data)) {
			return fmt.Errorf("%w: inline data of %d bytes, want [%d,+%d)", domain.ErrFaultyDisk, len(r.fi.Data), rng.StoredOffset, rng.StoredLength)
		}
		raw = bytes.NewReader(r.fi.Data[rng.StoredOffset : rng.StoredOffset+rng.StoredLength])
	} else if rc := takePinned(&r.pinned, r.offset); rc != nil {
		raw, r.closer = rc, rc
	} else {
		rc, err := r.st.ReadData(r.ctx, r.bucket, r.key, r.fi, rd.part, rng.StoredOffset, rng.StoredLength)
		if err != nil {
			return fmt.Errorf("object: open part %d of %s/%s: %w", rd.part.Number, r.bucket, r.key, lockErr(r.ctx, err))
		}
		raw, r.closer = rc, rc
	}
	n, salt := encryptionPart(rd.part, rd.i)
	dec, err := r.okey.DecryptPart(n, salt, raw, rng)
	if err != nil {
		r.closeCur()
		return err
	}
	r.cur = dec
	return nil
}

// pin implements pinnable: the ciphertext streams of the parts of the
// range are opened up front (inline data needs none). A range over more
// than max parts is read under the lock without opening anything first.
func (r *encryptedReader) pin(max int) bool {
	if r.fi.DataDir == "" {
		return true
	}
	parts := 0
	for off, rem := r.offset, r.remaining; rem > 0; parts++ {
		rd, err := r.locate(off, rem)
		if err != nil || parts == max {
			return false
		}
		off, rem = off+rd.partLen, rem-rd.partLen
	}
	var opened []pinnedStream
	for off, rem := r.offset, r.remaining; rem > 0; {
		rd, err := r.locate(off, rem)
		if err != nil {
			closePinned(opened)
			return false
		}
		rc, pinned, err := r.st.OpenData(r.ctx, r.bucket, r.key, r.fi, rd.part, rd.rng.StoredOffset, rd.rng.StoredLength)
		if err != nil {
			closePinned(opened)
			return false
		}
		opened = append(opened, pinnedStream{off: off, rc: rc})
		if !pinned {
			closePinned(opened)
			return false
		}
		off, rem = off+rd.partLen, rem-rd.partLen
	}
	r.pinned = opened
	return true
}

func (r *encryptedReader) closeCur() {
	if r.closer != nil {
		_ = r.closer.Close()
	}
	r.cur, r.closer = nil, nil
}

func (r *encryptedReader) Close() error {
	closePinned(r.pinned)
	r.pinned = nil
	r.closeCur()
	return nil
}

// ResealKeys implements Service.
func (l *layer) ResealKeys(ctx context.Context, bucket string) (ResealResult, error) {
	if err := validBucket(bucket); err != nil {
		return ResealResult{}, err
	}
	if l.deps.Crypto == nil {
		return ResealResult{}, errSSENotConfigured()
	}
	if err := l.statBucket(ctx, l.sets[0], bucket); err != nil {
		return ResealResult{}, err
	}
	type version struct{ key, id string }
	var res ResealResult
	after := ""
	for {
		var (
			batch []version
			keys  int
			last  string
		)
		err := l.walkAll(ctx, storage.WalkDirOptions{Bucket: bucket, Recursive: true, ForwardTo: after}, func(e walkEntry) error {
			if e.IsDir || e.Name == after {
				return nil
			}
			keys, last = keys+1, e.Name
			for _, fi := range e.Versions {
				if kmsSealed(fi.Metadata) {
					id := fi.VersionID
					if id == "" {
						id = nullVersion
					}
					batch = append(batch, version{key: e.Name, id: id})
				}
			}
			if keys == purgeBatch {
				return errListDone
			}
			return nil
		})
		if err != nil && !errors.Is(err, errListDone) {
			return res, fmt.Errorf("%s: re-seal the keys of %s: %w", l.name, bucket, err)
		}
		for _, v := range batch {
			res.Versions++
			if err := l.resealVersion(ctx, bucket, v.key, v.id); err != nil {
				if cerr := ctx.Err(); cerr != nil {
					return res, cerr
				}
				res.Failed++
				if res.FirstError == "" {
					res.FirstError = fmt.Sprintf("%s (version %s): %v", v.key, v.id, err)
				}
				l.deps.Logger.WarnContext(ctx, "object: cannot re-seal a data key", "bucket", bucket, "key", v.key, "version", v.id, "err", err)
				continue
			}
			res.Resealed++
		}
		if keys < purgeBatch {
			break
		}
		after = last
	}
	// The pending multipart uploads hold the sealed key of their object in
	// their description: without re-sealing it, an upload created before
	// the rotation could no longer take parts nor complete once the
	// previous master key is removed.
	uploads, err := l.uploadsOf(ctx, bucket)
	if err != nil {
		return res, fmt.Errorf("%s: re-seal the keys of the uploads of %s: %w", l.name, bucket, err)
	}
	for _, m := range uploads {
		if !kmsSealed(m.Metadata) {
			continue
		}
		res.Uploads++
		if err := l.resealUpload(ctx, bucket, m.Key, m.UploadID); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return res, cerr
			}
			res.Failed++
			if res.FirstError == "" {
				res.FirstError = fmt.Sprintf("%s (upload %s): %v", m.Key, m.UploadID, err)
			}
			l.deps.Logger.WarnContext(ctx, "object: cannot re-seal the data key of an upload", "bucket", bucket, "key", m.Key, "upload", m.UploadID, "err", err)
			continue
		}
		res.Resealed++
	}
	return res, nil
}

// resealUpload re-seals the data key of a pending multipart upload in its
// description, under the write lock of the upload (no part is written
// meanwhile). An upload completed or aborted meanwhile is skipped.
func (l *layer) resealUpload(ctx context.Context, bucket, key, uploadID string) error {
	ctx, unlock, err := l.lockUpload(ctx, bucket, uploadID, true)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := l.place.forUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return err
	}
	m, dir, err := l.loadUpload(ctx, st, bucket, key, uploadID)
	if err != nil {
		if errors.Is(err, domain.ErrNoSuchUpload) {
			return nil
		}
		return err
	}
	if !kmsSealed(m.Metadata) {
		return nil
	}
	resealed, err := l.deps.Crypto.Reseal(ctx, crypto.UnsealInput{Bucket: bucket, Object: key, Metadata: m.Metadata})
	if err != nil {
		return err
	}
	maps.Copy(m.Metadata, resealed)
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode upload: %w", err)
	}
	if err := checkLocks(ctx); err != nil {
		return err
	}
	return st.WriteAll(ctx, dir+"/"+uploadFile, data)
}

// kmsSealed reports a version whose data key is sealed by the KMS
// (SSE-S3, SSE-KMS).
func kmsSealed(meta map[string]string) bool {
	t := crypto.InfoOf(meta).Type
	return t == domain.SSES3 || t == domain.SSEKMS
}

// errNotResealed reports a version replaced since the walk by one whose
// key is not sealed by the KMS: there is nothing to re-seal.
var errNotResealed = errors.New("object: version not sealed by the KMS")

// resealVersion re-seals the data key of one version in place.
func (l *layer) resealVersion(ctx context.Context, bucket, key, versionID string) error {
	_, err := l.updateMeta(ctx, bucket, key, versionID, func(fi storage.FileInfo, meta map[string]string) error {
		if !kmsSealed(fi.Metadata) {
			return errNotResealed
		}
		resealed, err := l.deps.Crypto.Reseal(ctx, crypto.UnsealInput{Bucket: bucket, Object: key, Metadata: fi.Metadata})
		if err != nil {
			return err
		}
		maps.Copy(meta, resealed)
		return nil
	}, "")
	if errors.Is(err, errNotResealed) || errors.Is(err, domain.ErrNoSuchVersion) || isNotFound(err) {
		return nil // overwritten or deleted meanwhile
	}
	return err
}
