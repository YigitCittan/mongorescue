package operations

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
)

// verifyTimeout bounds reading the manifest of a restored database.
const verifyTimeout = 5 * time.Minute

// verifyRestore compares the database rec restored (req, from source) with the
// manifest captured at backup time and records the outcome on rec.Verification. Only
// completed restores are verified. A failed verification keeps the restore completed
// and adds a warning (the data is applied; the record says it cannot be trusted as a
// faithful copy); a verification that could not run is skipped with the reason in
// its notes. It never fails the restore.
func (s *Service) verifyRestore(ctx context.Context, req models.RestoreRequest, source *models.BackupRecord, rec *models.RestoreRecord) {
	if rec == nil || rec.Status != models.RestoreStatusCompleted {
		return
	}
	v := &models.RestoreVerification{CheckedAt: s.now().UTC()}
	rec.Verification = v
	tracker := runs.FromContext(ctx)
	skip := func(note string, warn bool) {
		v.Status, v.Notes = models.RestoreVerificationSkipped, append(v.Notes, note)
		if warn {
			addRecordWarning(rec, "verification skipped: "+note)
		}
		tracker.Printf("verification skipped: %s", note)
	}
	if rec.DryRun {
		skip("dry run: nothing was restored", false)
		return
	}
	if s.cfg.Inspector == nil {
		skip("the restored database cannot be inspected in this setup", true)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyTimeout)
	defer cancel()
	expected := s.manifest(ctx, source)
	if expected == nil {
		skip("the backup has no manifest of its collections (taken by an earlier release, or the capture failed)", true)
		return
	}
	actual, err := s.cfg.Inspector.Manifest(ctx, req.MongoURI, rec.TargetDatabase)
	if err != nil {
		skip("could not inspect the restored database "+rec.TargetDatabase+": "+redact.Text(err.Error()), true)
		return
	}
	v.Mismatches, v.Notes, v.Collections = compareRestored(expected, actual, rec.SelectedCollections, rec.InPlace, req.DropTarget)
	v.CheckedAt = s.now().UTC()
	if len(v.Mismatches) == 0 {
		v.Status = models.RestoreVerificationPassed
		tracker.Printf("verification passed: %d collection(s) match the backup's manifest", v.Collections)
		return
	}
	v.Status = models.RestoreVerificationFailed
	summary := fmt.Sprintf("%d mismatch(es) with the backup's manifest: %s", len(v.Mismatches), v.Mismatches[0])
	if len(v.Mismatches) > 1 {
		summary += fmt.Sprintf(" (+%d more)", len(v.Mismatches)-1)
	}
	addRecordWarning(rec, "verification failed: "+summary)
	tracker.Printf("verification failed: %s", strings.Join(v.Mismatches, "; "))
	s.logger.Warn("restored database does not match the backup's manifest",
		logsafe.Attr("restore_id", rec.ID),
		logsafe.Attr("target_db", rec.TargetDatabase),
		slog.Int("mismatches", len(v.Mismatches)),
	)
}

// compareRestored compares the manifest of a restored database (actual) with the
// backup's manifest (expected) the way the restore wrote it: only the selected
// collections (none selected means all) are expected, and an in-place restore only
// answers for the collections it restored (the target may hold others). Without
// drop_target an in-place restore adds the backup's documents to those already in
// the target, so a larger count is a note, not a mismatch. It returns the
// mismatches, the notes and the number of collections compared.
func compareRestored(expected, actual *models.Manifest, selected []string, inPlace, dropTarget bool) (mismatches, notes []string, compared int) {
	want := &models.Manifest{CapturedAt: expected.CapturedAt}
	sel := trimmedNonEmpty(selected)
	for _, c := range expected.Collections {
		if len(sel) == 0 || slices.Contains(sel, c.Name) {
			want.Collections = append(want.Collections, c)
		}
	}
	got := &models.Manifest{}
	if actual != nil {
		got.CapturedAt = actual.CapturedAt
		for _, c := range actual.Collections {
			// A safe clone of the whole database holds only what was restored, so its
			// extra collections are noted; anything else may have been there before.
			if (!inPlace && len(sel) == 0) || want.Collection(c.Name) != nil {
				got.Collections = append(got.Collections, c)
			}
		}
	}
	if inPlace && !dropTarget {
		for i := range got.Collections {
			c := &got.Collections[i]
			if w := want.Collection(c.Name); w != nil && c.DocumentsMax > w.DocumentsMax {
				notes = append(notes, fmt.Sprintf("collection %s holds %d documents, more than the %d of the backup: the target already held documents (not dropped)",
					c.Name, c.DocumentsMax, w.DocumentsMax))
				c.DocumentsMin, c.DocumentsMax = w.DocumentsMax, w.DocumentsMax
			}
		}
	}
	mismatches, more := models.CompareManifests(want, got)
	return mismatches, append(notes, more...), len(want.Collections)
}

// addRecordWarning appends w to the record's warnings.
func addRecordWarning(rec *models.RestoreRecord, w string) {
	if rec.Warning != "" {
		rec.Warning += "; "
	}
	rec.Warning += w
}
