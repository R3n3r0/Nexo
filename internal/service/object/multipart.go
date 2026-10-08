package object

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Multipart bookkeeping lives in the system volume:
//
//	multipart/<sha256(bucket/key)>/<uploadID>/upload.json   the upload
//	multipart/<sha256(bucket/key)>/<uploadID>/part.N        the part data
//	multipart/<sha256(bucket/key)>/<uploadID>/part.N.meta   the part description
const (
	uploadFile = "upload.json"
	metaSuffix = ".meta"
)

// uploadMeta is the persisted description of an in-progress upload.
type uploadMeta struct {
	Bucket    string                   `json:"bucket"`
	Key       string                   `json:"key"`
	UploadID  string                   `json:"uploadId"`
	Initiated time.Time                `json:"initiated"`
	Metadata  map[string]string        `json:"metadata"`
	Checksum  domain.ChecksumAlgorithm `json:"checksum,omitempty"`
}

// partMeta is the persisted description of an uploaded part.
type partMeta struct {
	Number     int             `json:"number"`
	ETag       string          `json:"etag"`
	Size       int64           `json:"size"`
	ActualSize int64           `json:"actualSize"`
	ModTime    time.Time       `json:"modTime"`
	Checksum   domain.Checksum `json:"checksum,omitempty"`
}

func uploadsRoot(bucket, key string) string {
	sum := sha256.Sum256([]byte(bucket + "/" + key))
	return storage.MultipartDir + "/" + hex.EncodeToString(sum[:])
}

func validUploadID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlnum && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

func (m uploadMeta) toDomain() domain.MultipartUpload {
	info := objectInfo(m.Bucket, m.Key, storage.FileInfo{Metadata: m.Metadata})
	return domain.MultipartUpload{
		Bucket:       m.Bucket,
		Key:          m.Key,
		UploadID:     m.UploadID,
		Initiated:    m.Initiated,
		StorageClass: info.StorageClass,
		ContentType:  info.ContentType,
		UserMetadata: info.UserMetadata,
		Tags:         info.Tags,
		Checksum:     m.Checksum,
	}
}

func (p partMeta) toDomain() domain.Part {
	return domain.Part{Number: p.Number, ETag: p.ETag, Size: p.Size, ModTime: p.ModTime, ActualSize: p.ActualSize, Checksum: p.Checksum}
}

// loadUpload reads the upload description or reports ErrNoSuchUpload.
func (l *layer) loadUpload(ctx context.Context, st store, bucket, key, uploadID string) (uploadMeta, string, error) {
	if !validUploadID(uploadID) {
		return uploadMeta{}, "", fmt.Errorf("%w: %q", domain.ErrNoSuchUpload, uploadID)
	}
	dir := uploadsRoot(bucket, key) + "/" + uploadID
	data, err := st.ReadAll(ctx, dir+"/"+uploadFile)
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) {
			return uploadMeta{}, "", fmt.Errorf("%w: %s", domain.ErrNoSuchUpload, uploadID)
		}
		return uploadMeta{}, "", fmt.Errorf("%s: read upload %s: %w", l.name, uploadID, err)
	}
	var m uploadMeta
	if err := json.Unmarshal(data, &m); err != nil || m.Bucket != bucket || m.Key != key {
		return uploadMeta{}, "", fmt.Errorf("%w: %s", domain.ErrNoSuchUpload, uploadID)
	}
	return m, dir, nil
}

