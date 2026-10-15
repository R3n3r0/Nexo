package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Defaults of the remote drive client.
const (
	defaultDiskCheckInterval = 2 * time.Second
	defaultHealingTTL        = 5 * time.Second
	healingFetchTimeout      = 2 * time.Second
)

// StorageClientDeps are the dependencies of the remote drive client.
type StorageClientDeps struct {
	// Conn reaches the node serving the drive (required).
	Conn Conn
	// Boot identifies this process among the runs of the node (a random
	// id drawn at startup): the node of the drive closes the file
	// handles an earlier run opened as soon as this one opens a file.
	Boot   string
	Logger *slog.Logger
}

// StorageClientOption configures the remote drive client.
type StorageClientOption func(*storageClientOptions)

type storageClientOptions struct {
	diskCheck  time.Duration
	healingTTL time.Duration
}

// WithDiskCheckInterval sets how often a drive reported missing by its
// node is checked again (default 2s).
func WithDiskCheckInterval(d time.Duration) StorageClientOption {
	return func(o *storageClientOptions) {
		if d > 0 {
			o.diskCheck = d
		}
	}
}

// WithHealingTTL sets how long the healing state of the drive is cached
// (default 5s).
func WithHealingTTL(d time.Duration) StorageClientOption {
	return func(o *storageClientOptions) {
		if d > 0 {
			o.healingTTL = d
		}
	}
}

// storageClient is storage.StorageAPI for a drive served by another node
// (the storageRESTClient of docs/08 §8.1). Failures to reach the node are
// reported as domain.ErrDiskNotFound (wrapping domain.ErrNodeOffline), so
// that the object layer treats the drive as offline.
type storageClient struct {
	deps StorageClientDeps
	opts storageClientOptions
	ep   domain.Endpoint

	closed atomic.Bool
	// missing is set when the node reports the drive gone; a background
	// check clears it when the drive answers again.
	missing  atomic.Bool
	checking atomic.Bool
	stop     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once

	hmu       sync.Mutex
	healing   *storage.HealingTracker
	healingAt time.Time
	healingOK bool

	// open are the file handles opened on the node and not closed yet:
	// Close closes them before the client.
	openMu sync.Mutex
	open   map[*remoteHandle]struct{}
}

var _ storage.StorageAPI = (*storageClient)(nil)

// NewStorageClient returns the StorageAPI of the drive ep of another node.
// The drive is assumed online until a call fails.
func NewStorageClient(deps StorageClientDeps, ep domain.Endpoint, opts ...StorageClientOption) (storage.StorageAPI, error) {
	if deps.Conn == nil {
		return nil, fmt.Errorf("%w: rpc: the storage client needs a connection", domain.ErrInvalidArgument)
	}
	if ep.Path == "" {
		return nil, fmt.Errorf("%w: rpc: endpoint %q has no drive path", domain.ErrInvalidArgument, ep.String())
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := storageClientOptions{diskCheck: defaultDiskCheckInterval, healingTTL: defaultHealingTTL}
	for _, opt := range opts {
		opt(&o)
	}
	return &storageClient{deps: deps, opts: o, ep: ep, stop: make(chan struct{}), open: map[*remoteHandle]struct{}{}}, nil
}

// Endpoint implements storage.StorageAPI.
func (c *storageClient) Endpoint() domain.Endpoint { return c.ep }

// IsOnline implements storage.StorageAPI: the node is reachable and did
// not report the drive missing.
func (c *storageClient) IsOnline() bool {
	return !c.closed.Load() && c.deps.Conn.IsOnline() && !c.missing.Load()
}

// Close implements storage.StorageAPI: the file handles still open on the
// node are closed first (a node that stops leaves none behind on its
// peers). The connection, shared by the drives of the node, stays open.
func (c *storageClient) Close(ctx context.Context) error {
	c.openMu.Lock()
	handles := make([]*remoteHandle, 0, len(c.open))
	for h := range c.open {
		handles = append(handles, h)
	}
	c.openMu.Unlock()
	if len(handles) > 0 {
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeHandlesTimeout)
		var wg sync.WaitGroup
		for _, h := range handles {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = h.Close(hctx)
			}()
		}
		wg.Wait()
		cancel()
	}
	c.once.Do(func() {
		c.closed.Store(true)
		close(c.stop)
	})
	c.wg.Wait()
	return nil
}

