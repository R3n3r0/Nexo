package storage

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/r3n3r0/nexo/internal/domain"
)

// xl.meta binary layout (integers are big endian):
//
//	[0, 4)        magic "NXLM"
//	[4, 6)        major format version
//	[6, 8)        minor format version
//	[8, 12)       length L of the metadata section
//	[12, 12+L)    metadata section: msgpack xlMeta (every version, no content)
//	[12+L, 16+L)  CRC-32C of bytes [0, 12+L)
//	[16+L, ...)   optional data section: msgpack map from version id to the
//	              inline content of small objects, then its CRC-32C
//
// Readers that only need the versions (listings, HEAD requests) read the
// first 16+L bytes and never load the inline content. A new minor version
// may add fields (unknown fields are ignored), a new major version is
// refused. The JSON document written by the first development builds
// ("legacy", recognized by its leading '{') is still readable and is
// rewritten in the binary format on the next update.
const (
	xlMetaMagic      = "NXLM"
	xlMetaMajor      = 1
	xlMetaMinor      = 0
	xlMetaHeaderSize = 12
	xlMetaCRCSize    = 4
	// xlMetaMaxDepth bounds the nesting of msgpack containers.
	xlMetaMaxDepth = 16
	// xlMetaMaxMetaSize bounds the metadata section.
	xlMetaMaxMetaSize = 64 << 20
	// xlMetaLegacyVersion is the "version" of the legacy JSON document.
	xlMetaLegacyVersion = 1
	// nullVersionID is the S3 name of the null version, stored as "".
	nullVersionID = "null"
)

// Metadata keys of FileInfo.Metadata stored in dedicated xl.meta fields.
const (
	metaKeyETag        = "etag"
	metaKeyContentType = "content-type"
	metaKeyTagging     = "x-amz-tagging"
	metaKeyUserPrefix  = "x-amz-meta-"
)

// xlVersionType tells objects from delete markers.
type xlVersionType uint8

const (
	xlTypeInvalid xlVersionType = iota
	xlTypeObject
	xlTypeDeleteMarker
)

// Version flags.
const (
	// xlFlagVersioned records that the bucket had versioning enabled.
	xlFlagVersioned uint8 = 1 << iota
	// xlFlagInlineData marks a version whose content is in the data section.
	xlFlagInlineData
)

// xlMeta is the metadata document of an object: every version, latest
// (highest modification time) first.
type xlMeta struct {
	Versions []xlVersion `msgpack:"vs"`
}

// xlVersion is one version of an object as persisted in xl.meta.
type xlVersion struct {
	// ID is the version id, "" for the null version.
	ID   string        `msgpack:"id,omitempty"`
	Type xlVersionType `msgpack:"ty"`
	// ModTime is in Unix nanoseconds (0 for the zero time).
	ModTime     int64  `msgpack:"mt"`
	Size        int64  `msgpack:"sz,omitempty"`
	ETag        string `msgpack:"et,omitempty"`
	ContentType string `msgpack:"ct,omitempty"`
	// UserMetadata holds the x-amz-meta-* entries without their prefix.
	UserMetadata map[string]string `msgpack:"um,omitempty"`
	// Tags is the URL-encoded tag set (x-amz-tagging).
	Tags string `msgpack:"tg,omitempty"`
	// Meta holds the remaining HTTP and system metadata.
	Meta        map[string]string `msgpack:"md,omitempty"`
	DataDir     string            `msgpack:"dd,omitempty"`
	Mode        uint32            `msgpack:"mo,omitempty"`
	Parts       []xlPart          `msgpack:"pt,omitempty"`
	Erasure     *xlErasure        `msgpack:"ec,omitempty"`
	Checksum    []byte            `msgpack:"cs,omitempty"`
	Flags       uint8             `msgpack:"fl,omitempty"`
	Replication map[string]string `msgpack:"rp,omitempty"`
	Transition  string            `msgpack:"tr,omitempty"`
	// Data is the inline content; it lives in the data section.
	Data []byte `msgpack:"-"`
}