// NewMultipartUpload implements Service.
func (l *layer) NewMultipartUpload(ctx context.Context, in NewMultipartInput) (domain.MultipartUpload, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.MultipartUpload{}, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return domain.MultipartUpload{}, err
	}
	if err := validateTags(in.Tags); err != nil {
		return domain.MultipartUpload{}, err
	}
	if _, err := newHashReader(nil, false, in.Checksum); err != nil {
		return domain.MultipartUpload{}, err
	}
	st := l.pick(in.Key)
	if _, err := st.StatVol(ctx, in.Bucket); err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: new upload %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	m := uploadMeta{
		Bucket:    in.Bucket,
		Key:       in.Key,
		UploadID:  strings.ReplaceAll(l.newID(), "/", "_"),
		Initiated: l.now(),
		Metadata:  buildMetadata(in.ContentType, in.UserMetadata, in.Tags, in.StorageClass, domain.Checksum{}),
		Checksum:  in.Checksum,
	}
	if !validUploadID(m.UploadID) {
		m.UploadID = uuid.NewString()
	}
	data, err := json.Marshal(m)
	if err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: encode upload: %w", l.name, err)
	}
	dir := uploadsRoot(in.Bucket, in.Key) + "/" + m.UploadID
	if err := st.WriteAll(ctx, dir+"/"+uploadFile, data); err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: new upload %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	return m.toDomain(), nil
}

// PutObjectPart implements Service.
func (l *layer) PutObjectPart(ctx context.Context, in PutObjectPartInput) (domain.Part, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.Part{}, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return domain.Part{}, err
	}
	if in.PartNumber < 1 || in.PartNumber > maxPartNumber {
		return domain.Part{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
	}
	st := l.pick(in.Key)
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.Part{}, err
	}
	algo := in.Checksum.Algorithm
	if algo == domain.ChecksumNone {
		algo = m.Checksum
	}
	body := in.Reader
	if body == nil {
		body = strings.NewReader("")
	}
	hr, err := newHashReader(body, in.SHA256Hex != "", algo)
	if err != nil {
		return domain.Part{}, err
	}
	// Parts are written to a private temporary directory, then moved into
	// the upload directory so that a re-upload replaces the previous part
	// atomically.
	tmpDir := storage.TmpDir + "/" + l.newID()
	n, err := st.WriteData(ctx, tmpDir, in.PartNumber, in.Size, hr)
	if err != nil {
		_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		return domain.Part{}, fmt.Errorf("%s: upload part %d of %s: %w", l.name, in.PartNumber, in.UploadID, mapBodyErr(err))
	}
	if in.Size >= 0 && n != in.Size {
		_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		return domain.Part{}, incompleteBody(in.Size, n)
	}
	checksum, err := hr.verify(in.MD5Hex, in.SHA256Hex, in.Checksum)
	if err != nil {
		_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		return domain.Part{}, err
	}
	part := partMeta{Number: in.PartNumber, ETag: hr.etag(), Size: n, ActualSize: n, ModTime: l.now(), Checksum: checksum}
	if err := st.RenameFile(ctx, tmpDir+"/"+partName(in.PartNumber), dir+"/"+partName(in.PartNumber)); err != nil {
		_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
	data, err := json.Marshal(part)
	if err != nil {
		return domain.Part{}, fmt.Errorf("%s: encode part: %w", l.name, err)
	}
	if err := st.WriteAll(ctx, dir+"/"+partName(in.PartNumber)+metaSuffix, data); err != nil {
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	return part.toDomain(), nil
}

// CopyObjectPart implements Service.
func (l *layer) CopyObjectPart(ctx context.Context, in CopyObjectPartInput) (domain.Part, error) {
	for _, b := range []string{in.SrcBucket, in.DstBucket} {
		if err := validBucket(b); err != nil {
			return domain.Part{}, err
		}
	}
	for _, k := range []string{in.SrcKey, in.DstKey} {
		if err := domain.ValidateObjectKey(k); err != nil {
			return domain.Part{}, err
		}
	}
	src, err := l.GetObject(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, Range: in.SrcRange, Conditions: in.SrcConditions})
	if err != nil {
		return domain.Part{}, err
	}
	defer func() { _ = src.Close() }()
	return l.PutObjectPart(ctx, PutObjectPartInput{
		Bucket: in.DstBucket, Key: in.DstKey, UploadID: in.UploadID, PartNumber: in.PartNumber,
		Reader: src, Size: src.Length,
	})
}

