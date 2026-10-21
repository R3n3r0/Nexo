package object

import (
	"context"
	"crypto/md5" // #nosec G501 -- the S3 multipart ETag
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
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
//
// A completion assembles the selected parts in a staging directory of the
// upload and records the moves first in a journal:
//
//	multipart/<bucket>/<uploadID>/completing.json   the journal
//	multipart/<bucket>/<uploadID>/completing.<dataDir>/part.K
//
// Nothing of an upload is ever in the temporary area emptied at startup:
// a completion interrupted by a crash leaves its journal, and the next
// completion attempt either finds the version committed (and finishes the
// cleanup) or moves the parts back before starting over.
const (
	uploadFile     = "upload.json"
	metaSuffix     = ".meta"
	journalFile    = "completing.json"
	stagingPrefix  = "completing."
	maxJournalSize = 4 << 20
)

// completionJournal is the persisted record of a completion in progress.
type completionJournal struct {
	// Staging is the staging directory (system path).
	Staging string `json:"staging"`
	// DataDir and VersionID identify the version the completion commits.
	DataDir   string `json:"dataDir"`
	VersionID string `json:"versionId,omitempty"`
	// Moves are the moves of the part data files into Staging.
	Moves []journalMove `json:"moves"`
}

// journalMove is one move of a completion journal (system paths).
type journalMove struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
}

// uploadMeta is the persisted description of an in-progress upload.
type uploadMeta struct {
	Bucket    string                   `json:"bucket"`
	Key       string                   `json:"key"`
	UploadID  string                   `json:"uploadId"`
	Initiated time.Time                `json:"initiated"`
	Metadata  map[string]string        `json:"metadata"`
	Checksum  domain.ChecksumAlgorithm `json:"checksum,omitempty"`
	// ChecksumType is the checksum type of the completed object (empty
	// for uploads of earlier builds: see checksumType).
	ChecksumType domain.ChecksumType `json:"checksumType,omitempty"`
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
	// Encrypted reports a part stored encrypted: Size is the size of the
	// ciphertext, ActualSize the one of the plaintext; Checksum.Value is
	// then sealed under the object key of the upload (sealPartChecksum).
	Encrypted bool `json:"encrypted,omitempty"`
	// Salt is the salt of the key the part was encrypted with
	// (crypto.NewPartSalt; absent for the parts of earlier builds).
	Salt []byte `json:"salt,omitempty"`
	// Compressed reports a part stored compressed (S-OBJ-14): Size is the
	// size of the S2 stream, ActualSize the one of the content, Index the
	// part index of its blocks (encodeCompressionIndex).
	Compressed bool   `json:"compressed,omitempty"`
	Index      []byte `json:"index,omitempty"`
}

// plainSize returns the size of the content of the part (the plaintext
// of an encrypted part).
func (p partMeta) plainSize() int64 {
	if p.Encrypted || p.Compressed || p.ActualSize > 0 {
		return p.ActualSize
	}
	return p.Size
}

func uploadsDir(bucket string) string { return storage.MultipartDir + "/" + bucket }

func uploadDir(bucket, uploadID string) string { return uploadsDir(bucket) + "/" + uploadID }

func partMetaName(n int) string { return partName(n) + metaSuffix }

// partPrefix starts the name of every part file.
const partPrefix = "part."

func partName(n int) string { return partPrefix + strconv.Itoa(n) }

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
		ChecksumType: m.checksumType(),
		Encryption:   crypto.InfoOf(m.Metadata),
	}
}

// checksumType returns the checksum type of the completed object: the
// recorded one, or the default of the algorithm for the uploads created
// before the type was recorded.
func (m uploadMeta) checksumType() domain.ChecksumType {
	if m.ChecksumType != domain.ChecksumTypeNone || m.Checksum == domain.ChecksumNone {
		return m.ChecksumType
	}
	typ, err := uploadChecksumType(m.Checksum, domain.ChecksumTypeNone)
	if err != nil {
		return domain.ChecksumTypeComposite
	}
	return typ
}

