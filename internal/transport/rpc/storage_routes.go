package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Arguments and answers of the storage service. Each call names the drive
// by its path in the "drive" query parameter.
type (
	volArgs struct {
		Volume string
	}
	deleteVolArgs struct {
		Volume string
		Force  bool
	}
	diskInfoReply struct {
		Info domain.DiskInfo
		Err  *wireError
	}
	readVersionArgs struct {
		Volume, Path, VersionID string
		Opts                    storage.ReadOptions
	}
	writeMetadataArgs struct {
		Volume, Path string
		FI           storage.FileInfo
	}
	updateMetadataArgs struct {
		Volume, Path string
		FI           storage.FileInfo
		Opts         storage.UpdateMetadataOptions
	}
	deleteVersionArgs struct {
		Volume, Path string
		FI           storage.FileInfo
		Opts         storage.DeleteOptions
	}
	createFileArgs struct {
		Volume, Path string
		Size         int64
	}
	readFileStreamArgs struct {
		Volume, Path   string
		Offset, Length int64
	}
	renameDataArgs struct {
		SrcVolume, SrcPath string
		FI                 storage.FileInfo
		DstVolume, DstPath string
		Opts               storage.RenameOptions
	}
	renameFileArgs struct {
		SrcVolume, SrcPath, DstVolume, DstPath string
	}
	walkDirArgs struct {
		Opts storage.WalkDirOptions
	}
	pathArgs struct {
		Volume, Path string
	}
	writeAllArgs struct {
		Volume, Path string
		Data         []byte
	}
	deleteArgs struct {
		Volume, Path string
		Opts         storage.DeleteOptions
	}
	checkPartsArgs struct {
		Volume, Path string
		FI           storage.FileInfo
	}
	statInfoFileArgs struct {
		Volume, Path string
		Glob         bool
	}
	// healingReply carries the healing tracker as the JSON of healing.bin,
	// so that the client decodes it exactly like the local drive does.
	healingReply struct {
		Tracker []byte
	}
	empty struct{}
)

// driveParam is the query parameter naming the drive of a storage call.
const driveParam = "drive"

// StorageRoutesDeps are the dependencies of the storage routes.
type StorageRoutesDeps struct {
	// Drives are the local drives of the node, addressed by their path.
	Drives []storage.StorageAPI
	// Auth, when set, is bound to the deployment of the format a peer
	// writes on a drive of this node while it does not know its own (the
	// node formatting a fresh deployment): from then on this node checks
	// the deployment of its callers, before its own bootstrap reads the
	// formats.
	Auth Authenticator
}

// storageRoutes implements StorageRoutes.
type storageRoutes struct {
	drives map[string]storage.StorageAPI
	auth   Authenticator
}

var _ StorageRoutes = (*storageRoutes)(nil)

// NewStorageRoutes exposes the local drives to the other nodes.
func NewStorageRoutes(deps StorageRoutesDeps, opts ...StorageRoutesOption) (StorageRoutes, error) {
	_ = apply(opts)
	h := &storageRoutes{drives: make(map[string]storage.StorageAPI, len(deps.Drives)), auth: deps.Auth}
	for _, d := range deps.Drives {
		if d == nil {
			continue
		}
		path := d.Endpoint().Path
		if _, dup := h.drives[path]; dup {
			return nil, fmt.Errorf("%w: rpc: drive %s exposed twice", domain.ErrInvalidArgument, path)
		}
		h.drives[path] = d
	}
	return h, nil
}

// drive resolves the drive of a call.
func (h *storageRoutes) drive(ctx context.Context) (storage.StorageAPI, error) {
	path, _ := ctx.Value(driveKey{}).(string)
	d, ok := h.drives[path]
	if !ok {
		return nil, fmt.Errorf("%w: drive %q is not served by this node", domain.ErrDiskNotFound, path)
	}
	return d, nil
}

type driveKey struct{}

