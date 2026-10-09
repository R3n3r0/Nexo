package heal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// HealPools implements Service.
func (s *service) HealPools(ctx context.Context) (int, error) {
	if len(s.deps.Pools) < 2 {
		return 0, nil
	}
	names := s.volumeNames(ctx)
	healed := 0
	var first error
	for _, name := range names {
		res, err := s.deps.Healer.HealBucket(ctx, name, object.HealOptions{})
		switch {
		case errors.Is(err, domain.ErrBucketNotFound), errors.Is(err, domain.ErrInvalidBucketName):
			// The remnant of a rolled-back creation that no set holds.
		case err != nil:
			s.deps.Logger.WarnContext(ctx, "heal: bucket not created on every pool (healed later)", "bucket", name, "err", err)
			if first == nil {
				first = err
			}
		case res.Healed > 0:
			healed++
		}
		if ctx.Err() != nil {
			return healed, context.Cause(ctx)
		}
	}
	if healed > 0 {
		s.deps.Logger.InfoContext(ctx, "heal: buckets created on the pools that missed them", "buckets", healed, "pools", len(s.deps.Pools))
	}
	return healed, first
}

// volumeNames lists the bucket volumes of every online drive of the
// deployment, sorted: every drive at once, each bounded by
// listVolsTimeout, so that an unresponsive node does not hold the caller.
// A drive that does not answer is skipped.
func (s *service) volumeNames(ctx context.Context) []string {
	var (
		mu   sync.Mutex
		seen = map[string]bool{}
		wg   sync.WaitGroup
	)
	for _, set := range s.sets {
		for _, d := range set.drives {
			if d == nil || !d.IsOnline() {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				callCtx, cancel := context.WithTimeout(ctx, s.opts.listVolsTimeout)
				defer cancel()
				vols, err := d.ListVols(callCtx)
				if err != nil {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, v := range vols {
					if v.Name != "" && !strings.HasPrefix(v.Name, ".") {
						seen[v.Name] = true
					}
				}
			}()
		}
	}
	wg.Wait()
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