func (p partMeta) toDomain() domain.Part {
	return domain.Part{Number: p.Number, ETag: p.ETag, Size: p.plainSize(), ModTime: p.ModTime, ActualSize: p.plainSize(), Checksum: p.Checksum}
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
	checksumType, err := uploadChecksumType(in.Checksum, in.ChecksumType)
	if err != nil {
		return domain.MultipartUpload{}, err
	}
	st, err := l.place.forWrite(ctx, in.Bucket, in.Key, -1)
	if err != nil {
		return domain.MultipartUpload{}, fmt.Errorf("%s: place %s/%s: %w", l.name, in.Bucket, in.Key, err)
	}
	if err := l.statBucket(ctx, st, in.Bucket); err != nil {
		return domain.MultipartUpload{}, err
	}
	cfg, err := l.bucketConfig(ctx, in.Bucket)
	if err != nil {
		return domain.MultipartUpload{}, err
	}
	if err := checkLockRequest(cfg, in.Retention, in.LegalHold, l.now()); err != nil {
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
		Checksum:     in.Checksum,
		ChecksumType: checksumType,
	}
	setRetentionMeta(m.Metadata, in.Retention)
	setLegalHoldMeta(m.Metadata, in.LegalHold)
	// The object key of an encrypted upload is sealed once, in the upload
	// description: every part is encrypted with a key derived from it.
	okey, err := l.sealFor(ctx, cfg, in.Bucket, in.Key, in.SSE)
	if err != nil {
		return domain.MultipartUpload{}, err
	}
	if okey != nil {
		for k, v := range okey.Metadata() {
			m.Metadata[k] = v
		}
	}
	// Whether the parts are compressed is decided once, for the whole
	// upload.
	if l.compressFor(ctx, in.Bucket, in.Key, in.ContentType, -1, okey != nil) {
		m.Metadata[metaCompression] = compressionS2
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
	up := m.toDomain()
	if okey != nil {
		up.Encryption = okey.Info()
	}
	return up, nil
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
	ctx, unlock, err := l.lockUpload(ctx, in.Bucket, in.UploadID, false)
	if err != nil {
		return domain.Part{}, err
	}
	defer unlock()
	st, err := l.place.forUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.Part{}, fmt.Errorf("%s: upload %s: %w", l.name, in.UploadID, err)
	}
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.Part{}, err
	}
	algo := in.Checksum.Algorithm
	switch {
	case algo == domain.ChecksumNone:
		algo = m.Checksum
	case m.Checksum != domain.ChecksumNone && algo != m.Checksum:
		return domain.Part{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("Checksum Type mismatch occurred, expected checksum Type: %s, actual checksum Type: %s",
				strings.ToLower(string(m.Checksum)), strings.ToLower(string(algo))))
	}
	// The parts of an encrypted upload are encrypted with the key of the
	// upload (an SSE-C upload needs its key with every part).
	okey, err := l.unsealFor(ctx, in.Bucket, in.Key, m.Metadata, in.SSECustomerKey)
	if err != nil {
		return domain.Part{}, err
	}
	body := in.Reader
	if body == nil {
		body = strings.NewReader("")
	}
	hr, err := newHashReader(body, in.SHA256Hex != "", algo)
	if err != nil {
		return domain.Part{}, err
	}
	payload, storedSize := io.Reader(hr), in.Size
	var salt []byte
	if okey != nil {
		plain := io.Reader(hr)
		if in.Size >= 0 {
			plain = io.LimitReader(hr, in.Size)
		}
		// Every upload of a part has its own key: a part number uploaded
		// again (a retry, UploadPartCopy over UploadPart) never reuses the
		// keystream of the previous upload under the key of the upload.
		if salt, err = crypto.NewPartSalt(); err != nil {
			return domain.Part{}, err
		}
		if payload, err = okey.EncryptPart(in.PartNumber, salt, plain); err != nil {
			return domain.Part{}, err
		}
		storedSize = crypto.EncryptedSize(in.Size)
	}
	var comp *compressor
	if okey == nil && isCompressed(m.Metadata) {
		plain := io.Reader(hr)
		if in.Size >= 0 {
			plain = io.LimitReader(hr, in.Size)
		}
		comp = newCompressor(plain)
		payload, storedSize = comp, -1
	}
	// The part is written to a private temporary directory, verified, then
	// moved under a unique name into the upload directory.
	tmpDir := l.newTmpDir()
	defer l.cleanupTmp(ctx, st, tmpDir)
	n, err := st.WriteData(ctx, tmpDir, in.PartNumber, storedSize, payload, m.layout(st))
	if err != nil {
		if comp != nil {
			comp.abort()
		}
		return domain.Part{}, fmt.Errorf("%s: upload part %d of %s: %w", l.name, in.PartNumber, in.UploadID, mapBodyErr(err))
	}
	var compIndex []byte
	if comp != nil {
		if compIndex, err = comp.finish(); err != nil {
			return domain.Part{}, fmt.Errorf("%s: upload part %d of %s: %w", l.name, in.PartNumber, in.UploadID, mapBodyErr(err))
		}
	}
	plainN := n
	if okey != nil || comp != nil {
		plainN = hr.n
	}
	if in.Size >= 0 && plainN != in.Size {
		return domain.Part{}, incompleteBody(in.Size, plainN)
	}
	checksum, err := hr.verify(in.MD5Hex, in.SHA256Hex, in.Checksum)
	if err != nil {
		return domain.Part{}, err
	}
	etag := hr.etag()
	if okey != nil {
		etag = okey.ETag(etag)
	}
	part := partMeta{
		Number: in.PartNumber, ETag: etag, Size: n, ActualSize: plainN, ModTime: l.now(), Checksum: checksum,
		DataFile: partName(in.PartNumber) + "." + uuid.NewString(), Encrypted: okey != nil, Salt: salt,
		Compressed: comp != nil, Index: compIndex,
	}
	// The description on the drives holds the checksum of an encrypted
	// part sealed; the response reports it in clear.
	stored := part
	if stored.Checksum, err = sealPartChecksum(okey, checksum); err != nil {
		return domain.Part{}, err
	}
	// Concurrent uploads of the same part number commit one at a time: on
	// an erasure set their descriptions would otherwise reach the drives
	// in different orders and no version of it would have a quorum.
	ctx, unlockPart, err := l.lockAll(ctx, []lockRequest{{bucket: domain.SystemBucket, key: dir + "/" + partName(in.PartNumber), write: true}})
	if err != nil {
		return domain.Part{}, err
	}
	defer unlockPart()
	// The data moves with the quorum of the layout of the upload: a part
	// acknowledged on fewer drives than its decoding needs would make the
	// completed object unreadable.
	move := []fileMove{{src: tmpDir + "/" + partName(in.PartNumber), dst: dir + "/" + part.DataFile}}
	if err := st.MoveFiles(ctx, move, m.layout(st)); err != nil {
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	previous, hadPrevious := l.readPart(ctx, st, dir, in.PartNumber)
	data, err := json.Marshal(stored)
	if err != nil {
		l.removeUploadFile(ctx, st, dir, part.DataFile)
		return domain.Part{}, fmt.Errorf("%s: encode part: %w", l.name, err)
	}
	if err := checkLocks(ctx); err != nil {
		l.removeUploadFile(ctx, st, dir, part.DataFile)
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	if err := st.WriteAll(ctx, dir+"/"+partMetaName(in.PartNumber), data); err != nil {
		l.removeUploadFile(ctx, st, dir, part.DataFile)
		return domain.Part{}, fmt.Errorf("%s: store part %d of %s: %w", l.name, in.PartNumber, in.UploadID, err)
	}
	if hadPrevious && previous.DataFile != part.DataFile {
		l.removeUploadFile(ctx, st, dir, previous.DataFile)
	}
	out := part.toDomain()
	if okey != nil {
		out.Encryption = okey.Info()
	}
	return out, nil
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
	ctx, unlock, err := l.lock(ctx, in.SrcBucket, in.SrcKey, false)
	if err != nil {
		return domain.Part{}, err
	}
	defer unlock()
	info, err := l.GetObjectInfo(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, NoLock: true, SSECustomerKey: in.SrcSSECustomerKey})
	if err != nil {
		return domain.Part{}, copySourceErr(err)
	}
	if err := checkSourceConditions(info, in.SrcConditions); err != nil {
		return domain.Part{}, err
	}
	if r := in.SrcRange; r != nil && (r.IsSuffix || r.End < 0 || r.End >= info.Size) {
		return domain.Part{}, domain.NewS3Error(domain.ErrInvalidRange, "InvalidRange",
			fmt.Sprintf("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy, within the %d bytes of the source", info.Size))
	}
	src, _, err := l.openObject(ctx, in.SrcBucket, in.SrcKey, GetObjectOptions{VersionID: in.SrcVersionID, Range: in.SrcRange, SSECustomerKey: in.SrcSSECustomerKey}, nil)
	if err != nil {
		return domain.Part{}, err
	}
	defer func() { _ = src.Close() }()
	return l.PutObjectPart(ctx, PutObjectPartInput{
		Bucket: in.DstBucket, Key: in.DstKey, UploadID: in.UploadID, PartNumber: in.PartNumber,
		Reader: src, Size: src.Length, SSECustomerKey: in.SSECustomerKey,
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
	// The checksums of the parts of an encrypted upload are sealed: they
	// are opened with its key (a KMS call for SSE-S3 and SSE-KMS); an
	// SSE-C upload listed without its key reports their algorithms only.
	sealed := false
	for _, p := range parts {
		sealed = sealed || crypto.IsSealedValue(p.Checksum.Value)
	}
	var okey *crypto.ObjectKey
	if !in.SSECustomerKey.IsZero() || (sealed && crypto.InfoOf(m.Metadata).Type != domain.SSEC) {
		if okey, err = l.unsealFor(ctx, in.Bucket, in.Key, m.Metadata, in.SSECustomerKey); err != nil {
			return domain.ListPartsResult{}, err
		}
	}
	for i := range parts {
		if parts[i].Checksum, err = openPartChecksum(okey, parts[i].Checksum); err != nil {
			return domain.ListPartsResult{}, fmt.Errorf("%s: checksum of part %d of %s: %w", l.name, parts[i].Number, in.UploadID, err)
		}
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
	ctx, unlock, err := l.lockUpload(ctx, bucket, uploadID, true)
	if err != nil {
		return err
	}
	defer unlock()
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
	// The key is locked before the upload (as CopyObjectPart locks its
	// source before the upload), so that the two never deadlock.
	ctx, unlock, err := l.lockWrite(ctx, in.Bucket, in.Key)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlock()
	ctx, unlockUpload, err := l.lockUpload(ctx, in.Bucket, in.UploadID, true)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	defer unlockUpload()
	st, err := l.place.forUpload(ctx, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: upload %s: %w", l.name, in.UploadID, err)
	}
	m, dir, err := l.loadUpload(ctx, st, in.Bucket, in.Key, in.UploadID)
	if err != nil {
		if errors.Is(err, domain.ErrNoSuchUpload) {
			if info, ok := l.completedFrom(ctx, in); ok {
				return info, nil
			}
		}
		return domain.ObjectInfo{}, err
	}
	if done, err := l.recoverCompletion(ctx, st, dir, in.Bucket, in.Key); err != nil || done != nil {
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		return *done, nil
	}
	uploaded, err := l.listParts(ctx, st, dir)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	byNumber := make(map[int]partMeta, len(uploaded))
	for _, p := range uploaded {
		byNumber[p.Number] = p
	}

	encrypted := crypto.IsEncrypted(m.Metadata)
	compressed := isCompressed(m.Metadata)
	// The checksums of the parts of an encrypted upload are sealed: they
	// are opened with the key of the upload when the completion needs them
	// (the checksum of the object, a part checksum or a whole checksum to
	// verify), which takes the SSE-C key for an SSE-C upload, as in S3.
	// Otherwise they move to the version as they are.
	var okey *crypto.ObjectKey
	if encrypted && (completionNeedsChecksums(m, in) || !in.SSECustomerKey.IsZero()) {
		if okey, err = l.unsealFor(ctx, in.Bucket, in.Key, m.Metadata, in.SSECustomerKey); err != nil {
			return domain.ObjectInfo{}, err
		}
	} else if !encrypted && !in.SSECustomerKey.IsZero() {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			"The encryption parameters are not applicable to this object.")
	}
	var (
		total      int64
		plainTotal int64
		etagSum    = md5.New() // #nosec G401 -- the S3 multipart ETag (MD5 of the part MD5s), not a security primitive
		parts      = make([]storage.PartInfo, 0, len(in.Parts))
		selected   = make([]partMeta, 0, len(in.Parts))
		uploadType = m.checksumType()
		// missingChecksum is the first part listed without its checksum
		// in a COMPOSITE upload with a checksum (0: none).
		missingChecksum int
	)
	for i, cp := range in.Parts {
		p, ok := byNumber[cp.Number]
		if !ok || !strings.EqualFold(strings.Trim(cp.ETag, `"`), p.ETag) {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. Part %d may not have been uploaded, or the specified entity tag may not match the part's entity tag.", cp.Number))
		}
		if missingChecksum == 0 && uploadType == domain.ChecksumTypeComposite && m.Checksum != domain.ChecksumNone && cp.Checksum.IsZero() {
			// As S3: the checksum of a COMPOSITE upload is the checksum of
			// the part checksums, which the request must confirm. Reported
			// after the verification of a declared object checksum, so
			// that a wrong one is a BadDigest.
			missingChecksum = cp.Number
		}
		if i < len(in.Parts)-1 && p.plainSize() < l.opts.minPartSize {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrEntityTooSmall, "EntityTooSmall",
				fmt.Sprintf("Your proposed upload is smaller than the minimum allowed object size. Part %d is %d bytes, the minimum is %d.", cp.Number, p.plainSize(), l.opts.minPartSize))
		}
		if p.Encrypted != encrypted {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. Part %d was not stored with the encryption of the upload.", cp.Number))
		}
		if p.Compressed != compressed {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. Part %d was not stored with the compression of the upload.", cp.Number))
		}
		sum, err := openPartChecksum(okey, p.Checksum)
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%s: checksum of part %d of %s: %w", l.name, cp.Number, in.UploadID, err)
		}
		if !cp.Checksum.IsZero() && (cp.Checksum.Algorithm != sum.Algorithm || cp.Checksum.Value != sum.Value) {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				fmt.Sprintf("One or more of the specified parts could not be found. The %s checksum of part %d does not match the checksum of the uploaded part.", cp.Checksum.Algorithm, cp.Number))
		}
		raw, err := hex.DecodeString(p.ETag)
		if err != nil {
			return domain.ObjectInfo{}, fmt.Errorf("%w: part %d has a malformed etag", domain.ErrInvalidPart, cp.Number)
		}
		_, _ = etagSum.Write(raw)
		total += p.Size
		plainTotal += p.plainSize()
		selected = append(selected, p)
		info := storage.PartInfo{Number: i + 1, ETag: p.ETag, Size: p.Size, ActualSize: p.plainSize(), ModTime: p.ModTime, Checksums: checksumMap(p.Checksum)}
		if encrypted {
			// The part keeps the key of its number and salt in the upload.
			info.Index = encodePartIndex(p.Number, p.Salt)
		}
		if compressed {
			info.Index = p.Index
		}
		parts = append(parts, info)
		p.Checksum = sum
		selected[len(selected)-1] = p
	}
	if in.ObjectSize != nil && *in.ObjectSize != plainTotal {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("The provided x-amz-mp-object-size header value %d does not match the size of the object, %d bytes.", *in.ObjectSize, plainTotal))
	}
	if in.ChecksumType != domain.ChecksumTypeNone && in.ChecksumType != uploadType {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("The upload was created using the %s checksum mode. The complete request must use the same checksum mode.", uploadType))
	}
	partSums := make([]partSum, len(selected))
	for i, p := range selected {
		partSums[i] = partSum{sum: p.Checksum, size: p.plainSize()}
	}
	checksum, err := multipartChecksum(m.Checksum, uploadType, partSums, in.Checksum)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if missingChecksum != 0 {
		return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest",
			fmt.Sprintf("The upload was created using a %s checksum. The complete request must include the checksum for each part. It was missing for part %d in the request.",
				strings.ToLower(string(m.Checksum)), missingChecksum))
	}
	if err := l.checkCurrent(ctx, in.Bucket, in.Key, in.Conditions); err != nil {
		return domain.ObjectInfo{}, err
	}
	cfg, err := l.bucketConfig(ctx, in.Bucket)
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if cfg.QuotaBytes > 0 && uint64(plainTotal) > cfg.QuotaBytes {
		return domain.ObjectInfo{}, fmt.Errorf("%w: object of %d bytes exceeds the bucket quota of %d bytes", domain.ErrQuotaExceeded, plainTotal, cfg.QuotaBytes)
	}

	// From the journal on, the completion runs to its end (commit, or
	// parts moved back into the upload) even when the client goes away: a
	// canceled context would leave the parts in the staging directory. It
	// still stops when a namespace lock is lost (rolling back), since
	// another node may then complete or abort the upload.
	ctx = detached(ctx)
	versionID, versioned := l.versionFor(cfg, "")
	meta := make(map[string]string, len(m.Metadata)+2)
	for k, v := range m.Metadata {
		meta[k] = v
	}
	meta[metaETag] = hex.EncodeToString(etagSum.Sum(nil)) + "-" + strconv.Itoa(len(parts))
	meta[metaUploadID] = in.UploadID
	if encrypted || compressed {
		setActualSize(meta, plainTotal)
	}
	if !checksum.IsZero() {
		meta[metaChecksumPrefix+strings.ToLower(string(checksum.Algorithm))] = checksum.Value
		meta[metaChecksumType] = string(checksum.Type)
	}
	fi := storage.FileInfo{
		Volume:    in.Bucket,
		Name:      in.Key,
		VersionID: versionID,
		ModTime:   l.commitTime(ctx, in.Bucket, in.Key),
		Size:      total,
		DataDir:   uuid.NewString(),
		Metadata:  meta,
		Parts:     parts,
		Erasure:   m.layout(st),
		Versioned: versioned,
	}
	applyDefaultRetention(fi.Metadata, cfg, fi.ModTime)
	if err := sealChecksums(okey, &fi); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: seal the checksum of %s: %w", l.name, in.UploadID, err)
	}
	// Assemble the selected parts, renumbered, in a staging directory of
	// the upload, the moves journaled first: a failure moves back every
	// part wherever it went, and so does the next attempt after a crash,
	// so that the upload stays completable.
	staging := dir + "/" + stagingPrefix + fi.DataDir
	moves := make([]fileMove, len(selected))
	journal := completionJournal{Staging: staging, DataDir: fi.DataDir, VersionID: fi.VersionID}
	for i, p := range selected {
		moves[i] = fileMove{src: dir + "/" + p.DataFile, dst: staging + "/" + partName(i+1)}
		journal.Moves = append(journal.Moves, journalMove{Src: moves[i].src, Dst: moves[i].dst})
	}
	jdata, err := json.Marshal(journal)
	if err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: encode completion journal: %w", l.name, err)
	}
	if err := st.WriteAll(ctx, dir+"/"+journalFile, jdata); err != nil {
		return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
	}
	restore := func() { l.undoCompletion(ctx, st, dir, staging, moves) }
	if err := st.StageParts(ctx, moves, staging, fi); err != nil {
		restore()
		if isNotFound(err) {
			return domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidPart, "InvalidPart",
				"One or more of the specified parts could not be found. The data of a part is missing.")
		}
		return domain.ObjectInfo{}, fmt.Errorf("%s: assemble the parts of %s: %w", l.name, in.UploadID, err)
	}
	if err := checkLocks(ctx); err != nil {
		restore()
		return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
	}
	if err := st.CommitData(ctx, staging, fi, in.Bucket, in.Key); err != nil {
		restore()
		if cerr := checkLocks(ctx); cerr != nil {
			err = cerr
		}
		return domain.ObjectInfo{}, fmt.Errorf("%s: complete %s: %w", l.name, in.UploadID, err)
	}
	info := l.finishCompletion(ctx, st, dir, in.UploadID, fi)
	if !checksum.IsZero() {
		// Sealed on the drives of an encrypted version: reported in clear.
		info.Checksum = domain.Checksum{Algorithm: checksum.Algorithm, Value: checksum.Value, Type: checksum.Type}
	}
	return info, nil
}