// closeHandlesTimeout bounds the close of the handles of a client that
// closes (an unreachable node keeps them until their idle timeout).
const closeHandlesTimeout = 5 * time.Second

// check fails the calls that cannot be attempted.
func (c *storageClient) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed.Load() {
		return fmt.Errorf("%w: remote drive %s is closed", domain.ErrDiskNotFound, c.ep)
	}
	return nil
}

func (c *storageClient) param() CallOption { return WithParam(driveParam, c.ep.Path) }

// call performs a unary storage call.
func (c *storageClient) call(ctx context.Context, method string, in, out any) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	return c.result(ctx, c.deps.Conn.Call(ctx, "storage/"+method, in, out, c.param()))
}

// result tracks the drive state from the outcome of a call and maps the
// failures to reach the node to an offline drive.
func (c *storageClient) result(ctx context.Context, err error) error {
	switch {
	case err == nil:
		c.missing.Store(false)
		return nil
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return err
	case errors.Is(err, domain.ErrNodeOffline):
		return fmt.Errorf("%w: drive %s: %w", domain.ErrDiskNotFound, c.ep, err)
	case errors.Is(err, domain.ErrDiskNotFound):
		c.markMissing()
	}
	return err
}

// markMissing records a drive reported gone by its node and starts the
// background check that notices its return.
func (c *storageClient) markMissing() {
	c.missing.Store(true)
	if c.closed.Load() || !c.checking.CompareAndSwap(false, true) {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.checking.Store(false)
		ticker := time.NewTicker(c.opts.diskCheck)
		defer ticker.Stop()
		for c.missing.Load() {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
			}
			if !c.deps.Conn.IsOnline() {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), healingFetchTimeout)
			var reply diskInfoReply
			err := c.deps.Conn.Call(ctx, "storage/disk-info", storage.DiskInfoOptions{NoOp: true}, &reply, c.param())
			cancel()
			if err == nil && reply.Err == nil {
				c.missing.Store(false)
				c.deps.Logger.Info("rpc: remote drive back online", "drive", c.ep.String())
			}
		}
	}()
}

// DiskInfo implements storage.StorageAPI.
func (c *storageClient) DiskInfo(ctx context.Context, opts storage.DiskInfoOptions) (domain.DiskInfo, error) {
	var reply diskInfoReply
	if err := c.call(ctx, "disk-info", opts, &reply); err != nil {
		return domain.DiskInfo{Endpoint: c.ep.String(), Path: c.ep.Path, PoolIdx: c.ep.PoolIdx, SetIdx: c.ep.SetIdx,
			DiskIdx: c.ep.DiskIdx, State: domain.StateOffline, Error: err.Error()}, err
	}
	if !opts.NoOp {
		c.invalidateHealing()
	}
	info := reply.Info
	info.Endpoint = c.ep.String()
	info.PoolIdx, info.SetIdx, info.DiskIdx = c.ep.PoolIdx, c.ep.SetIdx, c.ep.DiskIdx
	if reply.Err != nil {
		return info, c.result(ctx, decodeError(c.deps.Conn.Node(), *reply.Err))
	}
	return info, nil
}

// MakeVol implements storage.StorageAPI.
func (c *storageClient) MakeVol(ctx context.Context, volume string) error {
	return c.call(ctx, "make-vol", volArgs{Volume: volume}, nil)
}

// ListVols implements storage.StorageAPI.
func (c *storageClient) ListVols(ctx context.Context) ([]storage.VolInfo, error) {
	var vols []storage.VolInfo
	err := c.call(ctx, "list-vols", empty{}, &vols)
	return vols, err
}

