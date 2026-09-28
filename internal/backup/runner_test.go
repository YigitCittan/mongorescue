package backup

import (
	"context"
	"errors"
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
	_, _, _, err := newProcessRunner(tools)(context.Background(), "mongodump", "--archive")
	if !errors.Is(err, mongotools.ErrToolNotFound) {
		t.Fatalf("err = %v, want ErrToolNotFound", err)
	}
	if !strings.Contains(err.Error(), mongotools.EnvToolsDir) {
		t.Fatalf("err = %q, want a hint naming %s", err, mongotools.EnvToolsDir)
	}
}
