package object

import (
	"context"
	"errors"
)

// ReadinessCheck adapts the storage health of an object layer to a readiness
// probe: it fails while the write quorum is not met.
func ReadinessCheck(svc Service) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		h := svc.Health(ctx, HealthOptions{})
		if !h.Healthy {
			return errors.New(h.Message)
		}
		return nil
	}
}
