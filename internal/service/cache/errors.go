package cache

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Sentinels of the package; they wrap the domain ones, so that errors.Is
// on domain.ErrNotFound, domain.ErrConflict and domain.ErrInvalidArgument
// keeps working (and the transports map them).
var (
	// ErrPolicyNotFound reports an unknown cache policy.
	ErrPolicyNotFound = fmt.Errorf("cache policy not found: %w", domain.ErrNotFound)
	// ErrInvalidationNotFound reports an unknown invalidation.
	ErrInvalidationNotFound = fmt.Errorf("invalidation not found: %w", domain.ErrNotFound)
	// ErrWarmJobNotFound reports an unknown warm-up.
	ErrWarmJobNotFound = fmt.Errorf("warm-up not found: %w", domain.ErrNotFound)
	// ErrWebhookNotFound reports an unknown purge webhook.
	ErrWebhookNotFound = fmt.Errorf("purge webhook not found: %w", domain.ErrNotFound)
	// ErrInvalid reports an invalid policy, configuration, invalidation or
	// warm-up.
	ErrInvalid = fmt.Errorf("invalid cache configuration: %w", domain.ErrInvalidArgument)
	// ErrBuiltinPolicy reports a change of a built-in policy.
	ErrBuiltinPolicy = fmt.Errorf("built-in cache policy: %w", domain.ErrConflict)
	// ErrPolicyInUse reports the removal of a policy a level references.
	ErrPolicyInUse = fmt.Errorf("cache policy in use: %w", domain.ErrConflict)
	// ErrAlreadyExists reports a name or an id already taken.
	ErrAlreadyExists = fmt.Errorf("already exists: %w", domain.ErrConflict)
	// ErrNotReady reports a cache that is not composed yet (internode
	// calls during the startup of a node).
	ErrNotReady = fmt.Errorf("cache not ready: %w", domain.ErrNodeOffline)
)
