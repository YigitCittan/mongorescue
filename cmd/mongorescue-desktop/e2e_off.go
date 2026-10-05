//go:build desktop && !desktop_e2e

package main

// runE2E reports false: only desktop_e2e builds run the headless update of the
// desktop update end-to-end test (see e2e.go).
func runE2E([]string) (int, bool) {
	return 0, false
}
