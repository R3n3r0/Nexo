//go:build !race

package integration_test

// raceDetector reports a test binary built with -race: the slow tests of
// the erasure-coded modes are skipped (they run without the detector).
const raceDetector = false