// completionNeedsChecksums reports whether a completion reads the
// checksums of the parts: to compute the checksum of the object or to
// verify a checksum of the request.
func completionNeedsChecksums(m uploadMeta, in CompleteMultipartInput) bool {
	if m.Checksum != domain.ChecksumNone || !in.Checksum.IsZero() {
		return true
	}
	for _, cp := range in.Parts {
		if !cp.Checksum.IsZero() {
			return true
		}
	}
	return false
}

// completedFrom returns the current version of the key when the upload
// of in was completed into it with the same parts: S3 answers a retried
// completion (whose response was lost) with the object it created. It
// runs under the write lock of the key.
func (l *layer) completedFrom(ctx context.Context, in CompleteMultipartInput) (domain.ObjectInfo, bool) {
	st, err := l.place.forVersion(ctx, in.Bucket, in.Key, "")
	if err != nil {
		return domain.ObjectInfo{}, false
	}
	fi, err := l.readVersion(ctx, st, in.Bucket, in.Key, "", false)
	if err != nil || fi.Metadata[metaUploadID] != in.UploadID || len(fi.Parts) != len(in.Parts) {
		return domain.ObjectInfo{}, false
	}
	for i, p := range in.Parts {
		if !strings.EqualFold(strings.Trim(p.ETag, `"`), fi.Parts[i].ETag) {
			return domain.ObjectInfo{}, false
		}
	}
	fi.IsLatest = true
	return objectInfo(in.Bucket, in.Key, fi), true
}

