//go:build !race

package match_test

// raceDetector reports a test binary built with -race: the timing bounds
// are relaxed under the detector.
const raceDetector = false