// withDrive puts the drive named by the query in the context of the call.
func withDrive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), driveKey{}, r.URL.Query().Get(driveParam))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// on adapts a storage method to the unary shape.
func on[In, Out any](h *storageRoutes, fn func(ctx context.Context, d storage.StorageAPI, in In) (Out, error)) http.HandlerFunc {
	return unary(func(ctx context.Context, in In) (Out, error) {
		d, err := h.drive(ctx)
		if err != nil {
			var zero Out
			return zero, err
		}
		return fn(ctx, d, in)
	})
}

// Register implements Routes.
func (h *storageRoutes) Register(r chi.Router) {
	r.Route("/storage", func(r chi.Router) {
		r.Use(withDrive)
		r.Post("/disk-info", unary(func(ctx context.Context, opts storage.DiskInfoOptions) (diskInfoReply, error) {
			d, err := h.drive(ctx)
			if err != nil {
				return diskInfoReply{}, err
			}
			info, err := d.DiskInfo(ctx, opts)
			reply := diskInfoReply{Info: info}
			if err != nil {
				we, _ := encodeError(err)
				reply.Err = &we
			}
			return reply, nil
		}))
		r.Post("/healing", on(h, func(_ context.Context, d storage.StorageAPI, _ empty) (healingReply, error) {
			t := d.Healing()
			if t == nil {
				return healingReply{}, nil
			}
			raw, err := json.Marshal(t)
			return healingReply{Tracker: raw}, err
		}))
		r.Post("/make-vol", on(h, func(ctx context.Context, d storage.StorageAPI, in volArgs) (empty, error) {
			return empty{}, d.MakeVol(ctx, in.Volume)
		}))
		r.Post("/list-vols", on(h, func(ctx context.Context, d storage.StorageAPI, _ empty) ([]storage.VolInfo, error) {
			return d.ListVols(ctx)
		}))
		r.Post("/stat-vol", on(h, func(ctx context.Context, d storage.StorageAPI, in volArgs) (storage.VolInfo, error) {
			return d.StatVol(ctx, in.Volume)
		}))
		r.Post("/delete-vol", on(h, func(ctx context.Context, d storage.StorageAPI, in deleteVolArgs) (empty, error) {
			return empty{}, d.DeleteVol(ctx, in.Volume, in.Force)
		}))
		r.Post("/read-version", on(h, func(ctx context.Context, d storage.StorageAPI, in readVersionArgs) (storage.FileInfo, error) {
			return d.ReadVersion(ctx, in.Volume, in.Path, in.VersionID, in.Opts)
		}))
		r.Post("/write-metadata", on(h, func(ctx context.Context, d storage.StorageAPI, in writeMetadataArgs) (empty, error) {
			return empty{}, d.WriteMetadata(ctx, in.Volume, in.Path, in.FI)
		}))
		r.Post("/update-metadata", on(h, func(ctx context.Context, d storage.StorageAPI, in updateMetadataArgs) (empty, error) {
			return empty{}, d.UpdateMetadata(ctx, in.Volume, in.Path, in.FI, in.Opts)
		}))
		r.Post("/delete-version", on(h, func(ctx context.Context, d storage.StorageAPI, in deleteVersionArgs) (empty, error) {
			return empty{}, d.DeleteVersion(ctx, in.Volume, in.Path, in.FI, in.Opts)
		}))
		r.Post("/create-file", upload(func(ctx context.Context, in createFileArgs, body io.Reader) (empty, error) {
			d, err := h.drive(ctx)
			if err != nil {
				return empty{}, err
			}
			return empty{}, d.CreateFile(ctx, in.Volume, in.Path, in.Size, body)
		}))
		r.Post("/read-file-stream", download(func(ctx context.Context, in readFileStreamArgs) (io.ReadCloser, error) {
			d, err := h.drive(ctx)
			if err != nil {
				return nil, err
			}
			return d.ReadFileStream(ctx, in.Volume, in.Path, in.Offset, in.Length)
		}))
		r.Post("/rename-data", on(h, func(ctx context.Context, d storage.StorageAPI, in renameDataArgs) (storage.RenameDataResult, error) {
			return d.RenameData(ctx, in.SrcVolume, in.SrcPath, in.FI, in.DstVolume, in.DstPath, in.Opts)
		}))
		r.Post("/rename-file", on(h, func(ctx context.Context, d storage.StorageAPI, in renameFileArgs) (empty, error) {
			return empty{}, d.RenameFile(ctx, in.SrcVolume, in.SrcPath, in.DstVolume, in.DstPath)
		}))
		r.Post("/walk-dir", stream(func(ctx context.Context, in walkDirArgs, emit func(storage.WalkEntry) error) error {
			d, err := h.drive(ctx)
			if err != nil {
				return err
			}
			return d.WalkDir(ctx, in.Opts, func(e storage.WalkEntry) error { return emit(e) })
		}))
		r.Post("/read-all", on(h, func(ctx context.Context, d storage.StorageAPI, in pathArgs) ([]byte, error) {
			if restricted(ctx) && (in.Volume != domain.SystemBucket || in.Path != storage.FormatFile) {
				// A node bootstrapping reads the format, nothing else.
				return nil, errRestricted
			}
			return d.ReadAll(ctx, in.Volume, in.Path)
		}))
		r.Post("/write-all", on(h, func(ctx context.Context, d storage.StorageAPI, in writeAllArgs) (empty, error) {
			if restricted(ctx) && (in.Volume != domain.SystemBucket || (in.Path != storage.FormatFile && in.Path != storage.HealingFile)) {
				// A node without deployment receives its format, nothing else.
				return empty{}, errRestricted
			}
			if err := d.WriteAll(ctx, in.Volume, in.Path, in.Data); err != nil {
				return empty{}, err
			}
			h.bindFormat(ctx, in)
			return empty{}, nil
		}))
		r.Post("/delete", on(h, func(ctx context.Context, d storage.StorageAPI, in deleteArgs) (empty, error) {
			return empty{}, d.Delete(ctx, in.Volume, in.Path, in.Opts)
		}))
		r.Post("/check-parts", on(h, func(ctx context.Context, d storage.StorageAPI, in checkPartsArgs) (*storage.CheckPartsResult, error) {
			return d.CheckParts(ctx, in.Volume, in.Path, in.FI)
		}))
		r.Post("/stat-info-file", on(h, func(ctx context.Context, d storage.StorageAPI, in statInfoFileArgs) ([]storage.StatInfo, error) {
			if restricted(ctx) && (in.Volume != domain.SystemBucket || in.Path != storage.FormatFile || in.Glob) {
				return nil, errRestricted
			}
			return d.StatInfoFile(ctx, in.Volume, in.Path, in.Glob)
		}))
	})
}

// bindFormat binds the node to the deployment of the format a peer wrote
// on one of its drives while the node did not know its deployment.
func (h *storageRoutes) bindFormat(ctx context.Context, in writeAllArgs) {
	c, ok := CallerFromContext(ctx)
	if h.auth == nil || !ok || !c.Unverified || in.Volume != domain.SystemBucket || in.Path != storage.FormatFile {
		return
	}
	if f, err := topology.UnmarshalFormat(in.Data); err == nil && f.ID != "" {
		h.auth.SetDeploymentID(ctx, f.ID)
	}
}

// errRestricted refuses the calls a caller whose deployment is not
// established may not make (Claims.Unbound, Claims.Unverified).
var errRestricted = fmt.Errorf("%w: rpc: the deployment of the call is not established", domain.ErrAccessDenied)

// restricted reports a call whose deployment is not established: the
// caller has none, or this node does not know its own yet.
func restricted(ctx context.Context) bool {
	c, ok := CallerFromContext(ctx)
	return !ok || c.Unbound || c.Unverified
}
