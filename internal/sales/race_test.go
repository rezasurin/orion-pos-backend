//go:build race

package sales_test

// raceEnabled is true when the tests run under the race detector, which slows the code several
// times over and so makes latency numbers meaningless.
const raceEnabled = true
