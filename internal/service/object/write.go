package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// versionFor decides the version id of a new write from the bucket state:
// a fresh id when versioning is enabled, the null version ("") otherwise.
func (l *layer) versionFor(cfg BucketConfig, forced string) (versionID string, versioned bool) {
	if forced != "" && forced != "null" {
		return forced, true
	}
	switch cfg.Versioning {
	case domain.VersioningEnabled:
		return l.newID(), true
	case domain.VersioningSuspended:
		return "", true
	default:
		return "", false
	}
}

// writeInput is what a single-stream write needs beyond the body.
type writeInput struct {
	size       int64
	md5Hex     string
	sha256Hex  string
	checksum   domain.Checksum
	meta       map[string]string
	conditions domain.Conditions
	versionID  string
	modTime    time.Time
	eventName  event.Name
	// withinLock reports that the caller holds the object write lock.
	withinLock bool
	// lengthRange bounds the body length (POST policy); nil means unbounded.
	lengthRange *LengthRange
	// retention and legalHold are the Object Lock settings requested for
	// the new version (zero: the bucket default retention, no legal hold).
	retention domain.ObjectRetention
	legalHold domain.LegalHoldStatus
	// sse is the encryption requested for the new version (zero: the
	// default encryption of the bucket, if any).
	sse domain.SSERequest
	// replica marks a version written by the replication of another site,
	// replicaETag its ETag there (PutObjectInput).
	replica     bool
	replicaETag string
}

