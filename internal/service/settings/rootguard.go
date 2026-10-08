package settings

import (
	"context"
	"fmt"

	"github.com/r3n3r0/nexo/internal/service/policy"
)

// rootGuard decorates the policy engine of the S3 API with api.root_access.
type rootGuard struct {
	policy.Service
	settings Service
}

var _ policy.Service = (*rootGuard)(nil)

// GuardRootAccess decorates the policy engine of the S3 API: while
// api.root_access is off, Evaluate denies the root credentials themselves
// (not the service accounts and STS sessions derived from root, which the
// engine evaluates as usual). Every other method is the engine's. The
// setting is read at every evaluation (hot reload); when it cannot be
// read the request fails with the error instead of falling back to
// allowing root.
func GuardRootAccess(engine policy.Service, settings Service) policy.Service {
	return &rootGuard{Service: engine, settings: settings}
}

// Evaluate implements policy.Service.
func (g *rootGuard) Evaluate(ctx context.Context, args policy.Args) (policy.Decision, error) {
	if id := args.Identity; id != nil && id.IsRoot && id.Parent == "" {
		api, err := g.settings.API(ctx)
		if err != nil {
			return policy.Decision{}, err
		}
		if !api.RootAccess {
			return policy.Decision{
				Allowed: false,
				Reason:  fmt.Sprintf("the root credentials are disabled on the S3 API (%s.%s=off)", SubsystemAPI, KeyRootAccess),
				Source:  policy.SourceRoot,
			}, nil
		}
	}
	return g.Service.Evaluate(ctx, args)
}
