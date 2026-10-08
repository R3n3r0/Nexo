package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// BuildInfo is the version information shown in ServerInfo.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// Deps are the dependencies of the admin facade.
type Deps struct {
	Clock  clock.Clock
	Logger *slog.Logger
	Build  BuildInfo
	Mode   domain.Mode
	Region string
	// DeploymentID identifies the deployment (format.json).
	DeploymentID string
	// Drives are the local drives, reported as disks of the local node
	// with their usage.
	Drives []storage.StorageAPI
	// Pools summarizes the erasure pools of the deployment layout.
	Pools []domain.PoolInfo
	// Objects provides bucket statistics; optional.
	Objects object.Service
	// Heal runs the healing jobs; nil (the single-drive mode) answers
	// domain.ErrNotImplemented.
	Heal heal.Service
	// Scanner provides the data usage it computes; nil computes it on
	// demand.
	Scanner scanner.Service
	// Actions performs restart/stop; optional.
	Actions ActionHandler
}

// Option configures the facade.
type Option func(*service)

type service struct {
	deps      Deps
	startedAt time.Time
}

var _ Service = (*service)(nil)

// New returns the admin facade.
func New(deps Deps, opts ...Option) Service {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps, startedAt: deps.Clock.Now()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ServerInfo implements Service.
func (s *service) ServerInfo(ctx context.Context) (*domain.ServerInfo, error) {
	now := s.deps.Clock.Now()
	info := &domain.ServerInfo{
		Version:      s.deps.Build.Version,
		Commit:       s.deps.Build.Commit,
		BuildDate:    s.deps.Build.Date,
		Mode:         s.deps.Mode,
		Region:       s.deps.Region,
		DeploymentID: s.deps.DeploymentID,
		StartedAt:    s.startedAt,
		Pools:        append([]domain.PoolInfo(nil), s.deps.Pools...),
	}
	info.Uptime = now.Sub(info.StartedAt)
	disks := make([]domain.DiskInfo, 0, len(s.deps.Drives))
	for _, d := range s.deps.Drives {
		disks = append(disks, s.diskInfo(ctx, d))
	}
	info.Nodes = []domain.NodeInfo{{
		Endpoint: "local",
		State:    domain.StateOnline,
		Version:  s.deps.Build.Version,
		Commit:   s.deps.Build.Commit,
		Uptime:   info.Uptime,
		Disks:    disks,
	}}
	if s.deps.Objects != nil {
		buckets, err := s.deps.Objects.ListBuckets(ctx)
		if err == nil {
			info.Buckets = uint64(len(buckets))
			for _, b := range buckets {
				info.Objects += b.Objects
				info.Usage += b.Size
			}
		} else {
			s.deps.Logger.Warn("admin: list buckets for server info", "err", err)
		}
	}
	return info, nil
}

// diskInfo queries one drive, reporting an unreachable drive as offline
// instead of failing the whole server information.
func (s *service) diskInfo(ctx context.Context, d storage.StorageAPI) domain.DiskInfo {
	ep := d.Endpoint()
	info, err := d.DiskInfo(ctx, storage.DiskInfoOptions{})
	if err != nil {
		s.deps.Logger.Warn("admin: disk info", "drive", ep.String(), "err", err)
		if info.State == "" {
			info.State = domain.StateOffline
		}
		if info.Error == "" {
			info.Error = err.Error()
		}
	}
	if info.Endpoint == "" {
		info.Endpoint = ep.String()
	}
	if info.Path == "" {
		info.Path = ep.Path
	}
	info.PoolIdx, info.SetIdx, info.DiskIdx = ep.PoolIdx, ep.SetIdx, ep.DiskIdx
	if info.State == "" {
		info.State = domain.StateOnline
		if !d.IsOnline() {
			info.State = domain.StateOffline
		}
	}
	if h := d.Healing(); h != nil && !h.Finished {
		info.Healing = true
		info.State = domain.StateHealing
		info.HealingProgress = h.Progress()
	}
	return info
}

// DataUsage implements Service.
func (s *service) DataUsage(ctx context.Context) (*DataUsage, error) {
	if s.deps.Objects == nil {
		return nil, fmt.Errorf("admin: data usage: %w", domain.ErrNotImplemented)
	}
	if s.deps.Scanner != nil {
		u, err := s.deps.Scanner.Usage(ctx)
		switch {
		case err == nil:
			return s.scannedUsage(ctx, u)
		case !errors.Is(err, domain.ErrNotFound):
			s.deps.Logger.WarnContext(ctx, "admin: scanner usage unavailable, computing it", "err", err)
		}
	}
	buckets, err := s.deps.Objects.DataUsage(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("admin: data usage: %w", err)
	}
	du := &DataUsage{LastUpdate: s.deps.Clock.Now(), Buckets: make(map[string]BucketUsage, len(buckets))}
	for _, b := range buckets {
		du.add(b.Name, BucketUsage{Size: b.Size, ObjectsCount: b.Objects, VersionsCount: b.Versions, DeleteMarkers: b.DeleteMarkers})
	}
	return du, nil
}

// scannedUsage serves the usage of the last scanner cycle for the
// current buckets; a bucket created since the cycle is computed now.
func (s *service) scannedUsage(ctx context.Context, u scanner.Usage) (*DataUsage, error) {
	buckets, err := s.deps.Objects.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: data usage: %w", err)
	}
	du := &DataUsage{LastUpdate: u.LastUpdate, Buckets: make(map[string]BucketUsage, len(buckets))}
	for _, b := range buckets {
		if bu, ok := u.Buckets[b.Name]; ok {
			du.add(b.Name, bucketUsageOf(bu))
			continue
		}
		fresh, err := s.deps.Objects.DataUsage(ctx, b.Name)
		if errors.Is(err, domain.ErrBucketNotFound) {
			continue // removed meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("admin: data usage of %s: %w", b.Name, err)
		}
		for _, f := range fresh {
			du.add(f.Name, BucketUsage{Size: f.Size, ObjectsCount: f.Objects, VersionsCount: f.Versions, DeleteMarkers: f.DeleteMarkers})
		}
	}
	return du, nil
}

