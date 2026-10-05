package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// RetentionActor is the DeletedBy of base backups deleted by PITR retention.
const RetentionActor = "pitr-retention"

// purgeBatch bounds the chunks one retention run purges per stream.
const purgeBatch = 1000

// BaseUpdater updates a stored backup record atomically (implemented by
// *store.SQLiteStore). Retention deletes base backups through it.
type BaseUpdater func(ctx context.Context, id string, fn func(*models.BackupRecord) error) (*models.BackupRecord, error)

// errBaseChanged skips a base that changed since retention planned its deletion.
var errBaseChanged = errors.New("collector: the base backup changed since retention planned it")

// RetentionPlan is what retention does to one stream.
type RetentionPlan struct {
	// DeleteBases lists the base backups to delete.
	DeleteBases []string `json:"delete_bases"`
	// DeleteChunks lists the chunks to delete.
	DeleteChunks []string `json:"delete_chunks"`
}

// PlanRetention decides at now what the retention of stream st deletes, without
// changing anything. bases are the stream's base backups, chains its chains and
// chunks the chunks of each chain (in order).
//
// Bases: the BaseKeepCount newest completed bases and those younger than
// BaseKeepDays are kept, and so are pinned bases and the newest eligible one (its
// chain covers [T_before, T_after]). Chunks: in a chain with kept eligible bases,
// a chunk whose end is before the T_before of the oldest of them is deleted; an
// ended chain without one is deleted whole, while the open chain is kept (its
// base may still be running). Superseded chunks are deleted. OplogMaxDays, when
// set, also deletes the chunks that end more than that many days ago.
func PlanRetention(now time.Time, st *pitr.Stream, bases []*models.BackupRecord, chains []*pitr.Chain, chunks map[string][]*pitr.Chunk) RetentionPlan {
	plan := RetentionPlan{DeleteBases: []string{}, DeleteChunks: []string{}}
	spans := map[string]pitr.ChainSpan{}
	for _, c := range chains {
		spans[c.ChainID] = liveSpan(c.ChainID, chunks[c.ChainID])
	}
	chainOf := func(b *models.BackupRecord) string {
		for _, c := range chains {
			if covers(spans[c.ChainID], b) {
				return c.ChainID
			}
		}
		return ""
	}
	var completed []*models.BackupRecord
	for _, b := range bases {
		if b.Status == models.StatusCompleted && b.InstanceScope() {
			completed = append(completed, b)
		}
	}
	slices.SortStableFunc(completed, func(a, b *models.BackupRecord) int { return b.StartedAt.Compare(a.StartedAt) })
	newestEligible := ""
	for _, b := range completed {
		if eligibleBase(b) && chainOf(b) != "" {
			newestEligible = b.ID
			break
		}
	}
	keptBefore := map[string]pitr.Timestamp{} // chain -> oldest T_before of its kept eligible bases
	for i, b := range completed {
		keep := i < st.BaseKeepCount || b.Pinned || b.ID == newestEligible ||
			(st.BaseKeepDays > 0 && now.Sub(b.StartedAt) < time.Duration(st.BaseKeepDays)*24*time.Hour)
		if !keep {
			plan.DeleteBases = append(plan.DeleteBases, b.ID)
			continue
		}
		if chain := chainOf(b); chain != "" && eligibleBase(b) {
			if cur, ok := keptBefore[chain]; !ok || b.TBefore.TS.Compare(cur) < 0 {
				keptBefore[chain] = b.TBefore.TS
			}
		}
	}
	var maxAge pitr.Timestamp
	if st.OplogMaxDays > 0 {
		maxAge = pitr.Timestamp{T: uint32(now.Add(-time.Duration(st.OplogMaxDays) * 24 * time.Hour).Unix())} //nolint:gosec // oplog timestamps are uint32 seconds
	}
	for _, c := range chains {
		cutoff, kept := keptBefore[c.ChainID]
		for _, ch := range chunks[c.ChainID] {
			if ch.DeletedAt != nil || ch.Status == pitr.ChunkPruned {
				continue
			}
			del := ch.Status == pitr.ChunkSuperseded ||
				(kept && ch.To.Compare(cutoff) < 0) ||
				(!kept && !c.Open()) ||
				(!maxAge.IsZero() && ch.To.Compare(maxAge) < 0)
			if del {
				plan.DeleteChunks = append(plan.DeleteChunks, ch.ID)
			}
		}
	}
	return plan
}

// liveSpan returns the span of the live chunks of a chain.
func liveSpan(chainID string, chunks []*pitr.Chunk) pitr.ChainSpan {
	sp := pitr.ChainSpan{ChainID: chainID}
	for _, c := range chunks {
		if !c.Live() {
			continue
		}
		if sp.Chunks == 0 || c.From.Compare(sp.From) < 0 {
			sp.From = c.From
		}
		if sp.Chunks == 0 || c.To.Compare(sp.To) > 0 {
			sp.To = c.To
		}
		sp.Chunks++
		sp.SizeBytes += c.SizeBytes
	}
	return sp
}

