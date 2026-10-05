//go:build !desktop_e2e

package desktop

import (
	"testing"

	"github.com/yigitcittan/mongorescue/internal/update"
)

// A build without the desktop_e2e tag always checks GitHub, whatever
// E2EUpdateBaseURLEnv says, so the variable cannot redirect a release build's update.
func TestReleaseBuildIgnoresE2EUpdateBaseURL(t *testing.T) {
	t.Setenv(E2EUpdateBaseURLEnv, "http://127.0.0.1:1")
	u := NewUpdater(UpdaterOptions{Version: "1.0.0"})
	c, ok := u.opts.Source.(*update.Checker)
	if !ok {
		t.Fatalf("source = %T, want *update.Checker", u.opts.Source)
	}
	if *c != (update.Checker{}) {
		t.Fatalf("source = %+v, want a zero Checker (GitHub)", *c)
	}
	src := defaultUpdateSource(func(string) string { return "http://127.0.0.1:1" })
	if c, ok := src.(*update.Checker); !ok || *c != (update.Checker{}) {
		t.Fatalf("defaultUpdateSource = %#v, want a zero Checker", src)
	}
}
