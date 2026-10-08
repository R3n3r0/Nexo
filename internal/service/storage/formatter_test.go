package storage

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// seqIDs is a deterministic topology.IDGenerator.
type seqIDs struct{ n int }

func (g *seqIDs) NewID() string { g.n++; return "id-" + string(rune('a'+g.n-1)) }

func layoutAndDrives(t *testing.T, n int) (*topology.Layout, []StorageAPI, []string) {
	t.Helper()
	base := t.TempDir()
	args := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		args = append(args, filepath.Join(base, "d"+string(rune('0'+i))))
	}
	topo := topology.New(topology.Deps{Logger: slog.New(slog.DiscardHandler)})
	layout, err := topo.Parse(context.Background(), args)
	require.NoError(t, err)
	drives := make([]StorageAPI, 0, n)
	for _, ep := range layout.Endpoints() {
		d, err := NewLocal(Deps{}, ep, WithoutFsync())
		require.NoError(t, err)
		drives = append(drives, d)
	}
	return layout, drives, args
}

func TestEnsureFormatsFreshAndIdempotent(t *testing.T) {
	ctx := context.Background()
	layout, drives, _ := layoutAndDrives(t, 4)
	ids := &seqIDs{}
	f := NewFormatter(FormatterDeps{IDs: ids, Logger: slog.New(slog.DiscardHandler)})

	id, err := f.EnsureFormats(ctx, layout, drives)
	require.NoError(t, err)
	assert.Equal(t, "id-a", id, "the first generated id is the deployment id")
	for i, d := range drives {
		data, err := d.ReadAll(ctx, domain.SystemBucket, FormatFile)
		require.NoError(t, err)
		doc, err := topology.UnmarshalFormat(data)
		require.NoError(t, err)
		assert.Equal(t, id, doc.ID)
		assert.Equal(t, topology.FormatModeErasure, doc.Mode)
		set, disk, err := doc.Position()
		require.NoError(t, err)
		assert.Equal(t, 0, set)
		assert.Equal(t, i, disk)
		info, err := d.DiskInfo(ctx, DiskInfoOptions{NoOp: true})
		require.NoError(t, err)
		assert.Equal(t, doc.Erasure.This, info.ID, "DiskInfo reloads the drive id after formatting")
	}

	// A second run finds the documents and changes nothing.
	again, err := f.EnsureFormats(ctx, layout, drives)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	assert.Equal(t, 5, ids.n, "no new ids were generated")

	// A replacement drive (format.json lost) receives the document of its position.
	require.NoError(t, drives[2].Delete(ctx, domain.SystemBucket, FormatFile, DeleteOptions{}))
	again, err = f.EnsureFormats(ctx, layout, drives)
	require.NoError(t, err)
	assert.Equal(t, id, again)
	data, err := drives[2].ReadAll(ctx, domain.SystemBucket, FormatFile)
	require.NoError(t, err)
	doc, err := topology.UnmarshalFormat(data)
	require.NoError(t, err)
	_, disk, _ := doc.Position()
	assert.Equal(t, 2, disk)
}

func TestEnsureFormatsSNSD(t *testing.T) {
	ctx := context.Background()
	layout, drives, _ := layoutAndDrives(t, 1)
	f := NewFormatter(FormatterDeps{})
	id, err := f.EnsureFormats(ctx, layout, drives)
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	data, err := drives[0].ReadAll(ctx, domain.SystemBucket, FormatFile)
	require.NoError(t, err)
	doc, err := topology.UnmarshalFormat(data)
	require.NoError(t, err)
	assert.Equal(t, topology.FormatModeSNSD, doc.Mode)
}

func TestEnsureFormatsMismatches(t *testing.T) {
	ctx := context.Background()
	f := NewFormatter(FormatterDeps{Logger: slog.New(slog.DiscardHandler)})

	// Drives formatted as SNSD cannot be used in an erasure layout.
	snsdLayout, snsdDrives, snsdArgs := layoutAndDrives(t, 1)
	_, err := f.EnsureFormats(ctx, snsdLayout, snsdDrives)
	require.NoError(t, err)
	topo := topology.New(topology.Deps{})
	other := filepath.Join(t.TempDir(), "x")
	mixed, err := topo.Parse(ctx, []string{snsdArgs[0], other, other + "2", other + "3"})
	require.NoError(t, err)
	mixedDrives := make([]StorageAPI, 0, 4)
	for _, ep := range mixed.Endpoints() {
		d, err := NewLocal(Deps{}, ep, WithoutFsync())
		require.NoError(t, err)
		mixedDrives = append(mixedDrives, d)
	}
	_, err = f.EnsureFormats(ctx, mixed, mixedDrives)
	assert.ErrorIs(t, err, domain.ErrFormatMismatch)

	// Swapping two formatted drives is detected.
	layout, drives, _ := layoutAndDrives(t, 4)
	_, err = f.EnsureFormats(ctx, layout, drives)
	require.NoError(t, err)
	swapped := []StorageAPI{drives[1], drives[0], drives[2], drives[3]}
	layout.Pools[0].Endpoints[0], layout.Pools[0].Endpoints[1] = layout.Pools[0].Endpoints[1], layout.Pools[0].Endpoints[0]
	_, err = f.EnsureFormats(ctx, layout, swapped)
	assert.ErrorIs(t, err, domain.ErrFormatMismatch)

	// A corrupt document is reported as an invalid format.
	layout2, drives2, _ := layoutAndDrives(t, 2)
	_, err = f.EnsureFormats(ctx, layout2, drives2)
	require.NoError(t, err)
	require.NoError(t, drives2[0].WriteAll(ctx, domain.SystemBucket, FormatFile, []byte("{")))
	_, err = f.EnsureFormats(ctx, layout2, drives2)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)

	// No attached drive at all.
	_, err = f.EnsureFormats(ctx, layout2, nil)
	assert.ErrorIs(t, err, domain.ErrInvalidLayout)
	_, err = f.EnsureFormats(ctx, nil, drives2)
	assert.ErrorIs(t, err, domain.ErrInvalidLayout)
}
