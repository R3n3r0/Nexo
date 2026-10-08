package heal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// formatState is the format.json read from one drive.
type formatState struct {
	ref   driveRef
	state object.HealDriveState
	doc   *topology.FormatV1
}

// readFormat reads and classifies the format.json of a drive: offline,
// missing (a new or wiped drive), corrupt (unreadable or invalid) or ok.
func readFormat(ctx context.Context, ref driveRef) formatState {
	fs := formatState{ref: ref, state: object.HealDriveOffline}
	d := ref.drive
	if d == nil || !d.IsOnline() {
		return fs
	}
	data, err := d.ReadAll(ctx, domain.SystemBucket, storage.FormatFile)
	switch {
	case errors.Is(err, domain.ErrDiskNotFound):
		return fs
	case errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrBucketNotFound):
		fs.state = object.HealDriveMissing
		return fs
	case err != nil:
		fs.state = object.HealDriveCorrupt
		return fs
	}
	doc, err := topology.UnmarshalFormat(data)
	if err != nil {
		fs.state = object.HealDriveCorrupt
		return fs
	}
	fs.doc, fs.state = doc, object.HealDriveOK
	return fs
}

// HealFormat implements Service.
func (s *service) HealFormat(ctx context.Context, opts Options) (FormatResult, error) {
	var res FormatResult
	var errs []error
	for p := range s.deps.Pools {
		var refs []driveRef
		for _, d := range s.allDrives() {
			if d.pool == p {
				refs = append(refs, d)
			}
		}
		states := make([]formatState, len(refs))
		var wg sync.WaitGroup
		for i, r := range refs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				states[i] = readFormat(ctx, r)
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return res, err
		}
		healed, err := s.healPoolFormat(ctx, p, states, opts)
		if err != nil {
			errs = append(errs, err)
		}
		for _, st := range states {
			hd := object.HealDrive{Pool: st.ref.pool, Set: st.ref.set, Disk: st.ref.disk, Before: st.state, After: st.state}
			if st.ref.drive != nil {
				hd.Endpoint = st.ref.drive.Endpoint().String()
			}
			if healed[hd.Endpoint] {
				res.Healed = append(res.Healed, hd.Endpoint)
				if !opts.DryRun {
					hd.After = object.HealDriveOK
				}
			}
			res.Drives = append(res.Drives, hd)
		}
	}
	return res, errors.Join(errs...)
}

// healPoolFormat formats the missing local drives of pool p with the
// document of their position, taken from the drives that have one, and
// marks them for healing. It returns the endpoints formatted (or that
// would be, with DryRun).
func (s *service) healPoolFormat(ctx context.Context, p int, states []formatState, opts Options) (map[string]bool, error) {
	healed := map[string]bool{}
	missing := false
	docs := make([]*topology.FormatV1, 0, len(states))
	for _, st := range states {
		if st.doc != nil {
			docs = append(docs, st.doc)
		}
		if st.state == object.HealDriveMissing && isLocal(st.ref.drive) {
			missing = true
		}
	}
	if !missing {
		return healed, nil
	}
	ref, err := topology.CheckFormats(docs)
	if err != nil {
		return healed, fmt.Errorf("heal: format of pool %d: %w", p+1, err)
	}
	for _, st := range states {
		if st.state != object.HealDriveMissing || !isLocal(st.ref.drive) {
			continue
		}
		if st.ref.set >= len(ref.Erasure.Sets) || st.ref.disk >= len(ref.Erasure.Sets[st.ref.set]) {
			return healed, fmt.Errorf("%w: heal: pool %d: drive %s has no position in the format", domain.ErrFormatMismatch, p+1, st.ref.drive.Endpoint())
		}
		ep := st.ref.drive.Endpoint().String()
		if opts.DryRun {
			healed[ep] = true
			continue
		}
		if err := s.formatDrive(ctx, st.ref, ref); err != nil {
			s.deps.Logger.Warn("heal: cannot format drive", "drive", ep, "err", err)
			continue
		}
		healed[ep] = true
		s.deps.Logger.Warn("heal: empty drive formatted and marked for healing", "drive", ep,
			"pool", p+1, "set", st.ref.set+1, "disk", st.ref.disk+1)
	}
	return healed, nil
}

// formatDrive writes the format.json of its position and a fresh healing
// tracker on an empty drive.
func (s *service) formatDrive(ctx context.Context, ref driveRef, pool *topology.FormatV1) error {
	d := ref.drive
	if err := d.MakeVol(ctx, domain.SystemBucket); err != nil && !errors.Is(err, domain.ErrBucketAlreadyExists) {
		return fmt.Errorf("create the system volume: %w", err)
	}
	doc := pool.Clone()
	doc.Erasure.This = pool.Erasure.Sets[ref.set][ref.disk]
	data, err := doc.Marshal()
	if err != nil {
		return err
	}
	// The tracker first: a drive with a format but no tracker would look
	// healthy after a crash between the two writes.
	now := s.deps.Clock.Now().UTC()
	tracker := storage.HealingTracker{
		ID: doc.Erasure.This, PoolIndex: ref.pool, SetIndex: ref.set, DiskIndex: ref.disk,
		Path: d.Endpoint().Path, Endpoint: d.Endpoint().String(), Started: now, LastUpdate: now,
	}
	if err := writeTracker(ctx, d, tracker); err != nil {
		return err
	}
	if err := d.WriteAll(ctx, domain.SystemBucket, storage.FormatFile, data); err != nil {
		return fmt.Errorf("write format.json: %w", err)
	}
	return nil
}

// writeTracker saves a healing tracker on its drive.
func writeTracker(ctx context.Context, d storage.StorageAPI, t storage.HealingTracker) error {
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("encode healing tracker: %w", err)
	}
	if err := d.WriteAll(ctx, domain.SystemBucket, storage.HealingFile, data); err != nil {
		return fmt.Errorf("write healing tracker: %w", err)
	}
	return nil
}