// applyRetention applies the retention of every stream and purges the chunks whose
// grace period ended.
func (s *Service) applyRetention(ctx context.Context) {
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		return
	}
	for _, st := range streams {
		if ctx.Err() != nil {
			return
		}
		if err := s.RetainStream(ctx, st.ID); err != nil && ctx.Err() == nil {
			s.logger.Warn("PITR retention failed", logsafe.Attr("stream_id", st.ID), logsafe.Error(err))
		}
	}
}

// RetainStream applies the retention of stream id now (see PlanRetention) and
// purges its deleted chunks whose grace period ended. Deleted bases and chunks keep
// their objects until the delete grace period ends; bases are then purged with the
// other deleted backups. It holds runs.LockDeletion("pitr:<stream>").
func (s *Service) RetainStream(ctx context.Context, id string) error {
	unlock, err := lockStream(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return err
	}
	now := s.now()
	if err = s.purgeChunks(ctx, st, now); err != nil {
		return err
	}
	if s.cfg.Bases == nil {
		return nil // without bases no window can be proven: keep everything
	}
	bases, err := s.cfg.Bases(ctx, id)
	if err != nil {
		return fmt.Errorf("list base backups: %w", err)
	}
	chains, err := s.cfg.Repo.ListChains(ctx, id)
	if err != nil {
		return err
	}
	chunks := map[string][]*pitr.Chunk{}
	for _, c := range chains {
		if chunks[c.ChainID], err = s.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: id, ChainID: c.ChainID}); err != nil {
			return err
		}
	}
	plan := PlanRetention(now, st, bases, chains, chunks)
	purgeAfter := now.Add(s.deleteGrace())
	if s.cfg.UpdateBase != nil {
		for _, bid := range plan.DeleteBases {
			_, err := s.cfg.UpdateBase(ctx, bid, func(r *models.BackupRecord) error {
				if r.Pinned || r.Status != models.StatusCompleted {
					return errBaseChanged
				}
				r.MarkDeleted(models.SoftDelete{At: now, PurgeAfter: purgeAfter, By: RetentionActor,
					Reason: fmt.Sprintf("PITR base retention of stream %s", id)})
				return nil
			})
			switch {
			case errors.Is(err, errBaseChanged):
			case err != nil:
				return fmt.Errorf("delete base backup %s: %w", bid, err)
			default:
				s.logger.Info("PITR retention deleted a base backup; its archive is kept until the grace period ends",
					logsafe.Attr("stream_id", id), logsafe.Attr("backup_id", bid))
			}
		}
	}
	if len(plan.DeleteChunks) > 0 {
		n, err := s.cfg.Repo.DeleteChunks(ctx, plan.DeleteChunks, now, purgeAfter)
		if err != nil {
			return fmt.Errorf("delete chunks: %w", err)
		}
		s.logger.Info("PITR retention deleted oplog chunks; their objects are kept until the grace period ends",
			logsafe.Attr("stream_id", id), slog.Int64("chunks", n))
	}
	s.observeWindow(ctx, id)
	return nil
}

// purgeChunks removes the objects of the deleted chunks of st whose grace period
// ended and marks them pruned. Its caller holds the stream's deletion lock.
func (s *Service) purgeChunks(ctx context.Context, st *pitr.Stream, now time.Time) error {
	due, err := s.cfg.Repo.ListPurgeableChunks(ctx, st.ID, now, purgeBatch)
	if err != nil {
		return fmt.Errorf("list purgeable chunks: %w", err)
	}
	var errs []error
	for _, c := range due {
		if err := s.purgeChunk(ctx, c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// purgeChunk removes the object of deleted chunk c under its archive lock.
func (s *Service) purgeChunk(ctx context.Context, c *pitr.Chunk) error {
	unlock, err := runs.LockDeletion(ctx, runs.ArchiveKey(c.TargetID, c.StorageKey))
	if err != nil {
		return err
	}
	defer unlock()
	driver, err := s.cfg.Storage(ctx, c.TargetID)
	if err == nil {
		err = driver.Delete(ctx, c.StorageKey)
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("delete the object of chunk %s: %w", c.ID, err)
	}
	if err := s.cfg.Repo.MarkChunkPruned(ctx, c.ID); err != nil && !errors.Is(err, pitr.ErrNotFound) {
		return err
	}
	return nil
}

// observeWindow exports the newest window of stream id.
func (s *Service) observeWindow(ctx context.Context, id string) {
	if s.cfg.Observer == nil {
		return
	}
	status, err := s.Status(ctx, id)
	if err != nil {
		return
	}
	if n := len(status.Windows); n > 0 {
		w := status.Windows[n-1]
		s.cfg.Observer.SetPITRWindow(id, w.StartTime, w.EndTime)
		return
	}
	s.cfg.Observer.SetPITRWindow(id, time.Time{}, time.Time{})
}
