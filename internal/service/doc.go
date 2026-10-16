// Package service groups the Nexo services: one sub-package per service, each
// with its contract in service.go, an unexported implementation created by
// New(deps, opts...) and a generated mock in mock/ (docs/12-architettura-servizi.md).
//
// The mocks of every service interface are generated here, from the
// repository-wide .mockery.yaml, so that `go generate ./...` is the single
// entry point and CI can check that the generated code is up to date.
// mockery is pinned in the separate tools module (tools/go.mod) so that it is
// built with a golang.org/x/tools that reads the export data of the go.mod
// toolchain, without adding its dependencies to the Nexo module.
//
//go:generate go tool -modfile=../../tools/go.mod mockery --config ../../.mockery.yaml
package service
