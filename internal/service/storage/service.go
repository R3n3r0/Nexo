// Package storage defines StorageAPI, the contract for accessing a single
// drive: metadata files (xl.meta), data parts and volume (bucket) directories.
// Implementations are the local xl storage (NewLocal) and the RPC client to a
// remote node (M6). The Formatter service manages the format.json documents
// that bind the drives of a pool together.
//
// Mocks are generated into ./mock by the repository-wide mockery
// configuration (.mockery.yaml, run through go generate in internal/service).
package storage

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Deps are the dependencies of the storage implementations. Every field is
// optional: nil values are replaced by defaults.
type Deps struct {
	Clock  clock.Clock
	Logger *slog.Logger
}

// Formatter manages the format.json documents of the drives of a deployment.
type Formatter interface {
	// EnsureFormats reads the format.json of every drive of layout and makes
	// the deployment consistent: a pool whose drives carry no document is
	// freshly formatted (one document per drive, written atomically), a
	// pool whose drives carry documents is checked with
	// topology.Service.CheckFormats and its unformatted drives (fresh
	// replacements) receive the document of their position and a healing
	// tracker (HealingFile), so that the healing service repopulates them. Drives are
	// matched to the layout by endpoint; endpoints without a drive (remote
	// nodes) are skipped. It returns the deployment id.
	EnsureFormats(ctx context.Context, layout *topology.Layout, drives []StorageAPI) (string, error)
}