// finishCompletion removes the upload of a committed completion, drops the
// copies of the key in other pools and publishes the event.
func (l *layer) finishCompletion(ctx context.Context, st store, dir, uploadID string, fi storage.FileInfo) domain.ObjectInfo {
	l.dropElsewhere(ctx, st, fi.Volume, fi.Name, fi.VersionID)
	if err := st.DeletePath(ctx, domain.SystemBucket, dir, true); err != nil && !isNotFound(err) {
		l.deps.Logger.WarnContext(ctx, "object: cannot remove completed upload", "upload", uploadID, "err", err)
	}
	fi.IsLatest = true
	info := objectInfo(fi.Volume, fi.Name, fi)
	l.publish(ctx, event.ObjectCreatedCompleteMultipartUpload, fi.Volume, info)
	return info
}

// undoCompletion moves the parts of a completion that did not commit back
// into the upload, wherever they went, then removes the staging directory
// and the journal.
func (l *layer) undoCompletion(ctx context.Context, st store, dir, staging string, moves []fileMove) {
	ctx = context.WithoutCancel(ctx)
	st.UndoMoves(ctx, moves)
	l.cleanupTmp(ctx, st, staging)
	if err := st.DeletePath(ctx, domain.SystemBucket, dir+"/"+journalFile, false); err != nil && !isNotFound(err) {
		l.deps.Logger.WarnContext(ctx, "object: cannot remove a completion journal", "dir", dir, "err", err)
	}
}