// StatVol implements storage.StorageAPI.
func (c *storageClient) StatVol(ctx context.Context, volume string) (storage.VolInfo, error) {
	var vi storage.VolInfo
	err := c.call(ctx, "stat-vol", volArgs{Volume: volume}, &vi)
	return vi, err
}

// DeleteVol implements storage.StorageAPI.
func (c *storageClient) DeleteVol(ctx context.Context, volume string, force bool) error {
	return c.call(ctx, "delete-vol", deleteVolArgs{Volume: volume, Force: force}, nil)
}

// ReadVersion implements storage.StorageAPI.
func (c *storageClient) ReadVersion(ctx context.Context, volume, path, versionID string, opts storage.ReadOptions) (storage.FileInfo, error) {
	var fi storage.FileInfo
	if err := c.call(ctx, "read-version", readVersionArgs{Volume: volume, Path: path, VersionID: versionID, Opts: opts}, &fi); err != nil {
		return storage.FileInfo{}, err
	}
	normalizeFileInfo(&fi)
	return fi, nil
}

// WriteMetadata implements storage.StorageAPI.
func (c *storageClient) WriteMetadata(ctx context.Context, volume, path string, fi storage.FileInfo) error {
	return c.call(ctx, "write-metadata", writeMetadataArgs{Volume: volume, Path: path, FI: fi}, nil)
}

// UpdateMetadata implements storage.StorageAPI.
func (c *storageClient) UpdateMetadata(ctx context.Context, volume, path string, fi storage.FileInfo, opts storage.UpdateMetadataOptions) error {
	return c.call(ctx, "update-metadata", updateMetadataArgs{Volume: volume, Path: path, FI: fi, Opts: opts}, nil)
}

// DeleteVersion implements storage.StorageAPI.
func (c *storageClient) DeleteVersion(ctx context.Context, volume, path string, fi storage.FileInfo, opts storage.DeleteOptions) error {
	return c.call(ctx, "delete-version", deleteVersionArgs{Volume: volume, Path: path, FI: fi, Opts: opts}, nil)
}

// CreateFile implements storage.StorageAPI: the content is streamed in
// checksummed frames; a known size bounds what is read from r, as the
// local drive does.
func (c *storageClient) CreateFile(ctx context.Context, volume, path string, size int64, r io.Reader) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if size >= 0 {
		r = io.LimitReader(r, size)
	}
	err := c.deps.Conn.Upload(ctx, "storage/create-file", createFileArgs{Volume: volume, Path: path, Size: size}, r, nil, c.param())
	return c.result(ctx, err)
}

// ReadFileStream implements storage.StorageAPI.
func (c *storageClient) ReadFileStream(ctx context.Context, volume, path string, offset, length int64) (io.ReadCloser, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	rc, err := c.deps.Conn.Download(ctx, "storage/read-file-stream", readFileStreamArgs{Volume: volume, Path: path, Offset: offset, Length: length}, c.param())
	if err := c.result(ctx, err); err != nil {
		return nil, err
	}
	return rc, nil
}

// OpenFile implements storage.StorageAPI: the node of the drive keeps the
// file open (no data is transferred) until Close or its idle timeout.
func (c *storageClient) OpenFile(ctx context.Context, volume, path string) (storage.FileHandle, error) {
	if err := c.check(ctx); err != nil {
		return nil, err
	}
	var reply openFileReply
	if err := c.call(ctx, "open-file", openFileArgs{Volume: volume, Path: path, Boot: c.deps.Boot}, &reply); err != nil {
		return nil, err
	}
	h := &remoteHandle{c: c, id: reply.Handle, size: reply.Size, name: volume + "/" + path}
	c.openMu.Lock()
	c.open[h] = struct{}{}
	c.openMu.Unlock()
	return h, nil
}

// remoteHandle is a file opened on the node of a remote drive.
type remoteHandle struct {
	c    *storageClient
	id   string
	size int64
	name string
	once sync.Once
}

var _ storage.FileHandle = (*remoteHandle)(nil)

// Size implements storage.FileHandle.
func (h *remoteHandle) Size() int64 { return h.size }

