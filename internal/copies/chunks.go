package copies

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// chunkBatch is how many chunks (or purgeable copies) the queue loads at once;
// maxChunkBatches bounds the batches of one run.
const (
	chunkBatch      = 100
	maxChunkBatches = 50
)

// ChunkStore is the persistence port of the copies of PITR oplog chunks
// (implemented by *store.SQLiteStore). A Config.Store that implements it gets
// chunk copies too: the copies of every chunk of a stream with copy targets
// (pitr.Stream.CopyTargets) go through the same queue, with the same checksum
// check, Object Lock handling and backoff as backup copies, and are purged once
// their chunk is.
type ChunkStore interface {
	// ListStreams returns every PITR stream.
	ListStreams(ctx context.Context) ([]*pitr.Stream, error)
	// PlanChunkCopies queues the missing copies of the live chunks of a stream on
	// targets and drops its queued copies on other targets.
	PlanChunkCopies(ctx context.Context, streamID string, targets []models.CopyTarget) (int64, error)
	// DueChunkCopies returns up to limit live chunks with a copy due at now, oldest
	// first, after the chunk created at afterCreated with ID afterID (a cursor; the
	// zero time and "" start at the beginning).
	DueChunkCopies(ctx context.Context, now, afterCreated time.Time, afterID string, limit int) ([]*pitr.Chunk, error)
	// ChunkCopiesOf returns the copies of chunks by chunk ID.
	ChunkCopiesOf(ctx context.Context, ids []string) (map[string][]*models.ChunkCopy, error)
	// UpdateChunkCopy applies fn to one copy in a transaction.
	UpdateChunkCopy(ctx context.Context, chunkID, targetID string, fn func(*models.ChunkCopy) error) (*models.ChunkCopy, error)
	// PurgeableChunkCopies returns up to limit copies whose chunk is gone and
	// whose lock ended at now.
	PurgeableChunkCopies(ctx context.Context, now time.Time, limit int) ([]*models.ChunkCopy, error)
	// CountWaitingChunkCopies counts the chunk copies waiting in the queue.
	CountWaitingChunkCopies(ctx context.Context) (int64, error)
}

// chunkStore returns the store's ChunkStore, or nil.
func (s *Service) chunkStore() ChunkStore {
	cs, _ := s.cfg.Store.(ChunkStore)
	return cs
}

