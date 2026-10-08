//go:build race

package object_test

// raceEnabled reports a build with the race detector, whose overhead
// makes timing assertions meaningless.
const raceEnabled = true