// listParts reads every part description of an upload, sorted by number.
func (l *layer) listParts(ctx context.Context, st store, dir string) ([]partMeta, error) {
	infos, err := st.StatGlob(ctx, dir+"/part.*"+metaSuffix)
	if err != nil {
		return nil, fmt.Errorf("%s: list parts: %w", l.name, err)
	}
	parts := make([]partMeta, 0, len(infos))
	for _, info := range infos {
		data, err := st.ReadAll(ctx, info.Name)
		if err != nil {
			if errors.Is(err, domain.ErrObjectNotFound) {
				continue
			}
			return nil, fmt.Errorf("%s: read part %s: %w", l.name, info.Name, err)
		}
		var p partMeta
		if err := json.Unmarshal(data, &p); err != nil {
			l.deps.Logger.WarnContext(ctx, "object: skipping unreadable part description", "path", info.Name, "err", err)
			continue
		}
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// ListObjectParts implements Service.
func (l *layer) ListObjectParts(ctx context.Context, in domain.ListPartsInput) (domain.ListPartsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.ListPartsResult{}, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return domain.ListPartsResult{}, err
	}
	st := l.pick(in.Key)
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ListPartsResult{}, err
	}
	parts, err := l.listParts(ctx, st, dir)
	if err != nil {
		return domain.ListPartsResult{}, err
	}
	maxParts := in.MaxParts
	if maxParts <= 0 || maxParts > maxListKeys {
		maxParts = maxListKeys
	}
	res := domain.ListPartsResult{Upload: m.toDomain()}
	for _, p := range parts {
		if p.Number <= in.PartNumberMarker {
			continue
		}
		if len(res.Parts) == maxParts {
			res.IsTruncated = true
			res.NextPartNumberMarker = res.Parts[len(res.Parts)-1].Number
			break
		}
		res.Parts = append(res.Parts, p.toDomain())
	}
	return res, nil
}

// AbortMultipartUpload implements Service.
func (l *layer) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	if err := domain.ValidateObjectKey(key); err != nil {
		return err
	}
	st := l.pick(key)
	if _, _, err := l.loadUpload(ctx, st, bucket, key, uploadID); err != nil {
		return err
	}
	if err := st.DeletePath(ctx, domain.SystemBucket, uploadsRoot(bucket, key)+"/"+uploadID, true); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
		return fmt.Errorf("%s: abort upload %s: %w", l.name, uploadID, err)
	}
	return nil
}