// writeObject stores one stream as a new version of bucket/key. The body
// is streamed to a private temporary directory (or kept in memory when it
// is small enough to be inlined) without holding any lock, so that a slow
// or stalled client never blocks the readers of the key; the staged data
// is then committed atomically under the object write lock: readers see
// either the previous version or the new one. The checks that do not need
// the body (bucket, preconditions, declared size against the quota) run
// before streaming, to fail fast, and again under the lock, where they are
// authoritative. With withinLock the caller already holds the write lock.
func (l *layer) writeObject(ctx context.Context, bucket, key string, body io.Reader, in writeInput) (domain.ObjectInfo, error) {
	if err := validObject(bucket, key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := validSize(in.size); err != nil {
		return domain.ObjectInfo{}, err
	}
	if body == nil {
		body = bytes.NewReader(nil)
	}
	st, err := l.place.forWrite(ctx, bucket, key, in.size)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: place %s/%s: %w", l.name, bucket, key, err)
	}
	cfg, err := l.checkWrite(ctx, st, bucket, key, in)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	// The key of an encrypted version is sealed before the body is read:
	// a KMS that is missing or failing refuses the write up front.
	okey, err := l.sealFor(ctx, cfg, bucket, key, in.sse)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if lr := in.lengthRange; lr != nil {
		// A declared size is checked up front; a streamed body is cut as
		// soon as it exceeds the maximum and checked against the minimum
		// once fully read, before the commit.
		if in.size >= 0 {
			if err := checkLength(lr, in.size); err != nil {
				return domain.ObjectInfo{}, err
			}
		} else {
			body = &boundedReader{r: body, max: lr.Max}
		}
	}

	hr, err := newHashReader(body, in.sha256Hex != "", in.checksum.Algorithm)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	// The store receives the plaintext, or its encryption: the hashes,
	// the checksums and the lengths checked below are those of the
	// plaintext, which hr counts.
	data, storedSize := io.Reader(hr), in.size
	if okey != nil {
		plain := io.Reader(hr)
		if in.size >= 0 {
			plain = io.LimitReader(hr, in.size)
		}
		// The object key is fresh: its single part needs no salt.
		if data, err = okey.EncryptPart(1, nil, plain); err != nil {
			return domain.ObjectInfo{}, err
		}
		storedSize = crypto.EncryptedSize(in.size)
	}
	var comp *compressor
	if l.compressFor(ctx, bucket, key, in.meta[metaContentType], in.size, okey != nil) {
		plain := io.Reader(hr)
		if in.size >= 0 {
			plain = io.LimitReader(hr, in.size)
		}
		comp = newCompressor(plain)
		data, storedSize = comp, -1
	}
	fi := storage.FileInfo{
		Volume:   bucket,
		Name:     key,
		Metadata: in.meta,
		Erasure:  st.ErasureInfo(key, in.meta[metaStorageClass]),
	}
	tmpDir := ""
	var compIndex []byte
	if storedSize >= 0 && storedSize <= l.opts.inlineLimit {
		buf, err := io.ReadAll(io.LimitReader(data, storedSize+1))
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%s: read body: %w", l.name, mapBodyErr(err))
		}
		if hr.n != in.size || int64(len(buf)) != storedSize {
			return domain.ObjectInfo{}, incompleteBody(in.size, hr.n)
		}
		fi.Data = buf
		fi.Size = storedSize
	} else {
		tmpDir = l.newTmpDir()
		fi.DataDir = uuid.NewString()
		n, err := st.WriteData(ctx, tmpDir, 1, storedSize, data, fi.Erasure)
		if err != nil {
			if comp != nil {
				comp.abort()
			}
			l.cleanupTmp(ctx, st, tmpDir)
			return domain.ObjectInfo{}, fmt.Errorf("%s: write %s/%s: %w", l.name, bucket, key, mapBodyErr(err))
		}
		if comp != nil {
			if compIndex, err = comp.finish(); err != nil {
				l.cleanupTmp(ctx, st, tmpDir)
				return domain.ObjectInfo{}, fmt.Errorf("%s: write %s/%s: %w", l.name, bucket, key, mapBodyErr(err))
			}
		}
		plainN := n
		if okey != nil || comp != nil {
			plainN = hr.n
		}
		if in.size >= 0 && plainN != in.size {
			l.cleanupTmp(ctx, st, tmpDir)
			return domain.ObjectInfo{}, incompleteBody(in.size, plainN)
		}
		if in.lengthRange != nil {
			if err := checkLength(in.lengthRange, plainN); err != nil {
				l.cleanupTmp(ctx, st, tmpDir)
				return domain.ObjectInfo{}, err
			}
		}
		fi.Size = n
	}
	plainSize := fi.Size
	if okey != nil || comp != nil {
		plainSize = hr.n
	}
	checksum, err := hr.verify(in.md5Hex, in.sha256Hex, in.checksum)
	if err != nil {
		l.cleanupTmp(ctx, st, tmpDir)
		return domain.ObjectInfo{}, err
	}
	if fi.Metadata == nil {
		fi.Metadata = map[string]string{metaContentType: defaultContentType}
	}
	etag := hr.etag()
	if okey != nil {
		etag = okey.ETag(etag)
		for k, v := range okey.Metadata() {
			fi.Metadata[k] = v
		}
		setActualSize(fi.Metadata, plainSize)
	}
	if comp != nil {
		fi.Metadata[metaCompression] = compressionS2
		setActualSize(fi.Metadata, plainSize)
	}
	fi.Metadata[metaETag] = etag
	if !checksum.IsZero() {
		fi.Metadata[metaChecksumPrefix+strings.ToLower(string(checksum.Algorithm))] = checksum.Value
		fi.Metadata[metaChecksumType] = string(domain.ChecksumTypeFullObject)
	}

	// Commit: under the object write lock, with the checks repeated
	// against the state of that moment.
	if !in.withinLock {
		lctx, unlock, err := l.lockWrite(ctx, bucket, key)
		if err != nil {
			l.cleanupTmp(ctx, st, tmpDir)
			return domain.ObjectInfo{}, err
		}
		defer unlock()
		ctx = lctx
		if cfg, err = l.checkWrite(ctx, st, bucket, key, in); err != nil {
			l.cleanupTmp(ctx, st, tmpDir)
			return domain.ObjectInfo{}, err
		}
	}
	// #nosec G115 -- plainSize counts the bytes written, never negative
	if cfg.QuotaBytes > 0 && uint64(plainSize) > cfg.QuotaBytes {
		l.cleanupTmp(ctx, st, tmpDir)
		return domain.ObjectInfo{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, plainSize, cfg.QuotaBytes)
	}
	// The version and its time are decided at the commit, so that the
	// latest version is always the last one committed.
	fi.VersionID, fi.Versioned = l.versionFor(cfg, in.versionID)
	fi.ModTime = in.modTime
	if fi.ModTime.IsZero() {
		fi.ModTime = l.commitTime(ctx, bucket, key)
	}
	fi.Parts = []storage.PartInfo{{Number: 1, ETag: etag, Size: fi.Size, ActualSize: plainSize, ModTime: fi.ModTime, Index: compIndex}}
	setRetentionMeta(fi.Metadata, in.retention)
	setLegalHoldMeta(fi.Metadata, in.legalHold)
	applyDefaultRetention(fi.Metadata, cfg, fi.ModTime)
	setReplicationMeta(fi.Metadata, cfg, key, in.replica)
	if in.replica {
		keepReplicaETag(fi.Metadata, in.replicaETag)
	}
	// The response reports the checksum computed above; the drives only
	// receive it sealed when the version is encrypted.
	clearInfo := objectInfo(bucket, key, fi)
	if err := sealChecksums(okey, &fi); err != nil {
		l.cleanupTmp(ctx, st, tmpDir)
		return domain.ObjectInfo{}, fmt.Errorf("%s: seal the checksum of %s/%s: %w", l.name, bucket, key, err)
	}
	if err := l.commitVersion(ctx, st, bucket, key, fi, tmpDir); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: commit %s/%s: %w", l.name, bucket, key, err)
	}
	fi.IsLatest = true
	info := objectInfo(bucket, key, fi)
	info.Checksum = clearInfo.Checksum
	if okey != nil {
		info.Encryption = okey.Info()
	}
	if in.eventName != "" {
		l.publish(ctx, in.eventName, bucket, info)
	}
	return info, nil
}

