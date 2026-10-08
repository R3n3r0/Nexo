package object

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Multipart bookkeeping lives in the system volume, one directory per
// upload, in the store (erasure set) that will hold the object:
//
//	multipart/<bucket>/<uploadID>/upload.json      the upload description
//	multipart/<bucket>/<uploadID>/part.N.meta      the description of part N
//	multipart/<bucket>/<uploadID>/part.N.<id>      the data of part N
//
// The part description is the commit point of a part upload: it names the
// data file it describes, so concurrent uploads of the same part number
// never mix the data of one with the ETag of the other. Data files that
// no description names are removed with the upload.
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
	// Erasure is the layout the parts are written with, decided at the
	// creation of the upload (absent for uploads of earlier builds).
	Erasure *storage.ErasureInfo `json:"erasure,omitempty"`
}

// layout returns the erasure layout of the upload's parts in st.
func (m uploadMeta) layout(st store) storage.ErasureInfo {
	if m.Erasure != nil {
		return *m.Erasure
	}
	return st.ErasureInfo(m.Key, m.Metadata[metaStorageClass])
}

// partMeta is the persisted description of an uploaded part.
type partMeta struct {
	Number     int             `json:"number"`
	ETag       string          `json:"etag"`
	Size       int64           `json:"size"`
	ActualSize int64           `json:"actualSize"`
	ModTime    time.Time       `json:"modTime"`
	Checksum   domain.Checksum `json:"checksum,omitempty"`
	// DataFile is the name of the data file in the upload directory.
	DataFile string `json:"dataFile"`
}

func uploadsDir(bucket string) string { return storage.MultipartDir + "/" + bucket }

func uploadDir(bucket, uploadID string) string { return uploadsDir(bucket) + "/" + uploadID }

func partMetaName(n int) string { return partName(n) + metaSuffix }

func partName(n int) string { return "part." + strconv.Itoa(n) }

// validUploadID accepts the base64url ids produced by newUploadID (and any
// id of the same alphabet), so that an id never escapes its directory.
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

func noSuchUpload(uploadID string) error {
	return domain.NewS3Error(domain.ErrNoSuchUpload, "NoSuchUpload",
		"The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.").WithResource(uploadID)
}

// loadUpload reads the upload description or reports ErrNoSuchUpload.
func (l *layer) loadUpload(ctx context.Context, st store, bucket, key, uploadID string) (uploadMeta, string, error) {
	if !validUploadID(uploadID) {
		return uploadMeta{}, "", noSuchUpload(uploadID)
	}
	dir := uploadDir(bucket, uploadID)
	data, err := st.ReadAll(ctx, dir+"/"+uploadFile)
	if err != nil {
		if isNotFound(err) {
			if serr := l.statBucket(ctx, st, bucket); serr != nil {
				return uploadMeta{}, "", serr
			}
			return uploadMeta{}, "", noSuchUpload(uploadID)
		}
		return uploadMeta{}, "", fmt.Errorf("%s: read upload %s: %w", l.name, uploadID, err)
	}
	var m uploadMeta
	if err := json.Unmarshal(data, &m); err != nil || m.Bucket != bucket || m.Key != key || m.UploadID != uploadID {
		return uploadMeta{}, "", noSuchUpload(uploadID)
	}
	return m, dir, nil
}

// NewMultipartUpload implements Service.
func (l *layer) NewMultipartUpload(ctx context.Context, in NewMultipartInput) (domain.MultipartUpload, error) {
	if err := validObject(in.Bucket, in.Key); err != nil {
		return domain.MultipartUpload{}, err
	}
	if err := validateTags(in.Tags); err != nil {
		return domain.MultipartUpload{}, err
	}
	if _, err := newHashReader(nil, false, in.Checksum); err != nil {
		return domain.MultipartUpload{}, err
	}
	st, err := l.place.forWrite(ctx, in.Bucket, in.Key, -1)
	if err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: place %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	if err := l.statBucket(ctx, st, in.Bucket); err != nil {
		return domain.MultipartUpload{}, err
	}
	now := l.now()
	m := uploadMeta{
		Bucket:    in.Bucket,
		Key:       in.Key,
		UploadID:  l.newUploadID(now),
		Initiated: now,
		Metadata: httpMeta{
			contentType:        in.ContentType,
			contentEncoding:    in.ContentEncoding,
			contentDisposition: in.ContentDisposition,
			contentLanguage:    in.ContentLanguage,
			cacheControl:       in.CacheControl,
			expires:            in.Expires,
			user:               in.UserMetadata,
		}.build(in.Tags, in.StorageClass),
		Checksum: in.Checksum,
	}
	ei := st.ErasureInfo(in.Key, m.Metadata[metaStorageClass])
	m.Erasure = &ei
	data, err := json.Marshal(m)
	if err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: encode upload: %w", l.name, err)
	}
	if err := st.WriteAll(ctx, uploadDir(in.Bucket, m.UploadID)+"/"+uploadFile, data); err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: new upload %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	return m.toDomain(), nil
}