// CompleteMultipartUpload implements Service.
func (l *layer) CompleteMultipartUpload(ctx context.Context, in CompleteMultipartInput) (domain.ObjectInfo, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := domain.ValidateObjectKey(in.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if len(in.Parts) == 0 {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: no parts")
	}
	lc, err := l.lock(ctx, in.Bucket, in.Key, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer lc.Unlock()
	st := l.pick(in.Key)
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	uploaded, err := l.listParts(ctx, st, dir)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	byNumber := make(map[int]partMeta, len(uploaded))
	for _, p := range uploaded {
		byNumber[p.Number] = p
	}

	var (
		total   int64
		etagSum = md5.New()
		parts   = make([]storage.PartInfo, 0, len(in.Parts))
	)
	for i := 1; i < len(in.Parts); i++ {
		if in.Parts[i].Number <= in.Parts[i-1].Number {
			return domain.ObjectInfo{}, fmt.Errorf("%w: part %d after part %d", domain.ErrInvalidPartOrder, in.Parts[i].Number, in.Parts[i-1].Number)
		}
	}
	for i, cp := range in.Parts {
		p, ok := byNumber[cp.Number]
		if !ok || !strings.EqualFold(strings.Trim(cp.ETag, `"`), p.ETag) {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. Part %d may not have been uploaded, or the specified entity tag may not match the part's entity tag.", cp.Number))
		}
		if i < len(in.Parts)-1 && p.Size < l.opts.minPartSize {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrEntityTooSmall, "EntityTooSmall",
				fmt.Sprintf("Your proposed upload is smaller than the minimum allowed object size. Part %d is %d bytes, the minimum is %d.", cp.Number, p.Size, l.opts.minPartSize))
		}
		raw, err := hex.DecodeString(p.ETag)
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%w: part %d has a malformed etag", domain.ErrInvalidPart, cp.Number)
		}
		_, _ = etagSum.Write(raw)
		total += p.Size
		parts = append(parts, storage.PartInfo{Number: i + 1, ETag: p.ETag, Size: p.Size, ActualSize: p.ActualSize, ModTime: p.ModTime, Checksums: checksumMap(p.Checksum)})
	}
	if !in.Conditions.IsZero() {
		cur, err := st.ReadVersion(ctx, in.Bucket, in.Key, "", false)
		exists := err == nil && !cur.Deleted
		if err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
			return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
		}
		if err := checkWriteConditions(exists, objectInfo(in.Bucket, in.Key, cur), in.Conditions); err != nil {
			return domain.ObjectInfo{}, err
		}
	}
	cfg, err := l.bucketConfig(ctx, in.Bucket)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if cfg.QuotaBytes > 0 && total > int64(cfg.QuotaBytes) {
		return domain.ObjectInfo{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, total, cfg.QuotaBytes)
	}

	// Assemble the selected parts, renumbered, in a fresh temporary directory.
	tmpDir := storage.TmpDir + "/" + l.newID()
	for i, cp := range in.Parts {
		if err := st.RenameFile(ctx, dir+"/"+partName(cp.Number), tmpDir+"/"+partName(i+1)); err != nil {
			_ = st.DeletePath(ctx, domain.SystemBucket, tmpDir, true)
			return domain.ObjectInfo{}, fmt.Errorf("%s: assemble part %d of %s: %w", l.name, cp.Number, in.UploadID, err)
		}
	}
	versionID, versioned := l.versionFor(cfg, "")
	meta := make(map[string]string, len(m.Metadata)+1)
	for k, v := range m.Metadata {
		meta[k] = v
	}
	meta[metaETag] = hex.EncodeToString(etagSum.Sum(nil)) + "-" + strconv.Itoa(len(parts))
	if !in.Checksum.IsZero() {
		meta[metaChecksumPrefix+strings.ToLower(string(in.Checksum.Algorithm))] = in.Checksum.Value
	}
	fi := storage.FileInfo{
		Volume:    in.Bucket,
		Name:      in.Key,
		VersionID: versionID,
		ModTime:   l.now(),
		Size:      total,
		DataDir:   uuid.NewString(),
		Metadata:  meta,
		Parts:     parts,
		Erasure:   st.ErasureInfo(),
		Versioned: versioned,
	}
	if err := l.commitVersion(ctx, st, in.Bucket, in.Key, fi, tmpDir); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
	}
	if err := st.DeletePath(ctx, domain.SystemBucket, dir, true); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
		l.deps.Logger.WarnContext(ctx, "object: cannot remove completed upload", "upload", in.UploadID, "err", err)
	}
	fi.IsLatest = true
	info := objectInfo(in.Bucket, in.Key, fi)
	l.publish(ctx, event.ObjectCreatedCompleteMultipartUpload, in.Bucket, info)
	return info, nil
}

func checksumMap(c domain.Checksum) map[string]string {
	if c.IsZero() {
		return nil
	}
	return map[string]string{string(c.Algorithm): c.Value}
}

// uploadsOf lists the in-progress uploads of a bucket across all stores.
func (l *layer) uploadsOf(ctx context.Context, bucket string) ([]uploadMeta, error) {
	var all []uploadMeta
	for _, st := range l.sets {
		infos, err := st.StatGlob(ctx, storage.MultipartDir+"/*/*/"+uploadFile)
		if err != nil {
			return nil, fmt.Errorf("%s: list uploads: %w", l.name, err)
		}
		for _, info := range infos {
			data, err := st.ReadAll(ctx, info.Name)
			if err != nil {
				continue
			}
			var m uploadMeta
			if err := json.Unmarshal(data, &m); err != nil || m.Bucket != bucket {
				continue
			}
			all = append(all, m)
		}
	}
	return all, nil
}