// xlPart describes one part of a version.
type xlPart struct {
	Number     int               `msgpack:"n"`
	ETag       string            `msgpack:"et,omitempty"`
	Size       int64             `msgpack:"sz"`
	ActualSize int64             `msgpack:"as,omitempty"`
	ModTime    int64             `msgpack:"mt,omitempty"`
	Index      []byte            `msgpack:"ix,omitempty"`
	Checksums  map[string]string `msgpack:"cs,omitempty"`
}

// xlErasure is the erasure layout of a version on this drive.
type xlErasure struct {
	Algorithm    string       `msgpack:"al,omitempty"`
	Data         int          `msgpack:"d"`
	Parity       int          `msgpack:"p"`
	BlockSize    int64        `msgpack:"bs"`
	Index        int          `msgpack:"i"`
	Distribution []int        `msgpack:"dist,omitempty"`
	Checksums    []xlChecksum `msgpack:"cs,omitempty"`
}

// xlChecksum is the bitrot checksum of one part.
type xlChecksum struct {
	Part      int    `msgpack:"n"`
	Algorithm string `msgpack:"al,omitempty"`
	Hash      []byte `msgpack:"h,omitempty"`
}

// --- operations -------------------------------------------------------------

// normalizeVersionID maps the S3 name of the null version to its stored id.
func normalizeVersionID(id string) string {
	if id == nullVersionID {
		return ""
	}
	return id
}

// AddVersion inserts v, replacing the version with the same id (the null
// version of unversioned and suspended buckets is replaced, never
// duplicated). Versions stay ordered by descending modification time; v
// sorts before the versions with its own modification time, so the last
// write wins. It returns the replaced version, if any.
func (m *xlMeta) AddVersion(v xlVersion) (*xlVersion, error) {
	v.ID = normalizeVersionID(v.ID)
	if err := v.validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrInvalidArgument, err)
	}
	if v.Type == xlTypeDeleteMarker {
		v.clearContent()
	}
	var replaced *xlVersion
	if i := m.indexOf(v.ID); i >= 0 {
		old := m.Versions[i]
		replaced = &old
		m.Versions = slices.Delete(m.Versions, i, i+1)
	}
	pos := sort.Search(len(m.Versions), func(i int) bool { return m.Versions[i].ModTime <= v.ModTime })
	m.Versions = slices.Insert(m.Versions, pos, v)
	return replaced, nil
}

// DeleteVersion removes the version with the given id ("" and "null" are
// the null version) and returns it, or fails with domain.ErrNoSuchVersion.
func (m *xlMeta) DeleteVersion(versionID string) (xlVersion, error) {
	i, err := m.FindVersion(versionID)
	if err != nil {
		return xlVersion{}, err
	}
	v := m.Versions[i]
	m.Versions = slices.Delete(m.Versions, i, i+1)
	return v, nil
}

// FindVersion returns the index of the version with the given id ("" and
// "null" are the null version), or domain.ErrNoSuchVersion.
func (m *xlMeta) FindVersion(versionID string) (int, error) {
	if i := m.indexOf(normalizeVersionID(versionID)); i >= 0 {
		return i, nil
	}
	return -1, fmt.Errorf("%w: %q", domain.ErrNoSuchVersion, versionID)
}

// Latest returns the most recent version (possibly a delete marker).
func (m *xlMeta) Latest() (xlVersion, bool) {
	if len(m.Versions) == 0 {
		return xlVersion{}, false
	}
	return m.Versions[0], true
}

// ListVersions returns a copy of the versions, latest first.
func (m *xlMeta) ListVersions() []xlVersion {
	return slices.Clone(m.Versions)
}

