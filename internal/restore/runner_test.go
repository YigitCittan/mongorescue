package restore

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/mongotools"
)

func TestDefaultRunnerReportsMissingTool(t *testing.T) {
	root := t.TempDir()
	tools := &mongotools.Resolver{
		Dir:        filepath.Join(root, "custom"),
		Executable: func() (string, error) { return filepath.Join(root, "mongorescue"), nil },
		LookPath:   func(string) (string, error) { return "", errors.New("not on PATH") },
	}
	_, _, err := newProcessRunner(tools, slog.New(slog.DiscardHandler))(context.Background(), "mongorestore", strings.NewReader(""), "--archive")
	if !errors.Is(err, mongotools.ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
	if !strings.Contains(err.Error(), mongotools.EnvToolsDir) {
		t.Fatalf("err = %q, want a hint naming %s", err, mongotools.EnvToolsDir)
	}
}
