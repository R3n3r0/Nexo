package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
)

// MediaJobs queues the jobs of the media worker (implemented by the media
// hook of the event service).
type MediaJobs interface {
	// Reprocess queues a job deriving the outputs of a version of a video
	// again with the rule of its bucket that selects it
	// (domain.ErrInvalidArgument when no rule does).
	Reprocess(ctx context.Context, bucket, key, versionID string) error
}

// MediaManagerDeps are the dependencies of the media manager.
type MediaManagerDeps struct {
	// Buckets stores the media configurations (required).
	Buckets bucket.Service
	// Jobs queues the reprocessing jobs (optional: Reprocess fails with
	// domain.ErrNotImplemented without it).
	Jobs   MediaJobs
	Logger *slog.Logger
}

// MediaManagerOption configures the media manager.
type MediaManagerOption func(*mediaManager)

type mediaManager struct {
	deps MediaManagerDeps
}

var _ MediaManager = (*mediaManager)(nil)

// NewMediaManager returns the media manager. Missing dependencies are a
// wiring error (domain.ErrInvalidArgument).
func NewMediaManager(deps MediaManagerDeps, opts ...MediaManagerOption) (MediaManager, error) {
	if deps.Buckets == nil {
		return nil, fmt.Errorf("%w: media manager requires the bucket service", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &mediaManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Get implements MediaManager.
func (m *mediaManager) Get(ctx context.Context, name string) (*domain.MediaConfig, error) {
	cfg, err := m.deps.Buckets.GetMedia(ctx, name)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return &domain.MediaConfig{Rules: []domain.MediaRule{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Set implements MediaManager.
func (m *mediaManager) Set(ctx context.Context, name string, cfg domain.MediaConfig) (*domain.MediaConfig, error) {
	if _, err := m.deps.Buckets.GetVersioning(ctx, name); err != nil {
		return nil, err
	}
	cfg = cfg.Clone()
	var problems []RuleProblem
	ids := map[string]int{}
	for i := range cfg.Rules {
		one := domain.MediaConfig{Rules: cfg.Rules[i : i+1]}
		if err := one.Validate(); err != nil {
			p := ruleProblem(i+1, cfg.Rules[i].ID, err)
			p.Message = strings.TrimPrefix(p.Message, domain.ErrInvalidArgument.Error()+": ")
			// The problem names its rule: not the message too.
			p.Message = strings.TrimPrefix(p.Message, fmt.Sprintf("media rule %q: ", cfg.Rules[i].ID))
			p.Message = strings.TrimPrefix(p.Message, "media rule 1: ")
			problems = append(problems, p)
			continue
		}
		if first, dup := ids[cfg.Rules[i].ID]; dup {
			problems = append(problems, RuleProblem{Rule: i + 1, ID: cfg.Rules[i].ID, Code: "InvalidArgument",
				Message: fmt.Sprintf("duplicate rule id (rule %d)", first)})
		}
		ids[cfg.Rules[i].ID] = i + 1
	}
	if len(problems) > 0 {
		return nil, &ConfigError{Kind: "media", Problems: problems}
	}
	if err := m.deps.Buckets.SetMedia(ctx, name, cfg); err != nil {
		if isValidation(err) {
			return nil, &ConfigError{Kind: "media", Problems: []RuleProblem{ruleProblem(0, "", err)}}
		}
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: media configuration set", "bucket", name, "rules", len(cfg.Rules))
	return m.Get(ctx, name)
}

// Delete implements MediaManager.
func (m *mediaManager) Delete(ctx context.Context, name string) error {
	return m.deps.Buckets.DeleteMedia(ctx, name)
}

// Reprocess implements MediaManager.
func (m *mediaManager) Reprocess(ctx context.Context, name, key, versionID string) error {
	if m.deps.Jobs == nil {
		return fmt.Errorf("%w: the media jobs are not available", domain.ErrNotImplemented)
	}
	if key == "" {
		return fmt.Errorf("%w: an object key is required", domain.ErrInvalidArgument)
	}
	if err := m.deps.Jobs.Reprocess(ctx, name, key, versionID); err != nil {
		return err
	}
	m.deps.Logger.InfoContext(ctx, "admin: media reprocessing queued", "bucket", name, "key", key, "version", versionID)
	return nil
}
