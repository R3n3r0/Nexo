package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/media"
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
	Jobs MediaJobs
	// Media reads the presentations and the trace records of the forensic
	// watermark (optional: Trace fails with domain.ErrNotImplemented
	// without it).
	Media  media.Service
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

// GetProtection implements MediaManager.
func (m *mediaManager) GetProtection(ctx context.Context, name string) (*domain.MediaProtection, error) {
	p, err := m.deps.Buckets.GetMediaProtection(ctx, name)
	if err != nil {
		return nil, err
	}
	// The defaults of a protection never set.
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetProtection implements MediaManager.
func (m *mediaManager) SetProtection(ctx context.Context, name string, p domain.MediaProtection) (*domain.MediaProtection, error) {
	if err := m.deps.Buckets.SetMediaProtection(ctx, name, p); err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: media protection set", "bucket", name, "enabled", p.Enabled)
	return m.GetProtection(ctx, name)
}

// traceThreshold is the mean score of a bit position below which it is
// unknown: the votes of a clean copy are about ±1, those of a re-encoded
// one about ±0.7.
const traceThreshold = 0.25

// Trace implements MediaManager.
func (m *mediaManager) Trace(ctx context.Context, in TraceInput) (*TraceResult, error) {
	if m.deps.Media == nil {
		return nil, fmt.Errorf("%w: the media service is not available", domain.ErrNotImplemented)
	}
	if in.Key == "" {
		return nil, fmt.Errorf("%w: an object key is required", domain.ErrInvalidArgument)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	bits := in.Bits
	switch {
	case bits != "" && len(in.Votes) > 0:
		return nil, fmt.Errorf("%w: give the bits or the votes, not both", domain.ErrInvalidArgument)
	case bits == "" && len(in.Votes) == 0:
		return nil, fmt.Errorf("%w: the bits or the votes of the leaked copy are required", domain.ErrInvalidArgument)
	case bits == "":
		var err error
		if bits, err = m.traceBits(ctx, in); err != nil {
			return nil, err
		}
	}
	matches, err := m.deps.Media.Trace(ctx, in.Bucket, bits, limit)
	if err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: leak traced", "bucket", in.Bucket, "key", in.Key, "bits", bits, "matches", len(matches))
	return &TraceResult{Bits: bits, Matches: matches}, nil
}

// traceBits reads the bits of the code from the votes: the mean score of
// every segment of the video, then of every bit position (segment n is
// bit n mod domain.MediaTraceBits), '0' for the A variant, '1' for B, '?'
// when the votes do not tell.
func (m *mediaManager) traceBits(ctx context.Context, in TraceInput) (string, error) {
	src := media.Source{Bucket: in.Bucket, Key: in.Key, VersionID: in.VersionID}
	st, err := m.deps.Media.Status(ctx, src)
	if err != nil {
		return "", err
	}
	rendition := ""
	if st != nil && st.Current {
		for _, r := range st.Renditions {
			if r.Forensic {
				rendition = r.Name
				break
			}
		}
	}
	if rendition == "" {
		return "", fmt.Errorf("%w: %s has no rendition with the forensic watermark", domain.ErrInvalidArgument, in.Key)
	}
	src.Rendition = rendition
	p, err := m.deps.Media.Describe(ctx, src)
	if err != nil {
		return "", err
	}
	// The end of every segment, in seconds.
	ends := make([]float64, len(p.Segments))
	var t float64
	for i, d := range p.Segments {
		t += d.Seconds()
		ends[i] = t
	}
	sums := make([]float64, len(ends))
	counts := make([]int, len(ends))
	for _, v := range in.Votes {
		n := sort.SearchFloat64s(ends, v.T)
		if n < len(ends) && ends[n] == v.T {
			n++ // the first picture of the next segment
		}
		if n >= len(ends) || v.T < 0 || math.IsNaN(v.Score) {
			continue
		}
		sums[n] += max(-1.5, min(1.5, v.Score))
		counts[n]++
	}
	var pos [domain.MediaTraceBits]float64
	var seen [domain.MediaTraceBits]int
	for n := range sums {
		if counts[n] > 0 {
			pos[n%domain.MediaTraceBits] += sums[n] / float64(counts[n])
			seen[n%domain.MediaTraceBits]++
		}
	}
	var b strings.Builder
	known := 0
	for i := range domain.MediaTraceBits {
		switch mean := pos[i] / float64(max(seen[i], 1)); {
		case seen[i] > 0 && mean >= traceThreshold:
			b.WriteByte('0')
			known++
		case seen[i] > 0 && mean <= -traceThreshold:
			b.WriteByte('1')
			known++
		default:
			b.WriteByte('?')
		}
	}
	if known == 0 {
		return "", fmt.Errorf("%w: the votes tell no bit of the code (another video, another time offset?)", domain.ErrInvalidArgument)
	}
	return strings.TrimRight(b.String(), "?"), nil
}
