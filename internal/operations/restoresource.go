package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// restoreSource returns the view of backup rec a restore reads (see
// models.BackupRecord.AtCopy) and, when it falls back to a copy, why.
//
// want names the storage target to read from: rec's primary target or the target of
// a completed copy (anything else is an ErrInvalid). Without want, a backup whose
// primary archive is missing, failed its last verification or has the wrong size on
// its target is read from its first healthy copy that exists; the reason is
// returned. A backup without such a copy is read from its primary as before.
func (s *Service) restoreSource(ctx context.Context, rec *models.BackupRecord, want string) (*models.BackupRecord, string, error) {
	if want != "" && want != rec.StorageTargetID {
		c := rec.Copy(want)
		if !rec.CopyUsable(c) {
			return nil, "", public(fmt.Sprintf("backup %s has no completed copy of its current archive on storage target %s", rec.ID, want), ErrInvalid)
		}
		return rec.AtCopy(c), "", nil
	}
	if want != "" || len(rec.Copies) == 0 {
		return rec, "", nil
	}
	problem := s.archiveProblem(ctx, rec)
	if problem == "" {
		return rec, "", nil
	}
	for i := range rec.Copies {
		c := &rec.Copies[i]
		if !rec.CopyUsable(c) {
			continue
		}
		view := rec.AtCopy(c)
		if s.archiveProblem(ctx, view) != "" {
			continue
		}
		return view, fmt.Sprintf("the primary archive on storage target %s %s; the restore reads the copy on %s instead",
			targetLabel(rec.StorageTargetName, rec.StorageTargetID), problem, targetLabel(c.TargetName, c.TargetID)), nil
	}
	return rec, "", nil
}

// archiveProblem returns why the archive of rec (a backup or a copy view) cannot
// be read as recorded, or "": it is recorded missing or damaged, its target cannot
// be opened, or the object is missing or has another size than recorded. The
// content itself is not read (the integrity sweep compares checksums).
func (s *Service) archiveProblem(ctx context.Context, rec *models.BackupRecord) string {
	switch {
	case rec.Status == models.StatusMissing:
		return "is missing"
	case rec.Verification == models.VerificationMismatch:
		return "failed its last verification"
	case s.cfg.Storage == nil:
		return ""
	}
	driver, err := s.cfg.Storage(ctx, rec.StorageTargetID)
	if err != nil {
		return "cannot be opened"
	}
	obj, err := driver.Stat(ctx, rec.StorageKey)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return "is missing"
	case err != nil:
		return "cannot be read"
	case rec.SizeBytes > 0 && obj != nil && obj.SizeBytes > 0 && obj.SizeBytes != rec.SizeBytes:
		return fmt.Sprintf("has %d bytes instead of %d", obj.SizeBytes, rec.SizeBytes)
	}
	return ""
}

// targetLabel returns a storage target's name, else its ID.
func targetLabel(name, id string) string {
	if name != "" {
		return name
	}
	return id
}

// sourceCheck adds the source check of a preflight for a backup with copies: the
// archive the restore reads must exist on its target.
func (p *preflightRun) sourceCheck() {
	if len(p.source.Copies) == 0 && p.req.SourceFallback == "" {
		return
	}
	where := targetLabel(p.source.StorageTargetName, p.source.StorageTargetID)
	if problem := p.svc.archiveProblem(p.ctx, p.source); problem != "" {
		p.res.Add(models.PreflightCheckSource, models.PreflightFail, fmt.Sprintf("the archive on storage target %s %s", where, problem))
		return
	}
	if p.req.SourceFallback != "" {
		p.res.Add(models.PreflightCheckSource, models.PreflightWarn, p.req.SourceFallback)
		return
	}
	p.res.Add(models.PreflightCheckSource, models.PreflightPass, "the archive is read from storage target "+where)
}
