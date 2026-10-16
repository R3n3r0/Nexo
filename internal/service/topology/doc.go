// Package topology parses the server command line into a deployment layout.
//
// It expands MinIO style ellipsis patterns ({1...4}, {01...16}, {a...d}),
// validates the resulting endpoints, detects the deployment mode (SNSD, SNMD or
// distributed), splits every pool into erasure sets whose drives are spread
// across nodes, identifies the endpoints that belong to the local node and
// defines the on-disk format.json document (FormatV1) that records the layout.
//
// The shared vocabulary (domain.Mode, domain.Endpoint, domain.ErrInvalidArgument)
// comes from internal/domain; this package only adds the layout types.
//
// The package follows the service convention of docs/12-service-architecture.md:
// the Service interface lives in service.go, the implementation is unexported
// and created through New, and every dependency is injected through Deps.
package topology