// ReadStream implements storage.FileHandle.
func (h *remoteHandle) ReadStream(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	if err := h.c.check(ctx); err != nil {
		return nil, err
	}
	switch {
	case offset < 0 || offset > h.size:
		return nil, fmt.Errorf("%w: offset %d outside %s (%d bytes)", domain.ErrInvalidRange, offset, h.name, h.size)
	case length > h.size-offset:
		return nil, fmt.Errorf("%w: %s has %d bytes, want [%d, %d)", storage.ErrFileCorrupt, h.name, h.size, offset, offset+length)
	}
	rc, err := h.c.deps.Conn.Download(ctx, "storage/read-handle", readHandleArgs{Handle: h.id, Offset: offset, Length: length}, h.c.param())
	if err := h.c.result(ctx, err); err != nil {
		return nil, err
	}
	return rc, nil
}

// Close implements storage.FileHandle.
func (h *remoteHandle) Close(ctx context.Context) error {
	var err error
	h.once.Do(func() {
		h.c.openMu.Lock()
		delete(h.c.open, h)
		h.c.openMu.Unlock()
		if h.c.closed.Load() {
			return
		}
		err = h.c.call(ctx, "close-handle", handleArgs{Handle: h.id}, nil)
	})
	return err
}

// RenameData implements storage.StorageAPI.
func (c *storageClient) RenameData(ctx context.Context, srcVolume, srcPath string, fi storage.FileInfo, dstVolume, dstPath string, opts storage.RenameOptions) (storage.RenameDataResult, error) {
	var res storage.RenameDataResult
	err := c.call(ctx, "rename-data", renameDataArgs{SrcVolume: srcVolume, SrcPath: srcPath, FI: fi, DstVolume: dstVolume, DstPath: dstPath, Opts: opts}, &res)
	return res, err
}

// RenameFile implements storage.StorageAPI.
func (c *storageClient) RenameFile(ctx context.Context, srcVolume, srcPath, dstVolume, dstPath string) error {
	return c.call(ctx, "rename-file", renameFileArgs{SrcVolume: srcVolume, SrcPath: srcPath, DstVolume: dstVolume, DstPath: dstPath}, nil)
}

// WalkDir implements storage.StorageAPI. The node streams the walk; the
// control the callback exerts is applied here with the semantics of the
// local walk: fs.SkipDir on a prefix skips its content, on an object the
// rest of its directory; fs.SkipAll, an error or the Limit end the walk
// (closing the stream stops the node). Limit counts the objects the
// callback accepted, so it is enforced here rather than by the node.
func (c *storageClient) WalkDir(ctx context.Context, opts storage.WalkDirOptions, fn storage.WalkDirFunc) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if fn == nil {
		return fmt.Errorf("%w: nil walk function", domain.ErrInvalidArgument)
	}
	remote := opts
	remote.Limit = 0
	st, err := c.deps.Conn.Stream(ctx, "storage/walk-dir", walkDirArgs{Opts: remote}, c.param())
	if err := c.result(ctx, err); err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	base := ""
	if b := strings.Trim(opts.BaseDir, "/"); b != "" {
		base = b + "/"
	}
	skip, skipping, count := "", false, 0
	for {
		var e storage.WalkEntry
		err := st.Next(&e)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			return c.result(ctx, err)
		}
		if skipping && strings.HasPrefix(e.Name, skip) {
			continue
		}
		skipping = false
		ferr := fn(e)
		switch {
		case ferr == nil:
			if e.IsDir {
				continue
			}
			count++
			if opts.Limit > 0 && count >= opts.Limit {
				return nil
			}
		case errors.Is(ferr, fs.SkipDir):
			if e.IsDir {
				if opts.Recursive {
					skip, skipping = e.Name, true
				}
				continue
			}
			skip, skipping = parentPrefix(e.Name), true
			if skip == base {
				return nil // the rest of the walked directory: the walk is over
			}
		case errors.Is(ferr, fs.SkipAll):
			return nil
		default:
			return ferr
		}
	}
}

