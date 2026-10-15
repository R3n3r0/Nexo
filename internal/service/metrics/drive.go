package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// driveInstruments are the instruments of the drive decorator.
type driveInstruments struct {
	ops     Counter
	seconds Counter
	errors  Counter
}

// newDriveInstruments registers (or finds) the drive operation metrics.
func newDriveInstruments(ctx context.Context, reg Registry) (*driveInstruments, error) {
	ops, err := reg.Counter(ctx, Definition{Subsystem: "drive", Name: "operations_total",
		Help: "Operations on the local drives, by drive and operation.", Labels: []string{"drive", "op"}})
	if err != nil {
		return nil, err
	}
	seconds, err := reg.Counter(ctx, Definition{Subsystem: "drive", Name: "operation_seconds_total",
		Help:   "Time spent in the operations on the local drives, by drive and operation (divided by operations_total: the average latency).",
		Labels: []string{"drive", "op"}})
	if err != nil {
		return nil, err
	}
	errs, err := reg.Counter(ctx, Definition{Subsystem: "drive", Name: "errors_total",
		Help:   "Failed operations on the local drives, by drive and type of error (offline, faulty, corrupt, full, timeout, format, other).",
		Labels: []string{"drive", "type"}})
	if err != nil {
		return nil, err
	}
	return &driveInstruments{ops: ops, seconds: seconds, errors: errs}, nil
}

// WrapDrive returns d measured: the count and duration of every
// operation, and its failures by type (a missing object or version, an
// invalid argument and a canceled call are not failures of the drive).
// The pure accessors (Endpoint, IsOnline, Healing) pass through.
func WrapDrive(ctx context.Context, d storage.StorageAPI, reg Registry) (storage.StorageAPI, error) {
	if d == nil || reg == nil {
		return nil, fmt.Errorf("%w: metrics: the drive decorator needs a drive and a registry", domain.ErrInvalidArgument)
	}
	m, err := newDriveInstruments(ctx, reg)
	if err != nil {
		return nil, fmt.Errorf("metrics: drive: %w", err)
	}
	return &measuredDrive{next: d, m: m, drive: d.Endpoint().String()}, nil
}

// DriveErrorType classifies a drive error for the label type of
// nexo_drive_errors_total; "" for the errors that are not failures of the
// drive.
func DriveErrorType(err error) string {
	switch {
	case err == nil, errors.Is(err, context.Canceled),
		errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrBucketNotFound),
		errors.Is(err, domain.ErrNoSuchVersion), errors.Is(err, domain.ErrBucketNotEmpty),
		errors.Is(err, domain.ErrBucketAlreadyExists), errors.Is(err, domain.ErrInvalidArgument),
		errors.Is(err, domain.ErrInvalidRange), errors.Is(err, domain.ErrInvalidObjectName),
		errors.Is(err, domain.ErrInvalidBucketName), errors.Is(err, domain.ErrAccessDenied),
		errors.Is(err, domain.ErrNotFound):
		return ""
	case errors.Is(err, storage.ErrFileCorrupt):
		return "corrupt"
	case errors.Is(err, domain.ErrFaultyDisk):
		return "faulty"
	case errors.Is(err, domain.ErrDiskNotFound), errors.Is(err, domain.ErrNodeOffline):
		return "offline"
	case errors.Is(err, domain.ErrStorageFull):
		return "full"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, domain.ErrInvalidFormat), errors.Is(err, domain.ErrFormatMismatch):
		return "format"
	default:
		return "other"
	}
}

// measuredDrive is the decorator of WrapDrive.
type measuredDrive struct {
	next  storage.StorageAPI
	m     *driveInstruments
	drive string
}

var _ storage.StorageAPI = (*measuredDrive)(nil)

// done records an operation that started at start and ended with err.
func (d *measuredDrive) done(ctx context.Context, op string, start time.Time, err error) {
	d.m.ops.Add(ctx, 1, d.drive, op)
	d.m.seconds.Add(ctx, time.Since(start).Seconds(), d.drive, op)
	if t := DriveErrorType(err); t != "" {
		d.m.errors.Add(ctx, 1, d.drive, t)
	}
}

// Endpoint implements storage.StorageAPI.
func (d *measuredDrive) Endpoint() domain.Endpoint { return d.next.Endpoint() }

// IsOnline implements storage.StorageAPI.
func (d *measuredDrive) IsOnline() bool { return d.next.IsOnline() }

// Healing implements storage.StorageAPI.
func (d *measuredDrive) Healing() *storage.HealingTracker { return d.next.Healing() }

// DiskInfo implements storage.StorageAPI.
func (d *measuredDrive) DiskInfo(ctx context.Context, opts storage.DiskInfoOptions) (domain.DiskInfo, error) {
	start := time.Now()
	di, err := d.next.DiskInfo(ctx, opts)
	d.done(ctx, "disk_info", start, err)
	return di, err
}

// MakeVol implements storage.StorageAPI.
func (d *measuredDrive) MakeVol(ctx context.Context, volume string) error {
	start := time.Now()
	err := d.next.MakeVol(ctx, volume)
	d.done(ctx, "make_vol", start, err)
	return err
}

// ListVols implements storage.StorageAPI.
func (d *measuredDrive) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	start := time.Now()
	v, err := d.next.ListVols(ctx)
	d.done(ctx, "list_vols", start, err)
	return v, err
}

