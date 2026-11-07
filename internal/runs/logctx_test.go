package runs

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/logging"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestBindCarriesRunIDsToLogLines checks that a logger call made with the bound
// context carries run_id (the job run of the backup), job_id and the record ID, in
// both log formats.
func TestBindCarriesRunIDsToLogLines(t *testing.T) {
	for _, format := range []logging.Format{logging.FormatText, logging.FormatJSON} {
		var buf bytes.Buffer
		logger := logging.New(&buf, format, slog.LevelInfo)
		reg := NewRegistry()
		run, err := reg.Register(Meta{Kind: models.RunBackup, ID: "bkp_1", JobID: "job_1", Group: "run_1", Database: "shop"})
		if err != nil {
			t.Fatal(err)
		}
		ctx := run.Bind(context.Background())
		logger.InfoContext(ctx, "dumping")
		run.End()
		out := buf.String()
		for _, want := range []string{"run_1", "job_1", "bkp_1"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s log line lacks %q: %s", format, want, out)
			}
		}
	}
}

func TestBindRestoreUsesRecordAsRunID(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, logging.FormatJSON, slog.LevelInfo)
	run, err := NewRegistry().Register(Meta{Kind: models.RunRestore, ID: "rst_1"})
	if err != nil {
		t.Fatal(err)
	}
	logger.InfoContext(run.Bind(context.Background()), "restoring")
	run.End()
	out := buf.String()
	if !strings.Contains(out, `"run_id":"rst_1"`) || !strings.Contains(out, `"restore_id":"rst_1"`) || strings.Contains(out, "job_id") {
		t.Errorf("log line = %s", out)
	}
}