// Compact normalizes the document before it is written: ids use the
// stored form of the null version, delete markers carry no content, empty
// maps are dropped, versions are sorted by descending modification time and
// duplicated ids (a damaged document) keep only their most recent entry.
func (m *xlMeta) Compact() {
	for i := range m.Versions {
		v := &m.Versions[i]
		v.ID = normalizeVersionID(v.ID)
		if v.Type == xlTypeDeleteMarker {
			v.clearContent()
		}
		v.UserMetadata = nilIfEmpty(v.UserMetadata)
		v.Meta = nilIfEmpty(v.Meta)
		v.Replication = nilIfEmpty(v.Replication)
		for j := range v.Parts {
			v.Parts[j].Checksums = nilIfEmpty(v.Parts[j].Checksums)
		}
	}
	m.sortVersions()
	seen := make(map[string]bool, len(m.Versions))
	out := m.Versions[:0]
	for _, v := range m.Versions {
		if seen[v.ID] {
			continue
		}
		seen[v.ID] = true
		out = append(out, v)
	}
	clear(m.Versions[len(out):])
	m.Versions = out
}

// sharesDataDir reports whether a version of the document references the
// data directory: it must then survive the removal of another version.
func (m *xlMeta) sharesDataDir(dataDir string) bool {
	if dataDir == "" {
		return false
	}
	for i := range m.Versions {
		if m.Versions[i].DataDir == dataDir {
			return true
		}
	}
	return false
}

func (m *xlMeta) indexOf(id string) int {
	for i := range m.Versions {
		if m.Versions[i].ID == id {
			return i
		}
	}
	return -1
}

// sortVersions orders the versions by descending modification time,
// keeping the relative order of versions with the same time.
func (m *xlMeta) sortVersions() {
	sort.SliceStable(m.Versions, func(i, j int) bool { return m.Versions[i].ModTime > m.Versions[j].ModTime })
}

// clearContent drops the fields a delete marker cannot carry.
func (v *xlVersion) clearContent() {
	v.Size, v.DataDir, v.Parts, v.Erasure, v.Data = 0, "", nil, nil, nil
	v.Flags &^= xlFlagInlineData
}

// validate checks the fields that the drive later trusts: the data
// directory becomes a path and sizes become read lengths.
func (v *xlVersion) validate() error {
	switch v.Type {
	case xlTypeObject, xlTypeDeleteMarker:
	default:
		return fmt.Errorf("version %q: unknown type %d", v.ID, v.Type)
	}
	if v.Size < 0 {
		return fmt.Errorf("version %q: negative size %d", v.ID, v.Size)
	}
	if v.DataDir != "" && !isDataDir(v.DataDir) {
		return fmt.Errorf("version %q: data directory %q is not a UUID", v.ID, v.DataDir)
	}
	for _, p := range v.Parts {
		if p.Number < 1 || p.Size < 0 || p.ActualSize < 0 {
			return fmt.Errorf("version %q: invalid part %d (size %d)", v.ID, p.Number, p.Size)
		}
	}
	if e := v.Erasure; e != nil {
		if e.Data < 0 || e.Parity < 0 || e.BlockSize < 0 || e.Index < 0 {
			return fmt.Errorf("version %q: invalid erasure layout", v.ID)
		}
	}
	return nil
}

// signature identifies the committed version across drives (quorum checks):
// the fields every drive shares, not the per-drive erasure index.
func (v *xlVersion) signature() [4]byte {
	h := crc32.New(castagnoli())
	for _, s := range []string{v.ID, v.DataDir, v.ETag} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	var b [18]byte
	binary.BigEndian.PutUint64(b[0:], uint64(v.ModTime)) // #nosec G115 -- the bits of an int64 in the version signature
	binary.BigEndian.PutUint64(b[8:], uint64(v.Size))    // #nosec G115 -- the bits of an int64 in the version signature
	b[16], b[17] = byte(v.Type), v.Flags
	_, _ = h.Write(b[:])
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], h.Sum32())
	return out
}

// --- FileInfo conversion ----------------------------------------------------