// recoverCompletion handles the journal left by a completion of the upload
// interrupted by a crash: when the version it was committing is in place,
// it finishes the completion and returns the object; otherwise it moves
// the parts back into the upload (nil, nil). It runs under the write lock
// of the upload.
func (l *layer) recoverCompletion(ctx context.Context, st store, dir, bucket, key string) (*domain.ObjectInfo, error) {
	data, err := st.ReadAll(ctx, dir+"/"+journalFile)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%s: read the completion journal of %s: %w", l.name, dir, err)
	}
	var j completionJournal
	if len(data) > maxJournalSize || json.Unmarshal(data, &j) != nil || !j.valid(dir) {
		l.deps.Logger.WarnContext(ctx, "object: discarding an unreadable completion journal", "dir", dir)
		l.undoCompletion(ctx, st, dir, "", nil)
		return nil, nil
	}
	moves := make([]fileMove, len(j.Moves))
	for i, mv := range j.Moves {
		moves[i] = fileMove{src: mv.Src, dst: mv.Dst}
	}
	vid := j.VersionID
	if vid == "" {
		vid = nullVersion
	}
	if fi, err := st.ReadVersion(ctx, bucket, key, vid, false); err == nil && fi.DataDir == j.DataDir && !fi.Deleted {
		// The interrupted completion committed: only its cleanup is left.
		l.deps.Logger.InfoContext(ctx, "object: finishing a completion interrupted after its commit", "bucket", bucket, "key", key)
		info := l.finishCompletion(ctx, st, dir, path.Base(dir), fi)
		return &info, nil
	}
	l.deps.Logger.InfoContext(ctx, "object: moving back the parts of an interrupted completion", "bucket", bucket, "key", key, "parts", len(moves))
	l.undoCompletion(ctx, st, dir, j.Staging, moves)
	return nil, nil
}

