//go:build race

package accessrule_test

// raceDetector reports a test binary built with -race: the latency
// bounds are relaxed under the detector.
const raceDetector = true
