package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// formatter is the default Formatter: it reads the format.json documents
// through StorageAPI and delegates their construction and validation to the
// topology service.
type formatter struct {
	deps FormatterDeps
}

var _ Formatter = (*formatter)(nil)

// NewFormatter returns the formatter. Topology is required; IDs and Logger
// default to UUID v4 and slog.Default().
func NewFormatter(deps FormatterDeps) Formatter {
	if deps.IDs == nil {
		deps.IDs = topology.NewUUIDGenerator()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Topology == nil {
		deps.Topology = topology.New(topology.Deps{IDs: deps.IDs, Logger: deps.Logger})
	}
	return &formatter{deps: deps}
}

// EnsureFormats implements Formatter.
func (f *formatter) EnsureFormats(ctx context.Context, layout *topology.Layout, drives []StorageAPI) (string, error) {
	if layout == nil {
		return "", fmt.Errorf("%w: nil layout", domain.ErrInvalidLayout)
	}
	byEndpoint := make(map[string]StorageAPI, len(drives))
	for _, d := range drives {
		if d != nil {
			byEndpoint[d.Endpoint().String()] = d
		}
	}
	deploymentID := ""
	for _, pool := range layout.Pools {
		id, err := f.ensurePool(ctx, layout, pool, byEndpoint, deploymentID)
		if err != nil {
			return "", err
		}
		if id != "" {
			deploymentID = id
		}
	}
	if deploymentID == "" {
		return "", fmt.Errorf("%w: no local drive in the layout", domain.ErrInvalidLayout)
	}
	return deploymentID, nil
}

// ensurePool formats or checks one pool and returns its deployment id ("" when
// no drive of the pool is attached to this node).
func (f *formatter) ensurePool(ctx context.Context, layout *topology.Layout, pool topology.Pool, byEndpoint map[string]StorageAPI, deploymentID string) (string, error) {
	formats := make([]*topology.FormatV1, len(pool.Endpoints))
	present := make([]StorageAPI, len(pool.Endpoints))
	readable, attached := 0, 0
	for i, ep := range pool.Endpoints {
		d, ok := byEndpoint[ep.String()]
		if !ok {
			continue
		}
		present[i] = d
		attached++
		data, err := d.ReadAll(ctx, domain.SystemBucket, FormatFile)
		switch {
		case errors.Is(err, domain.ErrObjectNotFound):
			continue // unformatted drive
		case err != nil:
			return "", fmt.Errorf("storage: read format.json of %s: %w", ep, err)
		}
		fmtDoc, err := topology.UnmarshalFormat(data)
		if err != nil {
			return "", fmt.Errorf("storage: drive %s: %w", ep, err)
		}
		formats[i] = fmtDoc
		readable++
	}
	if attached == 0 {
		return "", nil
	}

	if readable == 0 {
		if deploymentID == "" {
			deploymentID = f.deps.IDs.NewID()
		}
		fresh, err := f.deps.Topology.NewFormats(ctx, layout, pool.Index, deploymentID)
		if err != nil {
			return "", fmt.Errorf("storage: format pool %d: %w", pool.Index+1, err)
		}
		for i, d := range present {
			if d == nil {
				continue
			}
			if err := f.write(ctx, d, fresh[i]); err != nil {
				return "", err
			}
		}
		f.deps.Logger.LogAttrs(ctx, slog.LevelInfo, "formatted fresh pool",
			slog.Int("pool", pool.Index+1), slog.String("deployment_id", deploymentID),
			slog.Int("drives", attached), slog.Int("sets", pool.SetCount), slog.Int("set_drives", pool.SetDriveCount))
		return deploymentID, nil
	}

	ref, err := f.deps.Topology.CheckFormats(ctx, layout, pool.Index, formats)
	if err != nil {
		return "", fmt.Errorf("storage: %w", err)
	}
	if deploymentID != "" && ref.ID != deploymentID {
		return "", fmt.Errorf("%w: pool %d belongs to deployment %s, pool 1 to %s", domain.ErrFormatMismatch, pool.Index+1, ref.ID, deploymentID)
	}
	for i, d := range present {
		if d == nil {
			continue
		}
		set, disk := i/pool.SetDriveCount, i%pool.SetDriveCount
		if formats[i] != nil {
			gotSet, gotDisk, err := formats[i].Position()
			if err != nil {
				return "", fmt.Errorf("storage: drive %s: %w", pool.Endpoints[i], err)
			}
			if gotSet != set || gotDisk != disk {
				return "", fmt.Errorf("%w: drive %s is formatted for set %d disk %d but is given as set %d disk %d",
					domain.ErrFormatMismatch, pool.Endpoints[i], gotSet+1, gotDisk+1, set+1, disk+1)
			}
			continue
		}
		// A fresh replacement drive takes the document of its position and
		// is marked for healing (the tracker first: a crash between the two
		// writes leaves an unformatted drive, formatted again next time).
		doc := ref.Clone()
		doc.Erasure.This = ref.Erasure.Sets[set][disk]
		now := f.deps.Clock.Now().UTC()
		tracker, err := json.Marshal(HealingTracker{
			ID: doc.Erasure.This, PoolIndex: pool.Index, SetIndex: set, DiskIndex: disk,
			Path: pool.Endpoints[i].Path, Endpoint: pool.Endpoints[i].String(), Started: now, LastUpdate: now,
		})
		if err != nil {
			return "", fmt.Errorf("storage: encode healing tracker: %w", err)
		}
		if err := d.WriteAll(ctx, domain.SystemBucket, HealingFile, tracker); err != nil {
			return "", fmt.Errorf("storage: mark %s for healing: %w", d.Endpoint(), err)
		}
		if err := f.write(ctx, d, doc); err != nil {
			return "", err
		}
		f.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "formatted replacement drive (marked for healing)",
			slog.String("drive", pool.Endpoints[i].String()), slog.Int("pool", pool.Index+1),
			slog.Int("set", set+1), slog.Int("disk", disk+1))
	}
	return ref.ID, nil
}

func (f *formatter) write(ctx context.Context, d StorageAPI, doc *topology.FormatV1) error {
	data, err := doc.Marshal()
	if err != nil {
		return fmt.Errorf("storage: encode format.json for %s: %w", d.Endpoint(), err)
	}
	if err := d.WriteAll(ctx, domain.SystemBucket, FormatFile, data); err != nil {
		return fmt.Errorf("storage: write format.json to %s: %w", d.Endpoint(), err)
	}
	return nil
}