func versionFromFileInfo(fi FileInfo) xlVersion {
	v := xlVersion{
		ID:          normalizeVersionID(fi.VersionID),
		Type:        xlTypeObject,
		ModTime:     unixNano(fi.ModTime),
		Size:        fi.Size,
		DataDir:     fi.DataDir,
		Mode:        fi.Mode,
		Checksum:    fi.Checksum,
		Replication: fi.ReplicationState,
		Transition:  fi.TransitionStatus,
		Data:        fi.Data,
	}
	if fi.Deleted {
		v.Type = xlTypeDeleteMarker
	}
	if fi.Versioned {
		v.Flags |= xlFlagVersioned
	}
	if fi.Data != nil {
		v.Flags |= xlFlagInlineData
	}
	for k, val := range fi.Metadata {
		switch {
		case val == "":
			v.Meta = setKey(v.Meta, k, val) // keeps empty values as they are
		case k == metaKeyETag:
			v.ETag = val
		case k == metaKeyContentType:
			v.ContentType = val
		case k == metaKeyTagging:
			v.Tags = val
		case strings.HasPrefix(k, metaKeyUserPrefix) && len(k) > len(metaKeyUserPrefix):
			v.UserMetadata = setKey(v.UserMetadata, k[len(metaKeyUserPrefix):], val)
		default:
			v.Meta = setKey(v.Meta, k, val)
		}
	}
	if len(fi.Parts) > 0 {
		v.Parts = make([]xlPart, len(fi.Parts))
		for i, p := range fi.Parts {
			v.Parts[i] = xlPart{Number: p.Number, ETag: p.ETag, Size: p.Size, ActualSize: p.ActualSize,
				ModTime: unixNano(p.ModTime), Index: p.Index, Checksums: p.Checksums}
		}
	}
	if e := fi.Erasure; !isZeroErasure(e) {
		xe := &xlErasure{Algorithm: e.Algorithm, Data: e.DataBlocks, Parity: e.ParityBlocks,
			BlockSize: e.BlockSize, Index: e.Index, Distribution: e.Distribution}
		for _, c := range e.Checksums {
			xe.Checksums = append(xe.Checksums, xlChecksum{Part: c.PartNumber, Algorithm: c.Algorithm, Hash: c.Hash})
		}
		v.Erasure = xe
	}
	return v
}

// fileInfo converts the version back; Metadata is never nil.
func (v *xlVersion) fileInfo(volume, name string) FileInfo {
	fi := FileInfo{
		Volume:           volume,
		Name:             name,
		VersionID:        v.ID,
		Deleted:          v.Type == xlTypeDeleteMarker,
		DataDir:          v.DataDir,
		ModTime:          fromUnixNano(v.ModTime),
		Size:             v.Size,
		Mode:             v.Mode,
		Data:             v.Data,
		Checksum:         v.Checksum,
		Versioned:        v.Flags&xlFlagVersioned != 0,
		ReplicationState: v.Replication,
		TransitionStatus: v.Transition,
	}
	md := make(map[string]string, len(v.Meta)+len(v.UserMetadata)+3)
	for k, val := range v.Meta {
		md[k] = val
	}
	for k, val := range v.UserMetadata {
		md[metaKeyUserPrefix+k] = val
	}
	if v.ETag != "" {
		md[metaKeyETag] = v.ETag
	}
	if v.ContentType != "" {
		md[metaKeyContentType] = v.ContentType
	}
	if v.Tags != "" {
		md[metaKeyTagging] = v.Tags
	}
	fi.Metadata = md
	if len(v.Parts) > 0 {
		fi.Parts = make([]PartInfo, len(v.Parts))
		for i, p := range v.Parts {
			fi.Parts[i] = PartInfo{Number: p.Number, ETag: p.ETag, Size: p.Size, ActualSize: p.ActualSize,
				ModTime: fromUnixNano(p.ModTime), Index: p.Index, Checksums: p.Checksums}
		}
	}
	if e := v.Erasure; e != nil {
		fi.Erasure = ErasureInfo{Algorithm: e.Algorithm, DataBlocks: e.Data, ParityBlocks: e.Parity,
			BlockSize: e.BlockSize, Index: e.Index, Distribution: e.Distribution}
		for _, c := range e.Checksums {
			fi.Erasure.Checksums = append(fi.Erasure.Checksums, ChecksumInfo{PartNumber: c.Part, Algorithm: c.Algorithm, Hash: c.Hash})
		}
	}
	return fi
}