// checkWrite runs the checks of a write that do not need its body: the
// bucket exists, the write preconditions hold against the current version
// and the declared size fits the quota. It returns the bucket settings.
func (l *layer) checkWrite(ctx context.Context, st store, bucket, key string, in writeInput) (BucketConfig, error) {
	if err := l.statBucket(ctx, st, bucket); err != nil {
		return BucketConfig{}, err
	}
	cfg, err := l.bucketConfig(ctx, bucket)
	if err != nil {
		return BucketConfig{}, err
	}
	if err := checkLockRequest(cfg, in.retention, in.legalHold, l.now()); err != nil {
		return BucketConfig{}, err
	}
	if err := l.checkCurrent(ctx, bucket, key, in.conditions); err != nil {
		return BucketConfig{}, err
	}
	if cfg.QuotaBytes > 0 && in.size > 0 && uint64(in.size) > cfg.QuotaBytes {
		return BucketConfig{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, in.size, cfg.QuotaBytes)
	}
	return cfg, nil
}

// checkCurrent evaluates write preconditions against the current version.
func (l *layer) checkCurrent(ctx context.Context, bucket, key string, c domain.Conditions) error {
	if c.IsZero() {
		return nil
	}
	st, err := l.place.forRead(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("%s: conditions on %s/%s: %w", l.name, bucket, key, err)
	}
	cur, err := st.ReadVersion(ctx, bucket, key, "", false)
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("%s: conditions on %s/%s: %w", l.name, bucket, key, err)
	}
	exists := err == nil && !cur.Deleted
	return checkWriteConditions(exists, objectInfo(bucket, key, cur), c)
}

// commitVersion makes a version visible (the store purges the data it
// replaces) and removes the copies the other pools may hold.
func (l *layer) commitVersion(ctx context.Context, st store, bucket, key string, fi storage.FileInfo, tmpDir string) error {
	if err := checkLocks(ctx); err != nil {
		l.cleanupTmp(ctx, st, tmpDir)
		return err
	}
	if err := st.CommitData(ctx, tmpDir, fi, bucket, key); err != nil {
		l.cleanupTmp(ctx, st, tmpDir)
		if cerr := checkLocks(ctx); cerr != nil {
			return cerr
		}
		return err
	}
	l.dropElsewhere(ctx, st, bucket, key, fi.VersionID)
	return nil
}

// commitTime returns the modification time of a new version (or delete
// marker) of bucket/key, decided under its write lock: the local clock,
// but always after the latest version already there and after the last
// metadata update of that version (tags, Object Lock). Versions are
// ordered by modification time, and the nodes of a cluster do not share
// a clock: a write coordinated by a node whose clock lags behind the one
// of the previous writer must still become the latest version (docs/06
// TC-13). The same time orders the events of the key (their sequencer).
func (l *layer) commitTime(ctx context.Context, bucket, key string) time.Time {
	now := l.now()
	st, err := l.place.forRead(ctx, bucket, key)
	if err != nil {
		return now
	}
	cur, err := st.ReadVersion(ctx, bucket, key, "", false)
	if err != nil {
		return now
	}
	after := cur.ModTime
	if stamp := metaStamp(cur.Metadata); stamp > after.UnixNano() {
		after = time.Unix(0, stamp)
	}
	if after.Before(now) {
		return now
	}
	return after.Add(time.Nanosecond).UTC()
}