func bucketUsageOf(b scanner.BucketUsage) BucketUsage {
	out := BucketUsage{Size: b.Size, ObjectsCount: b.Objects, VersionsCount: b.Versions, DeleteMarkers: b.DeleteMarkers}
	if len(b.Prefixes) > 0 {
		out.Prefixes = make(map[string]PrefixUsage, len(b.Prefixes))
		for p, c := range b.Prefixes {
			out.Prefixes[p] = PrefixUsage{Size: c.Size, ObjectsCount: c.Objects, VersionsCount: c.Versions, DeleteMarkers: c.DeleteMarkers}
		}
	}
	return out
}

// add records the usage of a bucket in the totals.
func (du *DataUsage) add(name string, b BucketUsage) {
	du.Buckets[name] = b
	du.BucketsCount++
	du.ObjectsCount += b.ObjectsCount
	du.VersionsCount += b.VersionsCount
	du.TotalSize += b.Size
}

// healer returns the healing service, or the error of a deployment
// without healing.
func (s *service) healer() (heal.Service, error) {
	if s.deps.Heal == nil {
		return nil, fmt.Errorf("admin: heal: healing needs an erasure-coded deployment: %w", domain.ErrNotImplemented)
	}
	return s.deps.Heal, nil
}

// StartHeal implements Service.
func (s *service) StartHeal(ctx context.Context, in HealInput) (heal.Job, error) {
	h, err := s.healer()
	if err != nil {
		return heal.Job{}, err
	}
	j, err := h.StartJob(ctx, heal.JobRequest{
		Bucket: in.Bucket, Prefix: in.Prefix, Recursive: in.Recursive,
		Options: heal.Options{DryRun: in.DryRun, Deep: in.Deep, Remove: in.Remove},
	})
	if err != nil {
		return heal.Job{}, fmt.Errorf("admin: heal: %w", err)
	}
	return j, nil
}

// HealStatus implements Service.
func (s *service) HealStatus(ctx context.Context, id string) (heal.Job, error) {
	h, err := s.healer()
	if err != nil {
		return heal.Job{}, err
	}
	j, err := h.Job(ctx, id)
	if err != nil {
		return heal.Job{}, fmt.Errorf("admin: heal: %w", err)
	}
	return j, nil
}

// ListHeals implements Service.
func (s *service) ListHeals(ctx context.Context) ([]heal.Job, error) {
	h, err := s.healer()
	if err != nil {
		return nil, err
	}
	jobs, err := h.Jobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: heal: %w", err)
	}
	return jobs, nil
}

// ServiceAction implements Service.
func (s *service) ServiceAction(ctx context.Context, action Action) error {
	switch action {
	case ActionRestart, ActionStop:
	default:
		return fmt.Errorf("%w: unknown service action %q", domain.ErrInvalidArgument, action)
	}
	if s.deps.Actions == nil {
		return fmt.Errorf("admin: service %s: %w", action, domain.ErrNotImplemented)
	}
	return s.deps.Actions.Handle(ctx, action)
}