// fileInfoAt builds the FileInfo of version i with the positional fields
// (IsLatest, NumVersions, SuccessorModTime, Idx) filled in.
func (m *xlMeta) fileInfoAt(i int, volume, name string, readData bool) FileInfo {
	fi := m.Versions[i].fileInfo(volume, name)
	fi.IsLatest = i == 0
	fi.NumVersions = len(m.Versions)
	fi.Idx = i
	if i > 0 {
		fi.SuccessorModTime = fromUnixNano(m.Versions[i-1].ModTime)
	}
	if !readData {
		fi.Data = nil
	}
	return fi
}

// fileInfos returns every version as a FileInfo, latest first, without
// inline data.
func (m *xlMeta) fileInfos(volume, name string) []FileInfo {
	out := make([]FileInfo, 0, len(m.Versions))
	for i := range m.Versions {
		out = append(out, m.fileInfoAt(i, volume, name, false))
	}
	return out
}

// DecodeFileInfos decodes a raw xl.meta document (as produced by WalkDir,
// possibly without its data section) into the FileInfos of its versions,
// latest first, without inline data.
func DecodeFileInfos(volume, name string, raw []byte) ([]FileInfo, error) {
	m, err := decodeXLMeta(raw, false)
	if err != nil {
		return nil, err
	}
	return m.fileInfos(volume, name), nil
}

// --- encoding ---------------------------------------------------------------

// encode compacts and serializes the document.
func (m *xlMeta) encode() ([]byte, error) {
	m.Compact()
	meta, err := marshalMsgpack(m)
	if err != nil {
		return nil, err
	}
	if len(meta) > xlMetaMaxMetaSize {
		return nil, fmt.Errorf("%w: xl.meta: metadata section of %d bytes exceeds %d", domain.ErrInvalidArgument, len(meta), xlMetaMaxMetaSize)
	}
	out := make([]byte, xlMetaHeaderSize, xlMetaHeaderSize+len(meta)+xlMetaCRCSize)
	copy(out, xlMetaMagic)
	binary.BigEndian.PutUint16(out[4:], xlMetaMajor)
	binary.BigEndian.PutUint16(out[6:], xlMetaMinor)
	binary.BigEndian.PutUint32(out[8:], uint32(len(meta))) // #nosec G115 -- len(meta) <= xlMetaMaxMetaSize is checked above
	out = append(out, meta...)
	out = binary.BigEndian.AppendUint32(out, crc32c(out))
	section, err := m.encodeDataSection()
	if err != nil {
		return nil, err
	}
	if section != nil {
		out = append(out, section...)
		out = binary.BigEndian.AppendUint32(out, crc32c(section))
	}
	return out, nil
}

