// Package settings applies the dynamic configuration (config.Service) to
// the server: it reads the subsystems the server honors at every call, so
// that a change written through the Admin API takes effect on the next
// request without a restart (hot reload), and it decorates the services
// whose behavior depends on them.
//
// Applied keys (Applied reports them, the console and nexoctl show the
// others as "not applied yet"):
//
//   - api.cors_allow_origin: the origins the S3 API allows for the
//     buckets without a CORS configuration of their own;
//   - api.root_access: "off" denies the root credentials on the S3 API
//     (GuardRootAccess); the console, the Admin API and the credentials
//     derived from root (service accounts, STS sessions) keep working, so
//     that an administrator can always turn it back on;
//   - scanner.speed and scanner.cycle: read by the background scanner at
//     the start of every cycle.
package settings

import (
	"context"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/service/config"
)

// Service exposes the applied settings of the dynamic configuration.
type Service interface {
	// API returns the applied settings of the "api" subsystem, with the
	// defaults of the keys that are not set. A value that cannot be parsed
	// (written around the Admin API validation) keeps the default and is
	// logged.
	API(ctx context.Context) (API, error)
}

// API are the applied settings of the S3 API.
type API struct {
	// RootAccess allows the root credentials on the S3 API
	// (api.root_access, default on).
	RootAccess bool
	// CORSAllowOrigin are the origins of the deployment-wide CORS policy
	// of the S3 API (api.cors_allow_origin, comma separated, "*" for
	// every origin): it applies to the buckets without a CORS
	// configuration. Nil (the key is not set) allows every origin.
	CORSAllowOrigin []string
}

// Deps are the dependencies of the settings service.
type Deps struct {
	// Config is the dynamic configuration (required).
	Config config.Service
	Logger *slog.Logger
}
