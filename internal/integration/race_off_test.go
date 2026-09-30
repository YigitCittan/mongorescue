//go:build integration && !race

package integration

// raceEnabled reports whether the race detector is on (it inflates memory use).
const raceEnabled = false