// parentPrefix returns the directory (with its trailing slash, "" at the
// root) holding an object entry; a directory object "a/b/" lives in "a/".
func parentPrefix(name string) string {
	name = strings.TrimSuffix(name, "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[:i+1]
	}
	return ""
}

// ReadAll implements storage.StorageAPI.
func (c *storageClient) ReadAll(ctx context.Context, volume, path string) ([]byte, error) {
	var data []byte
	err := c.call(ctx, "read-all", pathArgs{Volume: volume, Path: path}, &data)
	return data, err
}

// WriteAll implements storage.StorageAPI.
func (c *storageClient) WriteAll(ctx context.Context, volume, path string, b []byte) error {
	err := c.call(ctx, "write-all", writeAllArgs{Volume: volume, Path: path, Data: b}, nil)
	if isHealingFile(volume, path) {
		c.invalidateHealing()
	}
	return err
}

// Delete implements storage.StorageAPI.
func (c *storageClient) Delete(ctx context.Context, volume, path string, opts storage.DeleteOptions) error {
	err := c.call(ctx, "delete", deleteArgs{Volume: volume, Path: path, Opts: opts}, nil)
	if isHealingFile(volume, path) {
		c.invalidateHealing()
	}
	return err
}

// CheckParts implements storage.StorageAPI.
func (c *storageClient) CheckParts(ctx context.Context, volume, path string, fi storage.FileInfo) (*storage.CheckPartsResult, error) {
	var res storage.CheckPartsResult
	if err := c.call(ctx, "check-parts", checkPartsArgs{Volume: volume, Path: path, FI: fi}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// StatInfoFile implements storage.StorageAPI.
func (c *storageClient) StatInfoFile(ctx context.Context, volume, path string, glob bool) ([]storage.StatInfo, error) {
	var infos []storage.StatInfo
	err := c.call(ctx, "stat-info-file", statInfoFileArgs{Volume: volume, Path: path, Glob: glob}, &infos)
	return infos, err
}

// Healing implements storage.StorageAPI. The state is cached for the
// healing TTL and refreshed after the writes of the tracker; a refresh is
// a short call to the node (bounded by 2s), skipped while the node is
// offline (the cached state is returned).
func (c *storageClient) Healing() *storage.HealingTracker {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	fresh := c.healingOK && time.Since(c.healingAt) < c.opts.healingTTL
	if !fresh && !c.closed.Load() && c.deps.Conn.IsOnline() {
		ctx, cancel := context.WithTimeout(context.Background(), healingFetchTimeout)
		var reply healingReply
		err := c.deps.Conn.Call(ctx, "storage/healing", empty{}, &reply, c.param())
		cancel()
		if err == nil {
			c.healing, c.healingAt, c.healingOK = decodeTracker(reply.Tracker), time.Now(), true
		} else {
			c.deps.Logger.Debug("rpc: cannot read the healing state of a remote drive", "drive", c.ep.String(), "err", err)
		}
	}
	if c.healing == nil {
		return nil
	}
	cp := *c.healing
	return &cp
}

func (c *storageClient) invalidateHealing() {
	c.hmu.Lock()
	c.healingOK = false
	c.hmu.Unlock()
}

// decodeTracker decodes healing.bin like the local drive: an unreadable
// tracker is no tracker.
func decodeTracker(raw []byte) *storage.HealingTracker {
	if len(raw) == 0 {
		return nil
	}
	var t storage.HealingTracker
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	return &t
}

// isHealingFile reports whether volume/path designates the healing tracker.
func isHealingFile(volume, path string) bool {
	return volume == domain.SystemBucket && path == storage.HealingFile
}

// normalizeFileInfo restores the time zone of the decoded times: xl.meta
// stores Unix nanoseconds, which the local drive returns in UTC.
func normalizeFileInfo(fi *storage.FileInfo) {
	fi.ModTime = fi.ModTime.UTC()
	fi.SuccessorModTime = fi.SuccessorModTime.UTC()
	for i := range fi.Parts {
		fi.Parts[i].ModTime = fi.Parts[i].ModTime.UTC()
	}
}
