//go:build !race

package sales_test

// raceEnabled is false in a normal run, where latency is real.
const raceEnabled = false
