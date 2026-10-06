package integrity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
)

func TestRestoreTestRunsPostRestoreCommandsAfterTheComparison(t *testing.T) {
	for _, failing := range []bool{false, true} {
		f, job, rec := restoreTestFixture(t)
		f.postRestore = []models.PostRestoreCommand{{Database: "*", Command: json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": 1}, "limit": 0}]}`)}}
		var touchedBefore []string
		f.restorer.postRestore = func(_ context.Context, _ models.RestoreRequest, r *models.RestoreRecord) (*models.RestoreRecord, error) {
			touchedBefore = f.admin.touched()
			r.PostRestore = &models.PostRestoreReport{Status: models.PostRestoreCompleted}
			if failing {
				r.Status, r.PostRestore.Status = models.RestoreStatusFailed, models.PostRestoreFailed
				return r, errors.New("restore: post-restore command failed: command 1 of 1 (delete on users in " + r.TargetDatabase + "): Unauthorized")
			}
			return r, nil
		}
		res := f.svc.runRestoreTest(context.Background(), job, rec, TriggerManual)

		// The commands go with the restore, deferred until the copy was compared
		// with the manifest (they change it on purpose).
		req := f.restorer.requests[0]
		if !req.DeferPostRestore || len(req.PostRestoreCommands) != 1 {
			t.Fatalf("restore request = %+v", req)
		}
		if !slices.Contains(touchedBefore, "manifest "+res.TempDatabase) {
			t.Fatalf("post-restore commands ran before the comparison: %v", touchedBefore)
		}
		if res.PostRestore == nil {
			t.Fatalf("result without post_restore: %+v", res)
		}
		if !failing {
			if res.Status != models.RestoreTestOK || res.PostRestore.Status != models.PostRestoreCompleted {
				t.Fatalf("result = %+v", res)
			}
			continue
		}
		if res.Status != models.RestoreTestError || !strings.Contains(res.Error, "post-restore commands") || !strings.Contains(res.Error, "Unauthorized") {
			t.Fatalf("failing post-restore commands must fail the test: %+v", res)
		}
		// The temporary database is dropped as always.
		if !res.Dropped || !slices.Equal(f.admin.dropped, []string{res.TempDatabase}) {
			t.Fatalf("dropped = %v", f.admin.dropped)
		}
		neverTouchedSource(t, f, "shop")
	}
}