// StatVol implements storage.StorageAPI.
func (d *measuredDrive) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	start := time.Now()
	v, err := d.next.StatVol(ctx, volume)
	d.done(ctx, "stat_vol", start, err)
	return v, err
}

// DeleteVol implements storage.StorageAPI.
func (d *measuredDrive) DeleteVol(ctx context.Context, volume string, force bool) error {
	start := time.Now()
	err := d.next.DeleteVol(ctx, volume, force)
	d.done(ctx, "delete_vol", start, err)
	return err
}

// ReadVersion implements storage.StorageAPI.
func (d *measuredDrive) ReadVersion(ctx context.Context, volume, path, versionID string, opts storage.ReadOptions) (storage.FileInfo, error) {
	start := time.Now()
	fi, err := d.next.ReadVersion(ctx, volume, path, versionID, opts)
	d.done(ctx, "read_version", start, err)
	return fi, err
}

// WriteMetadata implements storage.StorageAPI.
func (d *measuredDrive) WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	start := time.Now()
	err := d.next.WriteMetadata(ctx, volume, path, fi)
	d.done(ctx, "write_metadata", start, err)
	return err
}

// UpdateMetadata implements storage.StorageAPI.
func (d *measuredDrive) UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo, opts storage.UpdateMetadataOptions) error {
	start := time.Now()
	err := d.next.UpdateMetadata(ctx, volume, path, fi, opts)
	d.done(ctx, "update_metadata", start, err)
	return err
}

// DeleteVersion implements storage.StorageAPI.
func (d *measuredDrive) DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo, opts storage.DeleteOptions) error {
	start := time.Now()
	err := d.next.DeleteVersion(ctx, volume, path, fi, opts)
	d.done(ctx, "delete_version", start, err)
	return err
}

// CreateFile implements storage.StorageAPI.
func (d *measuredDrive) CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error {
	start := time.Now()
	err := d.next.CreateFile(ctx, volume, path, size, r)
	d.done(ctx, "create_file", start, err)
	return err
}

// ReadFileStream implements storage.StorageAPI (the time to open the
// stream).
func (d *measuredDrive) ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	start := time.Now()
	rc, err := d.next.ReadFileStream(ctx, volume, path, offset, length)
	d.done(ctx, "read_file_stream", start, err)
	return rc, err
}

// OpenFile implements storage.StorageAPI (the time to open the file).
func (d *measuredDrive) OpenFile(ctx context.Context, volume, path string) (storage.FileHandle, error) {
	start := time.Now()
	h, err := d.next.OpenFile(ctx, volume, path)
	d.done(ctx, "open_file", start, err)
	return h, err
}

// RenameData implements storage.StorageAPI.
func (d *measuredDrive) RenameData(ctx context.Context, srcVolume, srcPath string, fi storage.FileInfo, dstVolume, dstPath string, opts storage.RenameOptions) (storage.RenameDataResult, error) {
	start := time.Now()
	res, err := d.next.RenameData(ctx, srcVolume, srcPath, fi, dstVolume, dstPath, opts)
	d.done(ctx, "rename_data", start, err)
	return res, err
}

// RenameFile implements storage.StorageAPI.
func (d *measuredDrive) RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error {
	start := time.Now()
	err := d.next.RenameFile(ctx, srcVolume, srcPath, dstVolume, dstPath)
	d.done(ctx, "rename_file", start, err)
	return err
}

// WalkDir implements storage.StorageAPI.
func (d *measuredDrive) WalkDir(ctx context.Context, opts storage.WalkDirOptions, fn storage.WalkDirFunc) error {
	start := time.Now()
	err := d.next.WalkDir(ctx, opts, fn)
	d.done(ctx, "walk_dir", start, err)
	return err
}

// ReadAll implements storage.StorageAPI.
func (d *measuredDrive) ReadAll(ctx context.Context, volume, path string) ([]byte, error) {
	start := time.Now()
	b, err := d.next.ReadAll(ctx, volume, path)
	d.done(ctx, "read_all", start, err)
	return b, err
}

// WriteAll implements storage.StorageAPI.
func (d *measuredDrive) WriteAll(ctx context.Context, volume, path string, b []byte) error {
	start := time.Now()
	err := d.next.WriteAll(ctx, volume, path, b)
	d.done(ctx, "write_all", start, err)
	return err
}

// Delete implements storage.StorageAPI.
func (d *measuredDrive) Delete(ctx context.Context, volume, path string, opts storage.DeleteOptions) error {
	start := time.Now()
	err := d.next.Delete(ctx, volume, path, opts)
	d.done(ctx, "delete", start, err)
	return err
}

// CheckParts implements storage.StorageAPI.
func (d *measuredDrive) CheckParts(ctx context.Context, volume, path string, fi storage.FileInfo) (*storage.CheckPartsResult, error) {
	start := time.Now()
	res, err := d.next.CheckParts(ctx, volume, path, fi)
	d.done(ctx, "check_parts", start, err)
	return res, err
}

// StatInfoFile implements storage.StorageAPI.
func (d *measuredDrive) StatInfoFile(ctx context.Context, volume, path string, glob bool) ([]storage.StatInfo, error) {
	start := time.Now()
	res, err := d.next.StatInfoFile(ctx, volume, path, glob)
	d.done(ctx, "stat_info_file", start, err)
	return res, err
}

// Close implements storage.StorageAPI.
func (d *measuredDrive) Close(ctx context.Context) error { return d.next.Close(ctx) }
