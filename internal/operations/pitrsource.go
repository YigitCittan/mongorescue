package operations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// chunkCopyLister lists the copies of oplog chunks (implemented by
// *store.SQLiteStore).
type chunkCopyLister interface {
	ChunkCopiesOf(ctx context.Context, ids []string) (map[string][]*models.ChunkCopy, error)
}

// pitrSource points the run of pp at the storage target a point-in-time restore
// reads, and returns the reason of a fallback ("" without one). With want (the
// request's source_target_id) other than the primary, the base and every chunk of
// the plan must have a complete copy on want (a copy chain), else the restore is
// refused with ErrPITRNotRestorable. Without want the restore reads the primary,
// unless its base archive or a chunk cannot be read and a copy target of the
// stream holds a complete copy chain: then it reads that one.
func (s *Service) pitrSource(ctx context.Context, pp *pitrPlan, want string) (string, error) {
	run := pp.run
	if run.Plan == nil || run.Base == nil {
		return "", nil
	}
	want = strings.TrimSpace(want)
	if want == "" || (want == run.Base.StorageTargetID && onTarget(run.Plan.Chunks, want)) {
		if want != "" || len(pp.stream.CopyTargets) == 0 {
			return "", nil
		}
		problem := s.pitrProblem(ctx, run)
		if problem == "" {
			return "", nil
		}
		for _, t := range pp.stream.CopyTargets {
			view, err := s.copyChain(ctx, run, t)
			if err != nil {
				continue
			}
			pp.run = view
			return fmt.Sprintf("the primary %s; the restore reads the copy chain on %s instead", problem,
				targetLabel(view.Base.StorageTargetName, t)), nil
		}
		return "", nil
	}
	view, err := s.copyChain(ctx, run, want)
	if err != nil {
		return "", public(err.Error(), ErrPITRNotRestorable, err)
	}
	pp.run = view
	return "", nil
}

// hasUsableCopy reports whether r has a copy a restore can read instead of its
// archive (models.BackupRecord.CopyUsable).
func hasUsableCopy(r *models.BackupRecord) bool {
	for i := range r.Copies {
		if r.CopyUsable(&r.Copies[i]) {
			return true
		}
	}
	return false
}

// onTarget reports whether every chunk is stored on target.
func onTarget(chunks []*pitr.Chunk, target string) bool {
	for _, c := range chunks {
		if c.TargetID != target {
			return false
		}
	}
	return true
}

// pitrProblem returns why run cannot be read from where it was planned, or "": its
// base archive has a problem (archiveProblem), or a chunk's object is missing or
// cannot be read.
func (s *Service) pitrProblem(ctx context.Context, run restore.PITRRun) string {
	if p := s.archiveProblem(ctx, run.Base); p != "" {
		return "base backup " + run.Base.ID + " " + p
	}
	if s.cfg.Storage == nil {
		return ""
	}
	drivers := map[string]storage.Storage{}
	for _, c := range run.Plan.Chunks {
		d, ok := drivers[c.TargetID]
		if !ok {
			var err error
			if d, err = s.cfg.Storage(ctx, c.TargetID); err != nil {
				return "storage target " + c.TargetID + " of the oplog chunks cannot be opened"
			}
			drivers[c.TargetID] = d
		}
		obj, err := d.Stat(ctx, c.StorageKey)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			return fmt.Sprintf("oplog chunk %s (%s-%s) is missing", c.ID, c.From, c.To)
		case err != nil:
			return fmt.Sprintf("oplog chunk %s (%s-%s) cannot be read", c.ID, c.From, c.To)
		case c.SizeBytes > 0 && obj != nil && obj.SizeBytes > 0 && obj.SizeBytes != c.SizeBytes:
			return fmt.Sprintf("oplog chunk %s (%s-%s) has %d bytes instead of %d", c.ID, c.From, c.To, obj.SizeBytes, c.SizeBytes)
		}
	}
	return ""
}

// copyChain returns run reading from copy target t: the base's copy there and a
// copy of every chunk. A chain is only valid when every chunk in the range has a
// complete copy on t checked against the chunk's checksum; otherwise it fails
// naming the first gap.
func (s *Service) copyChain(ctx context.Context, run restore.PITRRun, t string) (restore.PITRRun, error) {
	base := run.Base
	c := base.Copy(t)
	if !base.CopyUsable(c) {
		return run, fmt.Errorf("base backup %s has no completed copy on storage target %s", base.ID, t)
	}
	lister, ok := s.cfg.PITR.(chunkCopyLister)
	if !ok {
		return run, fmt.Errorf("the oplog chunks have no copies on storage target %s", t)
	}
	ids := make([]string, len(run.Plan.Chunks))
	for i, ch := range run.Plan.Chunks {
		ids[i] = ch.ID
	}
	copies, err := lister.ChunkCopiesOf(ctx, ids)
	if err != nil {
		return run, fmt.Errorf("list the chunk copies: %w", err)
	}
	chunks := make([]*pitr.Chunk, 0, len(run.Plan.Chunks))
	for _, ch := range run.Plan.Chunks {
		i := slices.IndexFunc(copies[ch.ID], func(cp *models.ChunkCopy) bool {
			return cp.TargetID == t && cp.Healthy() && cp.SHA256OK && strings.EqualFold(cp.SHA256, ch.SHA256)
		})
		if i < 0 {
			return run, fmt.Errorf("the copy chain on storage target %s is incomplete: oplog chunk %s (%s-%s) has no completed copy there", t, ch.ID, ch.From, ch.To)
		}
		cp := copies[ch.ID][i]
		view := *ch
		view.TargetID, view.StorageKey, view.VersionID, view.RetainUntil = t, cp.StorageKey, cp.VersionID, cp.RetainUntil
		view.VerifiedAt, view.VerifyError = cp.VerifiedAt, cp.VerificationError
		chunks = append(chunks, &view)
	}
	plan := *run.Plan
	plan.Chunks = chunks
	out := run
	out.Plan, out.Base = &plan, base.AtCopy(c)
	return out, nil
}
