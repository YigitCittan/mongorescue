//go:build !desktop_e2e

package desktop

import "github.com/yigitcittan/mongorescue/internal/update"

// defaultUpdateSource returns the release source of NewUpdater without
// UpdaterOptions.Source: GitHub. The environment is not read, so
// E2EUpdateBaseURLEnv cannot redirect the update of a build without the
// desktop_e2e tag.
func defaultUpdateSource(func(string) string) UpdateSource {
	return &update.Checker{}
}