// abortUploadsOf removes every upload of a bucket (forced bucket deletion).
func (l *layer) abortUploadsOf(ctx context.Context, bucket string) {
	uploads, err := l.uploadsOf(ctx, bucket)
	if err != nil {
		l.deps.Logger.WarnContext(ctx, "object: cannot list uploads to abort", "bucket", bucket, "err", err)
		return
	}
	for _, m := range uploads {
		st := l.pick(m.Key)
		if err := st.DeletePath(ctx, domain.SystemBucket, uploadsRoot(bucket, m.Key)+"/"+m.UploadID, true); err != nil {
			l.deps.Logger.WarnContext(ctx, "object: cannot abort upload", "bucket", bucket, "upload", m.UploadID, "err", err)
		}
	}
}

// ListMultipartUploads implements Service.
func (l *layer) ListMultipartUploads(ctx context.Context, in domain.ListMultipartUploadsInput) (domain.ListMultipartUploadsResult, error) {
	if err := validBucket(in.Bucket); err != nil {
		return domain.ListMultipartUploadsResult{}, err
	}
	if err := domain.ValidateObjectPrefix(in.Prefix); err != nil {
		return domain.ListMultipartUploadsResult{}, err
	}
	if _, err := l.sets[0].StatVol(ctx, in.Bucket); err != nil {
		return domain.ListMultipartUploadsResult{}, fmt.Errorf("%s: list uploads of %s: %w", l.name, in.Bucket, err)
	}
	uploads, err := l.uploadsOf(ctx, in.Bucket)
	if err != nil {
		return domain.ListMultipartUploadsResult{}, err
	}
	sort.Slice(uploads, func(i, j int) bool {
		if uploads[i].Key != uploads[j].Key {
			return uploads[i].Key < uploads[j].Key
		}
		if !uploads[i].Initiated.Equal(uploads[j].Initiated) {
			return uploads[i].Initiated.Before(uploads[j].Initiated)
		}
		return uploads[i].UploadID < uploads[j].UploadID
	})
	maxUploads := in.MaxUploads
	if maxUploads <= 0 || maxUploads > maxListKeys {
		maxUploads = maxListKeys
	}
	res := domain.ListMultipartUploadsResult{}
	count := 0
	seenPrefix := map[string]bool{}
	for _, m := range uploads {
		if !strings.HasPrefix(m.Key, in.Prefix) {
			continue
		}
		if m.Key < in.KeyMarker || (m.Key == in.KeyMarker && (in.UploadIDMarker == "" || m.UploadID <= in.UploadIDMarker)) {
			continue
		}
		if in.Delimiter != "" {
			rest := m.Key[len(in.Prefix):]
			if i := strings.Index(rest, in.Delimiter); i >= 0 {
				cp := m.Key[:len(in.Prefix)+i+len(in.Delimiter)]
				if seenPrefix[cp] || cp <= in.KeyMarker {
					continue
				}
				if count == maxUploads {
					res.IsTruncated = true
					break
				}
				seenPrefix[cp] = true
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextKeyMarker, res.NextUploadIDMarker = cp, ""
				count++
				continue
			}
		}
		if count == maxUploads {
			res.IsTruncated = true
			break
		}
		res.Uploads = append(res.Uploads, m.toDomain())
		res.NextKeyMarker, res.NextUploadIDMarker = m.Key, m.UploadID
		count++
	}
	if !res.IsTruncated {
		res.NextKeyMarker, res.NextUploadIDMarker = "", ""
	}
	return res, nil
}

func partName(n int) string { return "part." + strconv.Itoa(n) }
