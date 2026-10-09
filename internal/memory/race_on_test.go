//go:build race

package memory_test

// raceEnabled is true under -race, where sync.Pool deliberately drops objects.
const raceEnabled = true
