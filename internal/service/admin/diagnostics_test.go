package admin_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// localDrives opens n drives in temporary directories.
func localDrives(t *testing.T, n int) ([]storage.StorageAPI, []string) {
	t.Helper()
	var drives []storage.StorageAPI
	var roots []string
	for i := 0; i < n; i++ {
		root := filepath.Join(t.TempDir(), "d")
		ep, err := domain.ParseEndpoint(root)
		require.NoError(t, err)
		d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close(context.Background()) })
		drives = append(drives, d)
		roots = append(roots, root)
	}
	return drives, roots
}

// isGzip reports the gzip magic of the pprof format.
func isGzip(b []byte) bool { return len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b }

func TestProfile(t *testing.T) {
	d := admin.NewDiagnostics(admin.DiagnosticsDeps{Logger: slog.New(slog.DiscardHandler)}, admin.WithDiagnosticsNode("node1:9000"))
	ctx := context.Background()
	tests := []struct {
		name    string
		in      admin.ProfileInput
		wantErr error
		sampled bool
	}{
		{name: "heap snapshot", in: admin.ProfileInput{Type: admin.ProfileHeap}},
		{name: "goroutine snapshot ignores the duration", in: admin.ProfileInput{Type: admin.ProfileGoroutine, Duration: time.Hour}},
		{name: "cpu", in: admin.ProfileInput{Type: admin.ProfileCPU, Duration: time.Second}, sampled: true},
		{name: "mutex", in: admin.ProfileInput{Type: admin.ProfileMutex, Duration: time.Second}, sampled: true},
		{name: "block", in: admin.ProfileInput{Type: admin.ProfileBlock, Duration: time.Second}, sampled: true},
		{name: "unknown type", in: admin.ProfileInput{Type: "threadcreate"}, wantErr: domain.ErrInvalidArgument},
		{name: "too short", in: admin.ProfileInput{Type: admin.ProfileCPU, Duration: time.Millisecond}, wantErr: domain.ErrInvalidArgument},
		{name: "too long", in: admin.ProfileInput{Type: admin.ProfileMutex, Duration: admin.MaxProfileDuration + time.Second}, wantErr: domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := d.Profile(ctx, tt.in)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, isGzip(p.Data), "pprof protocol buffers are gzipped")
			assert.Equal(t, "node1:9000", p.Node)
			assert.Equal(t, tt.in.Type, p.Type)
			if tt.sampled {
				assert.Equal(t, tt.in.Duration, p.Duration)
			} else {
				assert.Zero(t, p.Duration)
			}
		})
	}
}

