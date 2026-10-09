//go:build !nexotest

package main

import (
	"github.com/r3n3r0/nexo/internal/app"
	"github.com/r3n3r0/nexo/internal/config"
)

// testOptions returns the application options of the test hooks: none in
// a release build (the cluster tests build bin/nexo with -tags nexotest,
// see testhooks_nexotest.go).
func testOptions(config.LookupEnv) ([]app.Option, error) { return nil, nil }
