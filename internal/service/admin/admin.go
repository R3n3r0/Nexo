package admin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
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
	}
	return info
}

// DataUsage implements Service.
func (s *service) DataUsage(ctx context.Context) (*DataUsage, error) {
	if s.deps.Objects == nil {
		return nil, fmt.Errorf("admin: data usage: %w", domain.ErrNotImplemented)
	}
	// DataUsage walks every bucket (ListBuckets leaves the counters at
	// zero). TODO(M8): serve the scanner's cached usage instead.
	buckets, err := s.deps.Objects.DataUsage(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("admin: data usage: %w", err)
	}
	du := &DataUsage{LastUpdate: s.deps.Clock.Now(), Buckets: make(map[string]BucketUsage, len(buckets)), BucketsCount: uint64(len(buckets))}
	for _, b := range buckets {
		du.Buckets[b.Name] = BucketUsage{Size: b.Size, ObjectsCount: b.Objects, VersionsCount: b.Versions, DeleteMarkers: b.DeleteMarkers}
		du.ObjectsCount += b.Objects
		du.VersionsCount += b.Versions
		du.TotalSize += b.Size
	}
	return du, nil
}

// Heal implements Service.
//
// TODO(M5): wire the healer.
func (s *service) Heal(context.Context, HealInput) (*HealResult, error) {
	return nil, fmt.Errorf("admin: heal: %w", domain.ErrNotImplemented)
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