// valid reports whether every path of the journal lies inside the upload
// directory dir.
func (j completionJournal) valid(dir string) bool {
	inside := func(p string) bool {
		return strings.HasPrefix(p, dir+"/") && !strings.Contains(p, "..") && !strings.Contains(p, "//")
	}
	if !inside(j.Staging) || !strings.HasPrefix(path.Base(j.Staging), stagingPrefix) {
		return false
	}
	for _, mv := range j.Moves {
		if !inside(mv.Src) || !strings.HasPrefix(mv.Dst, j.Staging+"/") || strings.Contains(mv.Dst, "..") {
			return false
		}
	}
	return true
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
	// The prefix only filters the keys of the uploads: any prefix is
	// accepted, as in S3.
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
	ctx, unlock, err := l.lockUpload(ctx, bucket, uploadID, true)
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := st.DeletePath(ctx, domain.SystemBucket, d.Name, true); err != nil {
		if isNotFound(err) || errors.Is(err, domain.ErrBucketNotFound) {
			return false, nil // completed or aborted meanwhile
		}
		return false, fmt.Errorf("%s: abort stale upload %s: %w", l.name, uploadID, err)
	}
	l.deps.Logger.InfoContext(ctx, "object: aborted stale multipart upload", "bucket", m.Bucket, "key", m.Key, "upload", uploadID, "initiated", m.Initiated)
	return true, nil
}
