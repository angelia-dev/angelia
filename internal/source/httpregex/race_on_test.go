//go:build race

package httpregex_test

// raceDetector: the race detector slows regexp matching about 25 times.
const raceDetector = true