// runChunks plans, copies and purges the copies of oplog chunks once.
func (s *Service) runChunks(ctx context.Context, cs ChunkStore) error {
	var errs []error
	streams, err := cs.ListStreams(ctx)
	if err != nil {
		return fmt.Errorf("list the PITR streams: %w", err)
	}
	for _, st := range streams {
		targets := make([]models.CopyTarget, 0, len(st.CopyTargets))
		for _, id := range st.CopyTargets {
			targets = append(targets, models.CopyTarget{ID: id})
		}
		if _, err = cs.PlanChunkCopies(ctx, st.ID, targets); err != nil {
			errs = append(errs, err)
		}
	}
	// One pass over the queue: a cursor moves through it, and a copy tried in this
	// run is never tried again in it (a failure waits for its backoff, even when
	// the run lasts longer than that).
	var afterCreated time.Time
	afterID := ""
	tried := map[string]bool{}
	for range maxChunkBatches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chunks, listErr := cs.DueChunkCopies(ctx, s.now(), afterCreated, afterID, chunkBatch)
		if listErr != nil {
			errs = append(errs, fmt.Errorf("list the due chunk copies: %w", listErr))
			break
		}
		ids := make([]string, len(chunks))
		for i, c := range chunks {
			ids[i] = c.ID
		}
		copies, listErr := cs.ChunkCopiesOf(ctx, ids)
		if listErr != nil {
			errs = append(errs, listErr)
			break
		}
		for _, c := range chunks {
			for _, cp := range copies[c.ID] {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				key := c.ID + "\x00" + cp.TargetID
				if tried[key] || !models.CopyDue(&cp.BackupCopy, s.now()) {
					continue
				}
				tried[key] = true
				if err = s.attemptChunk(ctx, cs, c, cp.TargetID); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if len(chunks) < chunkBatch {
			break
		}
		last := chunks[len(chunks)-1]
		afterCreated, afterID = last.CreatedAt, last.ID
	}
	if err = s.purgeChunkCopies(ctx, cs); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// attemptChunk copies chunk c to copy target targetID once, under the deletion
// lock of the copy's object (the lock the purge takes), and records the outcome.
func (s *Service) attemptChunk(ctx context.Context, cs ChunkStore, c *pitr.Chunk, targetID string) error {
	unlock, err := runs.LockDeletion(ctx, runs.ArchiveKey(targetID, c.StorageKey))
	if err != nil {
		return err
	}
	defer unlock()
	// The chunk's object read and checked like a backup archive.
	src := &models.BackupRecord{ID: c.ID, StorageTargetID: c.TargetID, StorageKey: c.StorageKey, StorageVersionID: c.VersionID,
		SHA256: c.SHA256, SizeBytes: c.SizeBytes}
	cp := &models.BackupCopy{TargetID: targetID, StorageKey: c.StorageKey}
	obj, copyErr := s.copyOne(ctx, src, cp, s.mbps(ctx, src))
	if ctx.Err() != nil && copyErr != nil {
		return ctx.Err()
	}
	at := s.now().UTC()
	updated, err := cs.UpdateChunkCopy(context.WithoutCancel(ctx), c.ID, targetID, func(cc *models.ChunkCopy) error {
		switch {
		case cc.Status == models.CopyPurged || cc.StorageKey != c.StorageKey:
			return errGone
		case !waiting(&cc.BackupCopy):
			return errSkip
		}
		cc.Attempts++
		if copyErr == nil {
			succeed(&cc.BackupCopy, obj, at, c.SHA256)
			return nil
		}
		cc.RecordFailure(redact.Text(copyErr.Error()), obj)
		cc.NextAttemptAt = nil
		if cc.Attempts < s.cfg.MaxAttempts {
			next := at.Add(s.backoff(cc.Attempts))
			cc.NextAttemptAt = &next
		}
		return nil
	})
	attrs := []any{logsafe.Attr("chunk_id", c.ID), logsafe.Attr("storage_target_id", targetID)}
	switch {
	case errors.Is(err, errSkip):
		return nil
	case errors.Is(err, errGone), errors.Is(err, store.ErrNotFound):
		if copyErr == nil || obj != nil {
			s.removeUntracked(ctx, c.ID, targetID, c.StorageKey, obj)
		}
		return nil
	case err != nil:
		return fmt.Errorf("record the copy of chunk %s to %s: %w", c.ID, targetID, err)
	case copyErr != nil:
		s.logger.Warn("oplog chunk copy failed", append(attrs, slog.Int("attempts", updated.Attempts), slog.String("error", updated.Error))...)
		return fmt.Errorf("copy chunk %s to %s: %w", c.ID, targetID, copyErr)
	}
	s.logger.Debug("oplog chunk copied", attrs...)
	return nil
}

// purgeChunkCopies deletes the copies of chunks that are gone (see
// ChunkStore.PurgeableChunkCopies), each under its deletion lock. A copy whose
// object is still under its Object Lock records the end of the lock and waits.
func (s *Service) purgeChunkCopies(ctx context.Context, cs ChunkStore) error {
	list, err := cs.PurgeableChunkCopies(ctx, s.now(), chunkBatch)
	if err != nil {
		return fmt.Errorf("list the purgeable chunk copies: %w", err)
	}
	var errs []error
	for _, cp := range list {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = s.purgeChunkCopy(ctx, cs, cp); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// purgeChunkCopy deletes the object of copy cp and marks it purged.
func (s *Service) purgeChunkCopy(ctx context.Context, cs ChunkStore, cp *models.ChunkCopy) error {
	unlock, err := runs.LockDeletion(ctx, runs.ArchiveKey(cp.TargetID, cp.StorageKey))
	if err != nil {
		return err
	}
	defer unlock()
	var until *time.Time
	if cp.MayExist() {
		dst, openErr := s.cfg.Storages(ctx, cp.TargetID)
		if openErr != nil {
			return fmt.Errorf("open copy target %s: %w", cp.TargetID, openErr)
		}
		until, err = storage.Purge(ctx, dst, cp.StorageKey, cp.VersionID, s.now())
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("purge the copy of chunk %s on %s: %w", cp.ChunkID, cp.TargetID, err)
		}
	}
	at := s.now().UTC()
	_, err = cs.UpdateChunkCopy(context.WithoutCancel(ctx), cp.ChunkID, cp.TargetID, func(cc *models.ChunkCopy) error {
		if until != nil {
			u := until.UTC()
			cc.RetainUntil = &u
			return nil
		}
		cc.Status, cc.PurgedAt, cc.NextAttemptAt = models.CopyPurged, &at, nil
		return nil
	})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("record the purge of the copy of chunk %s: %w", cp.ChunkID, err)
	}
	return nil
}
