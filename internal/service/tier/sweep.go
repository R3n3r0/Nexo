package tier

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Start implements lifecycle.Runner: the leader sweeps the tiers
// periodically.
func (s *service) Start(ctx context.Context) error {
	if s.deps.Objects == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.opts.sweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if s.deps.Leader != nil && !s.deps.Leader.IsLeader() {
				continue
			}
			if res, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.deps.Logger.WarnContext(ctx, "tier: sweep failed", "err", err, "removed", res.Removed)
			}
		}
	}()
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Sweep implements Service.
func (s *service) Sweep(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	if s.deps.Objects == nil {
		return res, fmt.Errorf("%w: tier: the sweep needs the object layer", domain.ErrNotImplemented)
	}
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	tiers, err := s.List(ctx)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, t := range tiers {
		if err := s.sweepTier(ctx, t, &res); err != nil {
			res.Failed++
			errs = append(errs, err)
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	return res, errors.Join(errs...)
}

// sweepTier removes the orphaned objects of one tier.
func (s *service) sweepTier(ctx context.Context, t Tier, res *SweepResult) error {
	remote, err := s.remote(ctx, t.Name)
	if err != nil {
		return err
	}
	prefix := s.objectPrefix(t)
	cutoff := s.deps.Clock.Now().Add(-s.opts.grace)
	token := ""
	for {
		page, next, err := remote.List(ctx, prefix, token)
		if err != nil {
			return fmt.Errorf("tier %s: list: %w", t.Name, err)
		}
		for _, o := range page {
			if !o.ModTime.Before(cutoff) {
				continue
			}
			res.Scanned++
			orphan, err := s.orphan(ctx, remote, o.Name)
			if err != nil {
				res.Failed++
				s.deps.Logger.WarnContext(ctx, "tier: cannot check an object", "tier", t.Name, "object", o.Name, "err", err)
				continue
			}
			if !orphan {
				continue
			}
			if err := remote.Delete(ctx, o.Name); err != nil {
				res.Failed++
				s.deps.Logger.WarnContext(ctx, "tier: cannot remove an orphaned object", "tier", t.Name, "object", o.Name, "err", err)
				continue
			}
			res.Removed++
			s.deps.Logger.DebugContext(ctx, "tier: orphaned object removed", "tier", t.Name, "object", o.Name)
		}
		if next == "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		token = next
	}
}

// orphan reports whether no version refers to an object of a tier any
// more. Only an object of this deployment whose version is known, looked
// up under its lock, can be an orphan: anything else is kept.
func (s *service) orphan(ctx context.Context, remote Remote, name string) (bool, error) {
	o, err := remote.Head(ctx, name)
	if errors.Is(err, domain.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	bucket, version := o.Metadata[metaBucket], o.Metadata[metaVersion]
	key, kerr := base64.RawURLEncoding.DecodeString(o.Metadata[metaKey])
	if o.Metadata[metaDeployment] != s.deps.DeploymentID || bucket == "" || kerr != nil || len(key) == 0 {
		return false, nil
	}
	loc, err := s.deps.Objects.TierLocation(ctx, bucket, string(key), version)
	if err != nil {
		return false, err
	}
	return !loc.Exists || loc.Object != name, nil
}