func TestProfileOneAtATime(t *testing.T) {
	d := admin.NewDiagnostics(admin.DiagnosticsDeps{Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for {
			// The probes below may hold the profiler for an instant.
			_, err := d.Profile(ctx, admin.ProfileInput{Type: admin.ProfileCPU, Duration: admin.MaxProfileDuration})
			if !errors.Is(err, domain.ErrConflict) {
				done <- err
				return
			}
		}
	}()
	require.Eventually(t, func() bool {
		_, err := d.Profile(context.Background(), admin.ProfileInput{Type: admin.ProfileHeap})
		return err != nil
	}, 5*time.Second, 10*time.Millisecond)
	_, err := d.Profile(context.Background(), admin.ProfileInput{Type: admin.ProfileGoroutine})
	require.ErrorIs(t, err, domain.ErrConflict, "the runtime sampling is global: one profile at a time")
	// A cancelled request returns the profile collected so far.
	cancel()
	require.NoError(t, <-done)
	_, err = d.Profile(context.Background(), admin.ProfileInput{Type: admin.ProfileHeap})
	require.NoError(t, err, "free again")
}

func TestSpeedtestLimits(t *testing.T) {
	drives, _ := localDrives(t, 1)
	d := admin.NewDiagnostics(admin.DiagnosticsDeps{Drives: drives, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()
	tests := []struct {
		name    string
		in      admin.SpeedtestInput
		wantErr error
	}{
		{name: "unknown kind", in: admin.SpeedtestInput{Kind: "gpu"}, wantErr: domain.ErrInvalidArgument},
		{name: "object too large", in: admin.SpeedtestInput{Kind: admin.SpeedtestDrive, Size: admin.MaxSpeedtestSize + 1}, wantErr: domain.ErrInvalidArgument},
		{name: "negative size", in: admin.SpeedtestInput{Kind: admin.SpeedtestDrive, Size: -1}, wantErr: domain.ErrInvalidArgument},
		{name: "too long", in: admin.SpeedtestInput{Kind: admin.SpeedtestDrive, Duration: admin.MaxSpeedtestDuration + time.Second}, wantErr: domain.ErrInvalidArgument},
		{name: "too short", in: admin.SpeedtestInput{Kind: admin.SpeedtestDrive, Duration: time.Millisecond}, wantErr: domain.ErrInvalidArgument},
		{name: "too concurrent", in: admin.SpeedtestInput{Kind: admin.SpeedtestObject, Concurrency: admin.MaxSpeedtestConcurrency + 1}, wantErr: domain.ErrInvalidArgument},
		{name: "net out of a cluster", in: admin.SpeedtestInput{Kind: admin.SpeedtestNet, Duration: 100 * time.Millisecond}, wantErr: domain.ErrNotImplemented},
		{name: "object without object layer", in: admin.SpeedtestInput{Kind: admin.SpeedtestObject, Duration: 100 * time.Millisecond}, wantErr: domain.ErrNotImplemented},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.Speedtest(ctx, tt.in)
			require.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestSpeedtestDrivesAndNet(t *testing.T) {
	drives, roots := localDrives(t, 2)
	peers, peerRoots := localDrives(t, 1)
	d := admin.NewDiagnostics(admin.DiagnosticsDeps{Drives: drives, Peers: peers, Logger: slog.New(slog.DiscardHandler)})
	ctx := context.Background()
	for _, kind := range []admin.SpeedtestKind{admin.SpeedtestDrive, admin.SpeedtestNet} {
		t.Run(string(kind), func(t *testing.T) {
			res, err := d.Speedtest(ctx, admin.SpeedtestInput{Kind: kind, Size: 64 << 10, Duration: 150 * time.Millisecond, Concurrency: 2})
			require.NoError(t, err)
			want := len(drives)
			if kind == admin.SpeedtestNet {
				want = 1
			}
			require.Len(t, res.Targets, want)
			for _, tg := range res.Targets {
				assert.Empty(t, tg.Error)
				assert.Positive(t, tg.Write.Operations, "%s writes", tg.Target)
				assert.Positive(t, tg.Read.Operations, "%s reads", tg.Target)
				assert.Equal(t, tg.Write.Operations*(64<<10), tg.Write.Bytes)
				assert.Positive(t, tg.Write.BytesPerSecond)
				assert.LessOrEqual(t, tg.Write.LatencyP50, tg.Write.LatencyP99)
			}
		})
	}
	// Nothing is left in the temporary areas.
	for _, root := range append(roots, peerRoots...) {
		entries, err := os.ReadDir(filepath.Join(root, domain.SystemBucket, storage.TmpDir))
		require.NoError(t, err)
		for _, e := range entries {
			assert.NotContains(t, e.Name(), "speedtest", "left in %s", root)
		}
	}
}

func TestSpeedtestObject(t *testing.T) {
	objects := objectmock.NewService(t)
	buckets := bucketmock.NewService(t)
	var mu sync.Mutex
	stored := map[string][]byte{}
	var bucketName string
	buckets.EXPECT().Create(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in bucket.CreateInput) (*domain.Bucket, error) {
		bucketName = in.Name
		return &domain.Bucket{Name: in.Name}, nil
	}).Once()
	objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		data, err := io.ReadAll(in.Reader)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		mu.Lock()
		defer mu.Unlock()
		stored[in.Key] = data
		return domain.ObjectInfo{Bucket: in.Bucket, Key: in.Key, Size: int64(len(data))}, nil
	})
	objects.EXPECT().GetObject(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, _ object.GetObjectOptions) (*object.Reader, error) {
			mu.Lock()
			defer mu.Unlock()
			return &object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(stored[key]))}, nil
		})
	deleted := false
	buckets.EXPECT().Delete(mock.Anything, mock.Anything, bucket.DeleteOptions{Force: true}).RunAndReturn(func(_ context.Context, name string, _ bucket.DeleteOptions) error {
		assert.Equal(t, bucketName, name)
		deleted = true
		return nil
	}).Once()

	d := admin.NewDiagnostics(admin.DiagnosticsDeps{Objects: objects, Buckets: buckets, Logger: slog.New(slog.DiscardHandler)})
	res, err := d.Speedtest(context.Background(), admin.SpeedtestInput{Kind: admin.SpeedtestObject, Size: 32 << 10, Duration: 150 * time.Millisecond, Concurrency: 4})
	require.NoError(t, err)
	assert.Regexp(t, `^nexo-speedtest-[0-9a-f]{12}$`, bucketName)
	assert.True(t, deleted, "the temporary bucket is removed")
	require.Len(t, res.Targets, 1)
	tg := res.Targets[0]
	assert.Empty(t, tg.Error)
	assert.Positive(t, tg.Write.Operations)
	assert.Positive(t, tg.Read.Operations)
	assert.Equal(t, 4, res.Concurrency)
}