// PutObjectPart implements Service.
func (l *layer) PutObjectPart(ctx context.Context, in PutObjectPartInput) (domain.Part, error) {
	if err := validObject(in.Bucket, in.Key); err != nil {
		return domain.Part{}, err
	}
	if in.PartNumber < 1 || in.PartNumber > maxPartNumber {
		return domain.Part{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive")
	}
	if err := validSize(in.Size); err != nil {
		return domain.Part{}, err
	}
	if !validUploadID(in.UploadID) {
		return domain.Part{}, noSuchUpload(in.UploadID)
	}
	lc, err := l.lockUpload(ctx, in.Bucket, in.UploadID, false)
	if err != nil {
		return domain.Part{}, err
	}
	defer lc.Unlock(ctx)
	st, err := l.place.forUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.Part{}, fmt.Errorf("%s: upload %s: %w", l.name, in.UploadID, err)
	}
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
	// The part is written to a private temporary directory, verified, then
	// moved under a unique name into the upload directory.
	tmpDir := l.newTmpDir()
	defer l.cleanupTmp(ctx, st, tmpDir)
	n, err := st.WriteData(ctx, tmpDir, in.PartNumber, in.Size, hr, m.layout(st))
	if err != nil {
		return domain.Part{}, fmt.Errorf("%s: upload part %d of %s: %w", l.name, in.PartNumber, in.UploadID, mapBodyErr(err))
	}
	if in.Size >= 0 && n != in.Size {
		return domain.Part{}, incompleteBody(in.Size, n)
	}
	checksum, err := hr.verify(in.MD5Hex, in.SHA256Hex, in.Checksum)
	if err != nil {
		return domain.Part{}, err
	}
	part := partMeta{
		Number: in.PartNumber, ETag: hr.etag(), Size: n, ActualSize: n, ModTime: l.now(), Checksum: checksum,
		DataFile: partName(in.PartNumber) + "." + uuid.NewString(),
	}
	// Concurrent uploads of the same part number commit one at a time: on
	// an erasure set their descriptions would otherwise reach the drives
	// in different orders and no version of it would have a quorum.
	plc, err := l.lockPath(ctx, domain.SystemBucket, dir+"/"+partName(in.PartNumber), true)
	if err != nil {
		return domain.Part{}, err
	}
	defer plc.Unlock(ctx)
	if err := st.RenameFile(ctx, tmpDir+"/"+partName(in.PartNumber), dir+"/"+part.DataFile); err != nil {
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	previous, hadPrevious := l.readPart(ctx, st, dir, in.PartNumber)
	data, err := json.Marshal(part)
	if err != nil {
		return domain.Part{}, fmt.Errorf("%s: encode part: %w", l.name, err)
	}
	if err := st.WriteAll(ctx, dir+"/"+partMetaName(in.PartNumber), data); err != nil {
		l.removeUploadFile(ctx, st, dir, part.DataFile)
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	if hadPrevious && previous.DataFile != part.DataFile {
		l.removeUploadFile(ctx, st, dir, previous.DataFile)
	}
	return part.toDomain(), nil
}

// readPart reads the description of part n, if any.
func (l *layer) readPart(ctx context.Context, st store, dir string, n int) (partMeta, bool) {
	data, err := st.ReadAll(ctx, dir+"/"+partMetaName(n))
	if err != nil {
		return partMeta{}, false
	}
	var p partMeta
	if err := json.Unmarshal(data, &p); err != nil {
		return partMeta{}, false
	}
	if p.DataFile == "" {
		p.DataFile = partName(p.Number)
	}
	return p, true
}

// removeUploadFile removes one file of an upload directory (best effort).
func (l *layer) removeUploadFile(ctx context.Context, st store, dir, name string) {
	if name == "" || strings.Contains(name, "/") {
		return
	}
	// The part is already replaced: remove its data even when the client
	// went away.
	ctx = context.WithoutCancel(ctx)
	if err := st.DeletePath(ctx, domain.SystemBucket, dir+"/"+name, false); err != nil && !isNotFound(err) {
		l.deps.Logger.DebugContext(ctx, "object: cannot remove replaced part", "path", dir+"/"+name, "err", err)
	}
}

// CopyObjectPart implements Service.
func (l *layer) CopyObjectPart(ctx context.Context, in CopyObjectPartInput) (domain.Part, error) {
	if err := validObject(in.SrcBucket, in.SrcKey); err != nil {
		return domain.Part{}, err
	}
	if err := validObject(in.DstBucket, in.DstKey); err != nil {
		return domain.Part{}, err
	}
	lc, err := l.lock(ctx, in.SrcBucket, in.SrcKey, false)
	if err != nil {
		return domain.Part{}, err
	}
	defer lc.Unlock(ctx)
	info, err := l.GetObjectInfo(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, NoLock: true})
	if err != nil {
		return domain.Part{}, err
	}
	if err := checkSourceConditions(info, in.SrcConditions); err != nil {
		return domain.Part{}, err
	}
	if r := in.SrcRange; r != nil && (r.IsSuffix || r.End < 0 || r.End >= info.Size) {
		return domain.Part{}, domain.NewS3Error(domain.ErrInvalidRange, "InvalidRange",
			fmt.Sprintf("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy, within the %d bytes of the source", info.Size))
	}
	src, _, err := l.openObject(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, Range: in.SrcRange}, nil)
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
			if isNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("%s: read part %s: %w", l.name, info.Name, err)
		}
		var p partMeta
		if err := json.Unmarshal(data, &p); err != nil {
			l.deps.Logger.WarnContext(ctx, "object: skipping unreadable part description", "path", info.Name, "err", err)
			continue
		}
		if p.DataFile == "" {
			p.DataFile = partName(p.Number)
		}
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// ListObjectParts implements Service.
func (l *layer) ListObjectParts(ctx context.Context, in domain.ListPartsInput) (domain.ListPartsResult, error) {
	if err := validObject(in.Bucket, in.Key); err != nil {
		return domain.ListPartsResult{}, err
	}
	st, err := l.place.forUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ListPartsResult{}, fmt.Errorf("%s: upload %s: %w", l.name, in.UploadID, err)
	}
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ListPartsResult{}, err
	}
	maxParts := clampMaxKeys(in.MaxParts)
	res := domain.ListPartsResult{Upload: m.toDomain()}
	if maxParts == 0 {
		return res, nil
	}
	parts, err := l.listParts(ctx, st, dir)
	if err != nil {
		return domain.ListPartsResult{}, err
	}
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
	if err := validObject(bucket, key); err != nil {
		return err
	}
	if !validUploadID(uploadID) {
		return noSuchUpload(uploadID)
	}
	lc, err := l.lockUpload(ctx, bucket, uploadID, true)
	if err != nil {
		return err
	}
	defer lc.Unlock(ctx)
	st, err := l.place.forUpload(ctx, bucket, key, uploadID)
	if err != nil {
		return fmt.Errorf("%s: upload %s: %w", l.name, uploadID, err)
	}
	if _, _, err := l.loadUpload(ctx, st, bucket, key, uploadID); err != nil {
		return err
	}
	if err := st.DeletePath(ctx, domain.SystemBucket, uploadDir(bucket, uploadID), true); err != nil && !isNotFound(err) {
		return fmt.Errorf("%s: abort upload %s: %w", l.name, uploadID, err)
	}
	return nil
}

