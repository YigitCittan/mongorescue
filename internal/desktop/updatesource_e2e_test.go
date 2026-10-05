//go:build desktop_e2e

package desktop

import (
	"context"
	"errors"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/update"
)

func TestE2EUpdateSource(t *testing.T) {
	tests := []struct {
		value string
		want  update.Checker
		bad   bool
	}{
		{value: "", want: update.Checker{}},
		{value: "http://127.0.0.1:8080", want: update.Checker{BaseURL: "http://127.0.0.1:8080",
			DownloadPrefix: "http://127.0.0.1:8080/download/", ReleasesPrefix: "http://127.0.0.1:8080/releases/"}},
		{value: "http://[::1]:9/", want: update.Checker{BaseURL: "http://[::1]:9",
			DownloadPrefix: "http://[::1]:9/download/", ReleasesPrefix: "http://[::1]:9/releases/"}},
		{value: "http://localhost:9", want: update.Checker{BaseURL: "http://localhost:9",
			DownloadPrefix: "http://localhost:9/download/", ReleasesPrefix: "http://localhost:9/releases/"}},
		{value: "http://example.com", bad: true},
		{value: "http://10.0.0.1:80", bad: true},
		{value: "https://127.0.0.1:443", bad: true},
		{value: "http://127.0.0.1:80/api", bad: true},
		{value: "http://user:pw@127.0.0.1:80", bad: true},
		{value: "http://127.0.0.1:80?x=1", bad: true},
		{value: "127.0.0.1:80", bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			src := defaultUpdateSource(func(name string) string {
				if name != E2EUpdateBaseURLEnv {
					t.Errorf("read %s", name)
				}
				return tt.value
			})
			if tt.bad {
				_, err := src.Check(context.Background(), "1.0.0")
				if !errors.Is(err, errE2EBaseURL) {
					t.Fatalf("Check error = %v, want errE2EBaseURL", err)
				}
				if _, err = src.DownloadAsset(context.Background(), update.Result{}, update.Asset{}, t.TempDir(), nil); !errors.Is(err, errE2EBaseURL) {
					t.Fatalf("DownloadAsset error = %v, want errE2EBaseURL", err)
				}
				return
			}
			c, ok := src.(*update.Checker)
			if !ok || *c != tt.want {
				t.Fatalf("source = %#v, want %+v", src, tt.want)
			}
		})
	}
}
