// Package service groups the Nexo services: one sub-package per service, each
// with its contract in service.go, an unexported implementation created by
// New(deps, opts...) and a generated mock in mock/ (docs/12-architettura-servizi.md).
//
// The mocks of every service interface are generated here, from the
// repository-wide .mockery.yaml, so that `go generate ./...` is the single
// entry point and CI can check that the generated code is up to date.
//
//go:generate go run github.com/vektra/mockery/v2@v2.53.4 --config ../../.mockery.yaml
package service