// dropElsewhere keeps an object in a single pool: a commit of the null
// version replaces the null version wherever it is, so a copy left in
// another pool (two first writes of the key racing to different pools) is
// removed. Other versions are distinct versions and stay where they are;
// reads pick the newest across pools.
func (l *layer) dropElsewhere(ctx context.Context, st store, bucket, key, versionID string) {
	if versionID != "" || lockLost(ctx) {
		return
	}
	ctx = context.WithoutCancel(ctx)
	for _, other := range l.place.others(st, key) {
		err := other.DeleteVersion(ctx, bucket, key, storage.FileInfo{})
		if err != nil && !isNotFound(err) && !errors.Is(err, domain.ErrNoSuchVersion) {
			l.deps.Logger.WarnContext(ctx, "object: cannot remove the copy of another pool", "bucket", bucket, "key", key, "err", err)
		}
	}
}

// validSize accepts a declared content length, or -1 for an unknown one.
func validSize(size int64) error {
	if size < -1 {
		return fmt.Errorf("%w: invalid content length %d", domain.ErrInvalidArgument, size)
	}
	return nil
}

// checkLength validates a content length against an inclusive range.
func checkLength(lr *LengthRange, size int64) error {
	if size < lr.Min {
		return domain.NewS3Error(domain.ErrEntityTooSmall, "EntityTooSmall",
			"Your proposed upload is smaller than the minimum allowed size: "+strconv.FormatInt(lr.Min, 10))
	}
	if size > lr.Max {
		return entityTooLarge(lr.Max)
	}
	return nil
}

func entityTooLarge(maxSize int64) error {
	return domain.NewS3Error(domain.ErrEntityTooLarge, "EntityTooLarge",
		"Your proposed upload exceeds the maximum allowed size: "+strconv.FormatInt(maxSize, 10))
}

// boundedReader fails with EntityTooLarge as soon as more than max bytes
// are read, so that an oversized stream is never stored in full.
type boundedReader struct {
	r   io.Reader
	max int64
	n   int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return n, entityTooLarge(b.max)
	}
	return n, err
}

func incompleteBody(want, got int64) error {
	return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody",
		fmt.Sprintf("You did not provide the number of bytes specified by the Content-Length HTTP header: expected %d, received %d", want, got))
}

// mapBodyErr keeps client body failures distinguishable from storage ones.
func mapBodyErr(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return domain.NewS3Error(domain.ErrInvalidArgument, "IncompleteBody", "The request body ended before the declared length.")
	}
	return err
}

// PutObject implements Service.
func (l *layer) PutObject(ctx context.Context, in PutObjectInput) (domain.ObjectInfo, error) {
	if err := validateTags(in.Tags); err != nil {
		return domain.ObjectInfo{}, err
	}
	if (!in.Retention.IsZero() || in.LegalHold != "") && in.MD5Hex == "" && in.Checksum.IsZero() {
		return domain.ObjectInfo{}, errLockMD5Required()
	}
	meta := httpMeta{
		contentType:        in.ContentType,
		contentEncoding:    in.ContentEncoding,
		contentDisposition: in.ContentDisposition,
		contentLanguage:    in.ContentLanguage,
		cacheControl:       in.CacheControl,
		expires:            in.Expires,
		user:               in.UserMetadata,
	}
	return l.writeObject(ctx, in.Bucket, in.Key, in.Reader, writeInput{
		size:        in.Size,
		md5Hex:      in.MD5Hex,
		sha256Hex:   in.SHA256Hex,
		checksum:    in.Checksum,
		meta:        meta.build(in.Tags, in.StorageClass),
		conditions:  in.Conditions,
		versionID:   in.VersionID,
		modTime:     in.ModTime,
		eventName:   event.ObjectCreatedPut,
		lengthRange: in.LengthRange,
		retention:   in.Retention,
		legalHold:   in.LegalHold,
		sse:         in.SSE,
		replica:     in.Replica,
		replicaETag: in.ReplicaETag,
	})
}