// FormatterDeps are the dependencies of the formatter.
type FormatterDeps struct {
	Topology topology.Service
	// IDs generates deployment identifiers (default: UUID v4).
	IDs topology.IDGenerator
	// Clock dates the healing trackers of the replacement drives (default:
	// wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}

// Janitor sweeps the temporary area (TmpDir) of the local drives of a
// node: an entry left there longer than the age threshold is the leftover
// of a write whose coordinator crashed (in a cluster, another node), which
// the startup cleanup of the drive would only remove at its own restart.
// In-progress writes keep their entries fresh (the files they write are
// modified as the data flows), so the threshold (default 24h) only needs
// to exceed the longest stall of a write.
type Janitor interface {
	lifecycle.Runner
	// Sweep removes the stale entries of every drive now and returns how
	// many it removed.
	Sweep(ctx context.Context) (int, error)
}

// JanitorDeps are the dependencies of the janitor.
type JanitorDeps struct {
	// Drives are the drives to sweep: the local drives of the node.
	Drives []StorageAPI
	// Clock tells the age of the entries (default: wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}

// Well-known paths inside the system volume (domain.SystemBucket).
const (
	// FormatFile is the format.json document, relative to the system volume.
	FormatFile = "format.json"
	// TmpDir holds in-progress writes; it is emptied at startup.
	TmpDir = "tmp"
	// MultipartDir holds in-progress multipart uploads.
	MultipartDir = "multipart"
	// BucketsDir holds the per-bucket metadata documents.
	BucketsDir = "buckets"
	// ConfigDir holds the cluster configuration and the IAM documents.
	ConfigDir = "config"
	// HealingFile is the healing tracker of the drive (HealingTracker
	// encoded as JSON). Its presence marks a drive being healed.
	HealingFile = "healing.bin"
	// QuarantineDir keeps the xl.meta documents found undecodable by a
	// write (see StorageAPI.WriteMetadata): the object starts over on the
	// drive and the copy is kept for inspection; nothing removes it.
	QuarantineDir = "quarantine"
	// MetaFile is the per-object metadata document.
	MetaFile = "xl.meta"
)

// SmallFileThreshold is the size below which the object layer keeps the
// content of a version inline in xl.meta instead of a data directory.
const SmallFileThreshold = 128 << 10

// StorageAPI is the low-level drive interface used by the object layer.
// Every method takes the volume (bucket) and the path (object key) the
// operation refers to. Errors are domain sentinels (ErrDiskNotFound,
// ErrFaultyDisk, ErrObjectNotFound, ErrNoSuchVersion, ErrStorageFull, ...).
//
// Paths are slash separated and relative to the volume: empty segments,
// "." and "..", and symbolic links on disk are refused, so that no
// operation can reach outside the drive. A version id of "null" designates
// the null version, like the empty id.
type StorageAPI interface {
	// Endpoint returns the drive endpoint this API serves.
	Endpoint() domain.Endpoint
	// IsOnline reports whether the drive is reachable.
	IsOnline() bool

	// DiskInfo returns the drive usage and identity.
	DiskInfo(ctx context.Context, opts DiskInfoOptions) (domain.DiskInfo, error)

	// MakeVol creates a volume directory.
	MakeVol(ctx context.Context, volume string) error
	// ListVols lists the volumes on the drive.
	ListVols(ctx context.Context) ([]VolInfo, error)
	// StatVol returns information about a volume.
	StatVol(ctx context.Context, volume string) (VolInfo, error)
	// DeleteVol removes a volume; with force it removes its content too.
	DeleteVol(ctx context.Context, volume string, force bool) error

	// ReadVersion reads the metadata of one version of an object. An empty
	// versionID selects the latest version. With opts.ReadData the inline
	// data (small objects) is returned in FileInfo.Data.
	ReadVersion(ctx context.Context, volume, path, versionID string, opts ReadOptions) (FileInfo, error)
	// WriteMetadata writes (or appends) a version to the metadata file.
	// Like RenameData and DeleteVersion, it moves an undecodable xl.meta
	// (domain.ErrInvalidFormat) into QuarantineDir and starts the object
	// over on the drive instead of failing: the other versions are healed
	// back from the drives of the erasure set.
	WriteMetadata(ctx context.Context, volume, path string, fi FileInfo) error
	// UpdateMetadata updates the metadata of an existing version in place.
	UpdateMetadata(ctx context.Context, volume, path string, fi FileInfo, opts UpdateMetadataOptions) error
	// DeleteVersion removes one version (or writes a delete marker when
	// fi.MarkDeleted is set) and cleans up its data directory (unless
	// opts.KeepData). On an undecodable xl.meta (quarantined, see
	// WriteMetadata) the drive no longer holds the version: the delete
	// succeeds, and a delete marker becomes the only version.
	DeleteVersion(ctx context.Context, volume, path string, fi FileInfo, opts DeleteOptions) error

	// CreateFile streams size bytes from r into a new data file, written
	// in the temporary area and renamed into place once complete. size may
	// be -1 when unknown; otherwise a shorter stream fails with
	// io.ErrUnexpectedEOF and nothing is left behind.
	CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error
	// ReadFileStream returns a reader over length bytes starting at offset
	// (length -1 reads to the end). A range beyond the end of the file is
	// ErrInvalidRange when offset is past the end, ErrFileCorrupt (a
	// domain.ErrFaultyDisk) when only the end is.
	ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error)
	// OpenFile opens a data file for reads at any offset until the handle
	// is closed: the data stays readable when the file is removed or
	// replaced meanwhile (an open file survives its removal, POSIX), which
	// pins the shards of a read that released its lock (docs/02 §2.4).
	// Opening transfers no data; a remote drive keeps the file open on its
	// node until Close or an idle timeout.
	OpenFile(ctx context.Context, volume, path string) (FileHandle, error)
	// RenameData atomically moves the temporary data directory and metadata
	// of fi into their final location, committing the version. Parent
	// prefixes that are objects, or a destination that is a prefix, are
	// refused with domain.ErrInvalidObjectName (code XNexoParentIsObject /
	// XNexoObjectExistsAsDirectory).
	RenameData(ctx context.Context, srcVolume, srcPath string, fi FileInfo, dstVolume, dstPath string, opts RenameOptions) (RenameDataResult, error)
	// RenameFile atomically moves a single file (multipart parts being
	// assembled into an object data directory). Missing parents of the
	// destination are created.
	RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error

	// WalkDir walks the entries below opts.BaseDir in S3 key order and
	// calls fn for each. Returning an error from fn stops the walk with
	// that error; fs.SkipAll stops it without error; fs.SkipDir skips the
	// content of a prefix entry (or the remaining entries of the current
	// directory when returned for an object entry).
	WalkDir(ctx context.Context, opts WalkDirOptions, fn WalkDirFunc) error

	// ReadAll reads a whole small file (configuration, format.json, ...).
	ReadAll(ctx context.Context, volume, path string) ([]byte, error)
	// WriteAll atomically writes a whole small file.
	WriteAll(ctx context.Context, volume, path string, b []byte) error
	// Delete removes a file or, with opts.Recursive, a directory tree.
	Delete(ctx context.Context, volume, path string, opts DeleteOptions) error

	// CheckParts verifies that every part of fi exists with the right size.
	CheckParts(ctx context.Context, volume, path string, fi FileInfo) (*CheckPartsResult, error)
	// StatInfoFile stats a path; with glob, path may contain "*".
	StatInfoFile(ctx context.Context, volume, path string, glob bool) ([]StatInfo, error)

	// Healing returns the healing state of the drive, nil when not healing.
	Healing() *HealingTracker
	// Close releases the drive resources.
	Close(ctx context.Context) error
}

// FileHandle is a data file opened by StorageAPI.OpenFile.
type FileHandle interface {
	// Size returns the size of the file when it was opened. Pure accessor.
	Size() int64
	// ReadStream returns a reader over length bytes of the file starting
	// at offset (length -1 reads to the end), with the bounds checks of
	// StorageAPI.ReadFileStream. Closing the stream leaves the handle
	// open; several streams may be open at once.
	ReadStream(ctx context.Context, offset, length int64) (io.ReadCloser, error)
	// Close releases the handle: the file is closed once its last stream
	// is closed. Closing twice is a no-op.
	Close(ctx context.Context) error
}

// DiskInfoOptions tunes DiskInfo.
type DiskInfoOptions struct {
	// DiskID, when set, must match the drive's id or ErrDiskNotFound is returned.
	DiskID string
	// Metrics requests per-API latency metrics in the result.
	Metrics bool
	// NoOp skips the filesystem statfs call and returns cached values.
	NoOp bool
}

// VolInfo describes a volume (bucket directory).
type VolInfo struct {
	Name    string
	Created time.Time
}

// ReadOptions tunes ReadVersion.
type ReadOptions struct {
	// ReadData returns inline data in FileInfo.Data.
	ReadData bool
	// Healing allows reading versions marked as being healed.
	Healing bool
	// InclFreeVersions includes free versions (tiered objects).
	InclFreeVersions bool
}

// UpdateMetadataOptions tunes UpdateMetadata.
type UpdateMetadataOptions struct {
	// NoPersistence skips the fsync for non-critical updates.
	NoPersistence bool
}

// DeleteOptions tunes DeleteVersion and Delete.
type DeleteOptions struct {
	Recursive bool
	Immediate bool
	// UndoWrite rolls back a partially written version.
	UndoWrite bool
	// OldDataDir is the data directory to remove when replacing a version.
	OldDataDir string
	// KeepData leaves in place the data directory of the version that
	// DeleteVersion removes (or that a delete marker replaces): the caller
	// purges it once the delete reached its quorum, or restores the version
	// with WriteMetadata when it did not.
	KeepData bool
}

// RenameOptions tunes RenameData.
type RenameOptions struct {
	// BaseOptions are shared with Delete when replacing an old version.
	BaseOptions DeleteOptions
}

// RenameDataResult is the outcome of RenameData.
type RenameDataResult struct {
	// OldDataDir is the data dir of the replaced version, to be purged.
	OldDataDir string
	// Signature identifies the written metadata for quorum checks.
	Signature [4]byte
}

// WalkDirOptions tunes WalkDir.
type WalkDirOptions struct {
	Bucket  string
	BaseDir string
	// Recursive descends into sub-directories.
	Recursive bool
	// ReportNotFound reports a missing BaseDir instead of an empty walk.
	ReportNotFound bool
	// FilterPrefix only returns entries starting with the prefix.
	FilterPrefix string
	// ForwardTo skips entries before this name (resume a listing).
	ForwardTo string
	// Limit stops after this many entries (0 = unlimited).
	Limit int
}

// WalkEntry is one entry produced by WalkDir.
type WalkEntry struct {
	// Name is the entry path relative to the volume. Directory entries end
	// with a slash.
	Name string
	// IsDir reports a directory (prefix).
	IsDir bool
	// Metadata is the raw xl.meta content for object entries, nil for prefixes.
	Metadata []byte
}

// WalkDirFunc receives WalkDir entries. Returning fs.SkipDir from a
// directory entry skips its content.
type WalkDirFunc func(entry WalkEntry) error

// FileInfo is one version of an object as stored in xl.meta.
type FileInfo struct {
	Volume string
	Name   string
	// VersionID is the version UUID, empty for unversioned objects.
	VersionID string
	IsLatest  bool
	// Deleted marks a delete marker.
	Deleted bool
	// DataDir is the UUID directory holding the parts.
	DataDir string
	ModTime time.Time
	Size    int64
	// Mode mirrors the fs.FileMode bits kept for compatibility.
	Mode uint32
	// Metadata holds the HTTP metadata (content-type, x-amz-meta-*, ...).
	Metadata map[string]string
	Parts    []PartInfo
	Erasure  ErasureInfo
	// Data is the inline content of small objects.
	Data []byte
	// NumVersions is the total number of versions of the object.
	NumVersions int
	// SuccessorModTime is the mod time of the next newer version.
	SuccessorModTime time.Time
	// Fresh reports a brand new object (no previous versions).
	Fresh bool
	// Idx is the position of the version in the metadata file.
	Idx int
	// Checksum is the composite additional checksum of the object.
	Checksum []byte
	// Versioned reports that the bucket had versioning when written.
	Versioned bool
	// MarkDeleted asks DeleteVersion to write a delete marker.
	MarkDeleted bool
	// ReplicationState and TransitionStatus are opaque to storage.
	ReplicationState map[string]string
	TransitionStatus string
}

// PartInfo describes an erasure-coded part on this drive.
type PartInfo struct {
	Number     int
	ETag       string
	Size       int64
	ActualSize int64
	ModTime    time.Time
	// Index is an optional compression/encryption index.
	Index []byte
	// Checksums holds the per-part additional checksums.
	Checksums map[string]string
}

// ErasureInfo is the erasure layout of a version.
type ErasureInfo struct {
	Algorithm    string
	DataBlocks   int
	ParityBlocks int
	BlockSize    int64
	// Index is the 1-based shard index of this drive.
	Index int
	// Distribution is the shard distribution across drives.
	Distribution []int
	// Checksums are the bitrot checksums of each part.
	Checksums []ChecksumInfo
}

// ChecksumInfo is the bitrot checksum of one part.
type ChecksumInfo struct {
	PartNumber int
	Algorithm  string
	Hash       []byte
}

// CheckPartsResult reports the state of each part.
type CheckPartsResult struct {
	// Results has one entry per part: one of the CheckPart* constants.
	Results []int
}

// CheckParts result codes.
const (
	CheckPartUnknown = iota
	CheckPartSuccess
	CheckPartFileNotFound
	CheckPartFileCorrupt
	CheckPartVolumeNotFound
)

// StatInfo describes a stat'ed path.
type StatInfo struct {
	Name    string
	Size    int64
	Dir     bool
	Mode    uint32
	ModTime time.Time
}

// HealingTracker is the persisted healing state of a drive.
type HealingTracker struct {
	ID            string
	PoolIndex     int
	SetIndex      int
	DiskIndex     int
	Path          string
	Endpoint      string
	Started       time.Time
	LastUpdate    time.Time
	ObjectsHealed uint64
	ObjectsFailed uint64
	BytesDone     uint64
	BytesFailed   uint64
	// Bucket and Object are the last item healed: a restarted healing
	// resumes after them.
	Bucket string
	Object string
	// HealedBuckets lists the buckets completely healed.
	HealedBuckets []string `json:",omitempty"`
	// RetryAttempts counts the passes started over because objects failed
	// to heal in the previous one (sources momentarily without quorum).
	RetryAttempts int `json:",omitempty"`
	Finished      bool
}
