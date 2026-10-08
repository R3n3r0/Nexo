package object_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

// renameDataFunc has the signature of StorageAPI.RenameData.
type renameDataFunc func(ctx context.Context, srcVolume, srcPath string, fi storage.FileInfo, dstVolume, dstPath string, opts storage.RenameOptions) (storage.RenameDataResult, error)

// delegatingDrive returns the generated StorageAPI mock forwarding every
// call to the base drive, except RenameData, which goes through intercept
// (when it returns handled) before reaching the drive: the fault injection
// point of the commit.
func delegatingDrive(t *testing.T, base storage.StorageAPI, intercept func(next renameDataFunc) renameDataFunc) *storagemock.StorageAPI {
	t.Helper()
	m := storagemock.NewStorageAPI(t)
	m.EXPECT().Endpoint().RunAndReturn(base.Endpoint).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(base.IsOnline).Maybe()
	m.EXPECT().Healing().RunAndReturn(base.Healing).Maybe()
	m.EXPECT().DiskInfo(mock.Anything, mock.Anything).RunAndReturn(base.DiskInfo).Maybe()
	m.EXPECT().MakeVol(mock.Anything, mock.Anything).RunAndReturn(base.MakeVol).Maybe()
	m.EXPECT().ListVols(mock.Anything).RunAndReturn(base.ListVols).Maybe()
	m.EXPECT().StatVol(mock.Anything, mock.Anything).RunAndReturn(base.StatVol).Maybe()
	m.EXPECT().DeleteVol(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.DeleteVol).Maybe()
	m.EXPECT().ReadVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadVersion).Maybe()
	m.EXPECT().WriteMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.WriteMetadata).Maybe()
	m.EXPECT().UpdateMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.UpdateMetadata).Maybe()
	m.EXPECT().DeleteVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.DeleteVersion).Maybe()
	m.EXPECT().CreateFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.CreateFile).Maybe()
	m.EXPECT().ReadFileStream(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadFileStream).Maybe()
	m.EXPECT().RenameData(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		RunAndReturn(intercept(base.RenameData)).Maybe()
	m.EXPECT().RenameFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.RenameFile).Maybe()
	m.EXPECT().WalkDir(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.WalkDir).Maybe()
	m.EXPECT().ReadAll(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.ReadAll).Maybe()
	m.EXPECT().WriteAll(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.WriteAll).Maybe()
	m.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.Delete).Maybe()
	m.EXPECT().CheckParts(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.CheckParts).Maybe()
	m.EXPECT().StatInfoFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.StatInfoFile).Maybe()
	m.EXPECT().Close(mock.Anything).RunAndReturn(base.Close).Maybe()
	return m
}

// commitFault is armed by a test to disturb the next commits: the client
// goes away (cancel) while the data is renamed into place, and the rename
// optionally fails.
type commitFault struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	err    error
}

func (f *commitFault) arm(cancel context.CancelFunc, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancel, f.err = cancel, err
}

func (f *commitFault) intercept(next renameDataFunc) renameDataFunc {
	return func(ctx context.Context, srcVolume, srcPath string, fi storage.FileInfo, dstVolume, dstPath string, opts storage.RenameOptions) (storage.RenameDataResult, error) {
		f.mu.Lock()
		cancel, err := f.cancel, f.err
		f.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if err != nil {
			return storage.RenameDataResult{}, err
		}
		return next(ctx, srcVolume, srcPath, fi, dstVolume, dstPath, opts)
	}
}

// newLayer builds an fs (one drive) or erasure (2+2, NewErasure or
// NewServerPools) layer over drives.
func newLayer(t *testing.T, kind string, drives []storage.StorageAPI) object.Service {
	t.Helper()
	deps := object.Deps{Logger: slog.New(slog.DiscardHandler)}
	opts := []object.Option{object.WithMinPartSize(16), object.WithLockTimeout(5 * time.Second), object.WithBlockSize(testBlockSize)}
	var (
		svc object.Service
		err error
	)
	switch kind {
	case "fs":
		deps.Store = drives[0]
		svc, err = object.NewFS(deps, opts...)
	case "pools":
		svc, err = object.NewServerPools(object.PoolsDeps{
			Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}}, DeploymentID: "deployment-test", Logger: deps.Logger,
		}, opts...)
	default:
		deps.Sets, deps.Parity, deps.DeploymentID = [][]storage.StorageAPI{drives}, 2, "deployment-test"
		svc, err = object.NewErasure(deps, opts...)
	}
	require.NoError(t, err)
	return svc
}

// TestCompleteMultipartSurvivesCanceledClient is a regression test: a
// client going away while CompleteMultipartUpload commits must neither
// lose the parts (a failed commit moves them back into the upload, which a
// retry then completes) nor leave a half-done completion.
func TestCompleteMultipartSurvivesCanceledClient(t *testing.T) {
	errInjected := fmt.Errorf("%w: injected rename failure", domain.ErrFaultyDisk)
	tests := []struct {
		name string
		// fail makes the commit fail as well as the context canceled.
		fail bool
	}{
		{name: "canceled during commit", fail: false},
		{name: "canceled and failed commit", fail: true},
	}
	for _, kind := range implKinds() {
		for _, tt := range tests {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				ctx := context.Background()
				fault := &commitFault{}
				bases, _ := newDrives(t, driveCount(kind))
				drives := make([]storage.StorageAPI, len(bases))
				for i, d := range bases {
					drives[i] = delegatingDrive(t, d, fault.intercept)
				}
				// The parts are uploaded through the drives themselves (the
				// mock records its arguments, which must not be streams
				// written concurrently); the completion goes through the
				// faulty ones.
				setup := newLayer(t, kind, bases)
				svc := newLayer(t, kind, drives)
				require.NoError(t, setup.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
				up, err := setup.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "big"})
				require.NoError(t, err)
				var (
					parts []domain.CompletePart
					want  []byte
				)
				for i := 1; i <= 2; i++ {
					body := payload(testBlockSize + 32*i)
					p, err := setup.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big", UploadID: up.UploadID, PartNumber: i, Reader: bytes.NewReader(body), Size: int64(len(body))})
					require.NoError(t, err)
					parts = append(parts, domain.CompletePart{Number: i, ETag: p.ETag})
					want = append(want, body...)
				}
				in := object.CompleteMultipartInput{Bucket: "bkt", Key: "big", UploadID: up.UploadID, Parts: parts}

				cctx, cancel := context.WithCancel(ctx)
				defer cancel()
				var injected error
				if tt.fail {
					injected = errInjected
				}
				fault.arm(cancel, injected)
				_, err = svc.CompleteMultipartUpload(cctx, in)
				fault.arm(nil, nil)
				if tt.fail {
					require.Error(t, err)
					assert.False(t, errors.Is(err, context.Canceled), "the commit is not abandoned on cancellation: %v", err)
					_, err = svc.GetObjectInfo(ctx, "bkt", "big", object.GetObjectOptions{})
					require.ErrorIs(t, err, domain.ErrObjectNotFound)
					// The client retries: the parts are back in the upload.
					_, err = svc.CompleteMultipartUpload(ctx, in)
					require.NoError(t, err, "retry after a failed completion")
				} else {
					require.NoError(t, err, "a started commit runs to its end")
				}
				data, _ := get(t, svc, "bkt", "big", object.GetObjectOptions{})
				assert.Equal(t, want, data)
				_, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "big", UploadID: up.UploadID})
				assert.ErrorIs(t, err, domain.ErrNoSuchUpload, "the completed upload is gone")
			})
		}
	}
}