// CopyObject implements Service. The copy is a new write of the source
// content: it is atomic like PutObject and never exposes a partial object.
func (l *layer) CopyObject(ctx context.Context, in CopyObjectInput) (domain.ObjectInfo, error) {
	if err := validObject(in.SrcBucket, in.SrcKey); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := validObject(in.DstBucket, in.DstKey); err != nil {
		return domain.ObjectInfo{}, err
	}
	if in.ReplaceTags {
		if err := validateTags(in.Tags); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	ctx, unlock, err := l.lockCopy(ctx, in.SrcBucket, in.SrcKey, in.DstBucket, in.DstKey)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	if err := l.statBucket(ctx, l.place.home(in.DstKey), in.DstBucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	src, fi, err := l.openObject(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, SSECustomerKey: in.SrcSSECustomerKey}, nil)
	if err != nil {
		return domain.ObjectInfo{}, copySourceErr(err)
	}
	defer func() { _ = src.Close() }()
	if err := checkSourceConditions(src.Info, in.SrcConditions); err != nil {
		return domain.ObjectInfo{}, err
	}
	same := in.SrcBucket == in.DstBucket && in.SrcKey == in.DstKey
	storageClass := in.StorageClass
	if storageClass == "" {
		storageClass = src.Info.StorageClass
	}
	checksumAlgo := in.ChecksumAlgorithm
	if checksumAlgo == domain.ChecksumNone {
		checksumAlgo = src.Info.Checksum.Algorithm
	}
	lockChange := !in.Retention.IsZero() || in.LegalHold != ""
	// A copy that asks for an encryption re-keys the object, and one that
	// asks for none of an encrypted source decrypts it (unless the default
	// encryption of the bucket encrypts it the same way again): legal
	// self-copies (key rotation, SSE-C to SSE-S3, in-place decryption).
	sseChange := !in.SSE.IsZero()
	if !sseChange && same && src.Info.Encryption.Type != domain.SSENone {
		cfg, err := l.bucketConfig(ctx, in.DstBucket)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		sseChange = effectiveSSE(cfg, in.SSE).Type != src.Info.Encryption.Type
	}
	if same && in.SrcVersionID == "" && !in.ReplaceMetadata && !in.ReplaceTags && storageClass == src.Info.StorageClass &&
		checksumAlgo == src.Info.Checksum.Algorithm && !lockChange && !sseChange {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	}
	if src.Length > maxCopySize {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The specified copy source is larger than the maximum allowable size for a copy source: "+strconv.FormatInt(maxCopySize, 10))
	}

	meta := copyableMeta(fi.Metadata)
	if in.ReplaceMetadata {
		meta = httpMeta{
			contentType:        in.ContentType,
			contentEncoding:    in.ContentEncoding,
			contentDisposition: in.ContentDisposition,
			contentLanguage:    in.ContentLanguage,
			cacheControl:       in.CacheControl,
			expires:            in.Expires,
			user:               in.UserMetadata,
		}.build(nil, "")
	}
	tags := src.Info.Tags
	if in.ReplaceTags {
		tags = in.Tags
	}
	if len(tags) > 0 {
		meta[metaTagging] = encodeTags(tags)
	}
	if storageClass != "" && storageClass != domain.StorageClassStandard {
		meta[metaStorageClass] = storageClass
	}
	return l.writeObject(ctx, in.DstBucket, in.DstKey, src, writeInput{
		size:       src.Length,
		checksum:   domain.Checksum{Algorithm: checksumAlgo},
		meta:       meta,
		eventName:  event.ObjectCreatedCopy,
		withinLock: true,
		retention:  in.Retention,
		legalHold:  in.LegalHold,
		sse:        in.SSE,
	})
}

// copySourceErr maps the failure to open the source of a copy: a source
// version that is a delete marker cannot be copied (S3 InvalidRequest),
// while a source whose latest version is a delete marker does not exist
// (NoSuchKey, without the delete marker details of a read).
func copySourceErr(err error) error {
	var marker *domain.DeleteMarkerError
	if !errors.As(err, &marker) {
		return err
	}
	if errors.Is(err, domain.ErrMethodNotAllowed) {
		return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The source of a copy request may not specifically refer to a delete marker by version id.")
	}
	return marker.Err
}