// CompleteMultipartUpload implements Service. The selected parts are moved,
// not copied, into the data directory of the new version: completion costs
// one rename per part whatever the object size.
func (l *layer) CompleteMultipartUpload(ctx context.Context, in CompleteMultipartInput) (domain.ObjectInfo, error) {
	if err := validObject(in.Bucket, in.Key); err != nil {
		return domain.ObjectInfo{}, err
	}
	if len(in.Parts) == 0 {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: no parts")
	}
	if len(in.Parts) > maxPartNumber {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema: too many parts")
	}
	for i := 1; i < len(in.Parts); i++ {
		if in.Parts[i].Number <= in.Parts[i-1].Number {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPartOrder, "InvalidPartOrder",
				fmt.Sprintf("The list of parts was not in ascending order: part %d after part %d.", in.Parts[i].Number, in.Parts[i-1].Number))
		}
	}
	if !validUploadID(in.UploadID) {
		return domain.ObjectInfo{}, noSuchUpload(in.UploadID)
	}
	unlock, err := l.lockWrite(ctx, in.Bucket, in.Key)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	ulc, err := l.lockUpload(ctx, in.Bucket, in.UploadID, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer ulc.Unlock(ctx)
	st, err := l.place.forUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: upload %s: %w", l.name, in.UploadID, err)
	}
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
		total    int64
		etagSum  = md5.New()
		parts    = make([]storage.PartInfo, 0, len(in.Parts))
		selected = make([]partMeta, 0, len(in.Parts))
	)
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
		if !cp.Checksum.IsZero() && (cp.Checksum.Algorithm != p.Checksum.Algorithm || cp.Checksum.Value != p.Checksum.Value) {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. The %s checksum of part %d does not match the checksum of the uploaded part.", cp.Checksum.Algorithm, cp.Number))
		}
		raw, err := hex.DecodeString(p.ETag)
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%w: part %d has a malformed etag", domain.ErrInvalidPart, cp.Number)
		}
		_, _ = etagSum.Write(raw)
		total += p.Size
		selected = append(selected, p)
		parts = append(parts, storage.PartInfo{Number: i + 1, ETag: p.ETag, Size: p.Size, ActualSize: p.ActualSize, ModTime: p.ModTime, Checksums: checksumMap(p.Checksum)})
	}
	partSums := make([]domain.Checksum, len(selected))
	for i, p := range selected {
		partSums[i] = p.Checksum
	}
	checksum, err := multipartChecksum(m.Checksum, partSums, in.Checksum)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if err := l.checkCurrent(ctx, in.Bucket, in.Key, in.Conditions); err != nil {
		return domain.ObjectInfo{}, err
	}
	cfg, err := l.bucketConfig(ctx, in.Bucket)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if cfg.QuotaBytes > 0 && uint64(total) > cfg.QuotaBytes {
		return domain.ObjectInfo{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, total, cfg.QuotaBytes)
	}

	// From the first part moved on, the completion runs to its end (commit,
	// or parts moved back into the upload) even when the client goes away:
	// a canceled context would leave the parts in the temporary directory,
	// which is then purged, and the upload unusable.
	ctx = context.WithoutCancel(ctx)
	// Assemble the selected parts, renumbered, in a fresh temporary directory.
	tmpDir := l.newTmpDir()
	moved := 0
	restore := func() {
		for i := 0; i < moved; i++ {
			if err := st.RenameFile(ctx, tmpDir+"/"+partName(i+1), dir+"/"+selected[i].DataFile); err != nil {
				l.deps.Logger.WarnContext(ctx, "object: cannot restore part after a failed completion", "upload", in.UploadID, "part", selected[i].Number, "err", err)
			}
		}
		l.cleanupTmp(ctx, st, tmpDir)
	}
	for i, p := range selected {
		if err := st.RenameFile(ctx, dir+"/"+p.DataFile, tmpDir+"/"+partName(i+1)); err != nil {
			restore()
			if isNotFound(err) {
				return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
					fmt.Sprintf("One or more of the specified parts could not be found. The data of part %d is missing.", p.Number))
			}
			return domain.ObjectInfo{}, fmt.Errorf("%s: assemble part %d of %s: %w", l.name, p.Number, in.UploadID, err)
		}
		moved++
	}
	versionID, versioned := l.versionFor(cfg, "")
	meta := make(map[string]string, len(m.Metadata)+2)
	for k, v := range m.Metadata {
		meta[k] = v
	}
	meta[metaETag] = hex.EncodeToString(etagSum.Sum(nil)) + "-" + strconv.Itoa(len(parts))
	if !checksum.IsZero() {
		meta[metaChecksumPrefix+strings.ToLower(string(checksum.Algorithm))] = checksum.Value
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
		Erasure:   m.layout(st),
		Versioned: versioned,
	}
	old, err := st.CommitData(ctx, tmpDir, fi, in.Bucket, in.Key)
	if err != nil {
		restore()
		return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
	}
	l.purgeReplaced(ctx, st, in.Bucket, in.Key, old, fi.DataDir)
	l.dropElsewhere(ctx, st, in.Bucket, in.Key, fi.VersionID)
	if err := st.DeletePath(ctx, domain.SystemBucket, dir, true); err != nil && !isNotFound(err) {
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
		infos, err := st.StatGlob(ctx, uploadsDir(bucket)+"/*/"+uploadFile)
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

// uploadBefore orders uploads by key, initiation time and id.
func uploadBefore(a, b uploadMeta) bool {
	if a.Key != b.Key {
		return a.Key < b.Key
	}
	if !a.Initiated.Equal(b.Initiated) {
		return a.Initiated.Before(b.Initiated)
	}
	return a.UploadID < b.UploadID
}

// ListMultipartUploads implements Service. Uploads are listed by key, then
// by initiation time; upload-id-marker resumes after the given upload of
// key-marker and is ignored without a key-marker, as in S3.
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
	maxUploads := clampMaxKeys(in.MaxUploads)
	if maxUploads == 0 {
		return domain.ListMultipartUploadsResult{}, nil
	}
	uploads, err := l.uploadsOf(ctx, in.Bucket)
	if err != nil {
		return domain.ListMultipartUploadsResult{}, err
	}
	sort.Slice(uploads, func(i, j int) bool { return uploadBefore(uploads[i], uploads[j]) })

	// The marker upload, positioned by the time its id carries.
	var marker *uploadMeta
	if in.KeyMarker != "" && in.UploadIDMarker != "" {
		if at, ok := uploadIDTime(in.UploadIDMarker); ok {
			marker = &uploadMeta{Key: in.KeyMarker, UploadID: in.UploadIDMarker, Initiated: at}
		}
	}
	after := func(m uploadMeta) bool {
		switch {
		case in.KeyMarker == "":
			return true
		case m.Key != in.KeyMarker:
			return m.Key > in.KeyMarker
		case marker == nil:
			return false
		default:
			return uploadBefore(*marker, m)
		}
	}

	res := domain.ListMultipartUploadsResult{}
	count := 0
	seenPrefix := map[string]bool{}
	for _, m := range uploads {
		if !strings.HasPrefix(m.Key, in.Prefix) || !after(m) {
			continue
		}
		if in.Delimiter != "" {
			rest := m.Key[len(in.Prefix):]
			if i := strings.Index(rest, in.Delimiter); i >= 0 {
				cp := m.Key[:len(in.Prefix)+i+len(in.Delimiter)]
				// Same rule as ListObjects: a prefix holding the marker
				// ("a/" with key-marker "a/x") still rolls up the uploads
				// after it.
				if seenPrefix[cp] || (in.KeyMarker != "" && skipPrefix(cp, in.KeyMarker)) {
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

// CleanupStaleUploads implements Service.
func (l *layer) CleanupStaleUploads(ctx context.Context, bucket string, olderThan time.Duration) (int, error) {
	if olderThan < 0 {
		return 0, fmt.Errorf("%w: negative upload age %s", domain.ErrInvalidArgument, olderThan)
	}
	pattern := storage.MultipartDir + "/*/*"
	if bucket != "" {
		if err := validBucket(bucket); err != nil {
			return 0, err
		}
		pattern = uploadsDir(bucket) + "/*"
	}
	cutoff := l.now().Add(-olderThan)
	removed := 0
	for _, st := range l.sets {
		dirs, err := st.StatGlob(ctx, pattern)
		if err != nil {
			return removed, fmt.Errorf("%s: list uploads: %w", l.name, err)
		}
		for _, d := range dirs {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if !d.Dir {
				continue
			}
			ok, err := l.cleanupUpload(ctx, st, d, cutoff)
			if err != nil {
				return removed, err
			}
			if ok {
				removed++
			}
		}
	}
	return removed, nil
}

// cleanupUpload removes one upload directory when it is older than cutoff;
// it reports whether a valid upload was aborted.
func (l *layer) cleanupUpload(ctx context.Context, st store, d storage.StatInfo, cutoff time.Time) (bool, error) {
	bucket, uploadID := path.Base(path.Dir(d.Name)), path.Base(d.Name)
	data, err := st.ReadAll(ctx, d.Name+"/"+uploadFile)
	var m uploadMeta
	if err != nil || json.Unmarshal(data, &m) != nil || m.UploadID != uploadID {
		// An orphaned directory (no readable description): age by mtime.
		if !d.ModTime.Before(cutoff) {
			return false, nil
		}
		if err := st.DeletePath(ctx, domain.SystemBucket, d.Name, true); err != nil && !isNotFound(err) {
			return false, fmt.Errorf("%s: remove orphaned upload %s: %w", l.name, d.Name, err)
		}
		return false, nil
	}
	if !m.Initiated.Before(cutoff) {
		return false, nil
	}
	lc, err := l.lockUpload(ctx, bucket, uploadID, true)
	if err != nil {
		return false, err
	}
	defer lc.Unlock(ctx)
	if err := st.DeletePath(ctx, domain.SystemBucket, d.Name, true); err != nil {
		if isNotFound(err) || errors.Is(err, domain.ErrBucketNotFound) {
			return false, nil // completed or aborted meanwhile
		}
		return false, fmt.Errorf("%s: abort stale upload %s: %w", l.name, uploadID, err)
	}
	l.deps.Logger.InfoContext(ctx, "object: aborted stale multipart upload", "bucket", m.Bucket, "key", m.Key, "upload", uploadID, "initiated", m.Initiated)
	return true, nil
}
