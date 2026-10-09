package object_test

import (
	"log/slog"

	"github.com/r3n3r0/nexo/internal/service/erasure"
)

// testCodecs returns the erasure codec factory of a test deployment, keyed
// like the one internal/app injects (PoolsDeps.Codecs is required).
func testCodecs(deploymentID string, opts ...erasure.Option) erasure.Factory {
	return erasure.NewFactory(erasure.Deps{Logger: slog.New(slog.DiscardHandler)},
		append([]erasure.Option{erasure.WithDeploymentID(deploymentID)}, opts...)...)
}