// encodeDataSection encodes the inline content of the versions as a
// msgpack map from version id to bytes, keys in sorted order so that the
// document is deterministic. It returns nil without inline versions.
func (m *xlMeta) encodeDataSection() ([]byte, error) {
	var ids []string
	content := make(map[string][]byte)
	for _, v := range m.Versions {
		if v.Flags&xlFlagInlineData != 0 {
			ids = append(ids, v.ID)
			content[v.ID] = v.Data
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sort.Strings(ids)
	var b bytes.Buffer
	enc := msgpack.NewEncoder(&b)
	err := enc.EncodeMapLen(len(ids))
	for _, id := range ids {
		if err == nil {
			err = enc.EncodeString(id)
		}
		if err == nil {
			err = enc.EncodeBytes(content[id])
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: encode xl.meta data: %w", domain.ErrInvalidArgument, err)
	}
	return b.Bytes(), nil
}

// decodeXLMeta parses an xl.meta document. With withData the inline
// content of the versions is loaded from the data section, which must then
// be present for every inline version; without it the data section, if
// any, is ignored (raw may stop at the end of the metadata section).
func decodeXLMeta(raw []byte, withData bool) (*xlMeta, error) {
	if isLegacyXLMeta(raw) {
		return decodeLegacyXLMeta(raw, withData)
	}
	end, err := xlMetaSectionEnd(raw)
	if err != nil {
		return nil, err
	}
	if got, want := crc32c(raw[:end-xlMetaCRCSize]), binary.BigEndian.Uint32(raw[end-xlMetaCRCSize:end]); got != want {
		return nil, fmt.Errorf("%w: %w: xl.meta metadata checksum mismatch", domain.ErrInvalidFormat, ErrFileCorrupt)
	}
	var m xlMeta
	if err := unmarshalMsgpack(raw[xlMetaHeaderSize:end-xlMetaCRCSize], &m); err != nil {
		return nil, fmt.Errorf("%w: xl.meta metadata: %w", domain.ErrInvalidFormat, err)
	}
	for i := range m.Versions {
		if err := m.Versions[i].validate(); err != nil {
			return nil, fmt.Errorf("%w: xl.meta: %w", domain.ErrInvalidFormat, err)
		}
	}
	if !withData {
		return &m, nil
	}
	var data map[string][]byte
	if rest := raw[end:]; len(rest) > 0 {
		if len(rest) <= xlMetaCRCSize {
			return nil, fmt.Errorf("%w: %w: xl.meta data section truncated", domain.ErrInvalidFormat, ErrFileCorrupt)
		}
		section := rest[:len(rest)-xlMetaCRCSize]
		if crc32c(section) != binary.BigEndian.Uint32(rest[len(section):]) {
			return nil, fmt.Errorf("%w: %w: xl.meta data checksum mismatch", domain.ErrInvalidFormat, ErrFileCorrupt)
		}
		if err := unmarshalMsgpack(section, &data); err != nil {
			return nil, fmt.Errorf("%w: xl.meta data: %w", domain.ErrInvalidFormat, err)
		}
	}
	for i := range m.Versions {
		v := &m.Versions[i]
		if v.Flags&xlFlagInlineData == 0 {
			continue
		}
		content, ok := data[v.ID]
		if !ok {
			return nil, fmt.Errorf("%w: %w: xl.meta: inline data of version %q missing", domain.ErrInvalidFormat, ErrFileCorrupt, v.ID)
		}
		if content == nil {
			content = []byte{}
		}
		v.Data = content
	}
	return &m, nil
}

// xlMetaSectionEnd validates the header of a binary xl.meta and returns the
// offset where its metadata section (and checksum) ends.
func xlMetaSectionEnd(raw []byte) (int, error) {
	if len(raw) < xlMetaHeaderSize+xlMetaCRCSize || string(raw[:4]) != xlMetaMagic {
		return 0, fmt.Errorf("%w: xl.meta: bad magic or truncated header", domain.ErrInvalidFormat)
	}
	if major := binary.BigEndian.Uint16(raw[4:]); major != xlMetaMajor {
		return 0, fmt.Errorf("%w: xl.meta: unsupported format version %d.%d", domain.ErrInvalidFormat, major, binary.BigEndian.Uint16(raw[6:]))
	}
	n := uint64(binary.BigEndian.Uint32(raw[8:]))
	// #nosec G115 -- len(raw) >= xlMetaHeaderSize+xlMetaCRCSize is checked above
	if n > xlMetaMaxMetaSize || n > uint64(len(raw)-xlMetaHeaderSize-xlMetaCRCSize) {
		return 0, fmt.Errorf("%w: %w: xl.meta metadata section of %d bytes truncated", domain.ErrInvalidFormat, ErrFileCorrupt, n)
	}
	return xlMetaHeaderSize + int(n) + xlMetaCRCSize, nil
}

func marshalMsgpack(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := msgpack.NewEncoder(&b)
	enc.SetSortMapKeys(true)
	enc.UseCompactInts(true)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: encode xl.meta: %w", domain.ErrInvalidArgument, err)
	}
	return b.Bytes(), nil
}

// unmarshalMsgpack decodes b into v after a structural check that bounds
// the allocations of the decoder (see checkMsgpack).
func unmarshalMsgpack(b []byte, v any) error {
	if err := checkMsgpack(b, xlMetaMaxDepth); err != nil {
		return err
	}
	return msgpack.NewDecoder(bytes.NewReader(b)).Decode(v)
}

// --- legacy JSON format -----------------------------------------------------

// legacyXLMeta is the JSON xl.meta of the first development builds.
type legacyXLMeta struct {
	Version  int             `json:"version"`
	Versions []legacyVersion `json:"versions"`
}

type legacyVersion struct {
	ID               string            `json:"id,omitempty"`
	Deleted          bool              `json:"deleted,omitempty"`
	DataDir          string            `json:"dataDir,omitempty"`
	ModTime          time.Time         `json:"modTime"`
	Size             int64             `json:"size"`
	Mode             uint32            `json:"mode,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Parts            []PartInfo        `json:"parts,omitempty"`
	Erasure          ErasureInfo       `json:"erasure"`
	Data             []byte            `json:"data,omitempty"`
	Checksum         []byte            `json:"checksum,omitempty"`
	Versioned        bool              `json:"versioned,omitempty"`
	ReplicationState map[string]string `json:"replicationState,omitempty"`
	TransitionStatus string            `json:"transitionStatus,omitempty"`
}

// isLegacyXLMeta recognizes the JSON document by its first non-blank byte.
func isLegacyXLMeta(raw []byte) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func decodeLegacyXLMeta(raw []byte, withData bool) (*xlMeta, error) {
	var legacy legacyXLMeta
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, fmt.Errorf("%w: legacy xl.meta: %w", domain.ErrInvalidFormat, err)
	}
	if legacy.Version != xlMetaLegacyVersion {
		return nil, fmt.Errorf("%w: legacy xl.meta version %d, want %d", domain.ErrInvalidFormat, legacy.Version, xlMetaLegacyVersion)
	}
	m := &xlMeta{Versions: make([]xlVersion, 0, len(legacy.Versions))}
	for _, lv := range legacy.Versions {
		v := versionFromFileInfo(FileInfo{
			VersionID: lv.ID, Deleted: lv.Deleted, DataDir: lv.DataDir, ModTime: lv.ModTime,
			Size: lv.Size, Mode: lv.Mode, Metadata: lv.Metadata, Parts: lv.Parts, Erasure: lv.Erasure,
			Data: lv.Data, Checksum: lv.Checksum, Versioned: lv.Versioned,
			ReplicationState: lv.ReplicationState, TransitionStatus: lv.TransitionStatus,
		})
		if err := v.validate(); err != nil {
			return nil, fmt.Errorf("%w: legacy xl.meta: %w", domain.ErrInvalidFormat, err)
		}
		if !withData {
			v.Data = nil
		}
		m.Versions = append(m.Versions, v)
	}
	m.sortVersions()
	return m, nil
}

// --- helpers ----------------------------------------------------------------

func castagnoli() *crc32.Table { return crc32.MakeTable(crc32.Castagnoli) }

func crc32c(b []byte) uint32 { return crc32.Checksum(b, castagnoli()) }

// unixNano encodes a time as Unix nanoseconds, the zero time as 0.
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// fromUnixNano is the inverse of unixNano; times are returned in UTC.
func fromUnixNano(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func isZeroErasure(e ErasureInfo) bool {
	return e.Algorithm == "" && e.DataBlocks == 0 && e.ParityBlocks == 0 && e.BlockSize == 0 &&
		e.Index == 0 && len(e.Distribution) == 0 && len(e.Checksums) == 0
}

func setKey(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = make(map[string]string)
	}
	m[k] = v
	return m
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}
