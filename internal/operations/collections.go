package operations

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// Sources of BackupCollections.Source.
const (
	// CollectionsFromArchive means the list was read from the archive prelude.
	CollectionsFromArchive = "archive"
	// CollectionsFromRecord means the archive could not be read and the list is the
	// backup record's own collection filter (empty for a whole-database backup).
	CollectionsFromRecord = "record"
)

// ArchivePreviewTimeout bounds reading the collection list of one archive.
const ArchivePreviewTimeout = 30 * time.Second

// archiveCacheSize is the number of backups whose archive collection list is cached.
const archiveCacheSize = 64

// ArchiveLister lists the collections stored in a backup archive (implemented by
// *restore.Engine). A RestoreEngine that also implements it enables the archive
// contents preview of ListBackupCollections.
type ArchiveLister interface {
	// ArchiveCollections reads the collection list from the archive prelude of rec.
	ArchiveCollections(ctx context.Context, rec *models.BackupRecord) ([]models.BackupCollection, error)
}

// BackupCollections is the collection list of a backup, offered for a selective
// restore.
type BackupCollections struct {
	// BackupID is the backup the list belongs to.
	BackupID string `json:"backup_id"`
	// Database is the database of the backup.
	Database string `json:"database"`
	// Source is CollectionsFromArchive or CollectionsFromRecord.
	Source string `json:"source"`
	// Collections lists the collections (views and time-series collections included).
	Collections []models.BackupCollection `json:"collections"`
	// Warning explains why the archive could not be read when Source is
	// CollectionsFromRecord.
	Warning string `json:"warning,omitempty"`
}

// ListBackupCollections returns the collections stored in backup id, read from the
// prelude of its archive (only the prelude is streamed, see restore.Engine.
// ArchiveCollections) within ArchivePreviewTimeout. Lists read from completed backups
// are cached per backup ID.
//
// When the archive cannot be read (an older or damaged artifact, a missing key), the
// list falls back to the record's own collection filter with Source
// CollectionsFromRecord and a Warning. An encrypted backup without key material and
// without such a filter fails with ErrKeyRequired and the same hint as a restore. A
// missing backup yields ErrNotFound. Only completed backups are read from the cache
// (and cached); a failed or pruned backup has no complete archive and gets the record
// fallback without a read, and its cache entry, if any, is evicted.
func (s *Service) ListBackupCollections(ctx context.Context, id string) (*BackupCollections, error) {
	return s.ListBackupCollectionsWithin(ctx, id, 0)
}

// ListBackupCollectionsWithin is ListBackupCollections with the archive read bounded
// by timeout instead of the configured preview timeout (0 keeps it; a longer timeout
// than the configured one is cut to it). Adapters whose responses must be written
// within a shorter deadline pass one, so the record fallback still reaches the client.
func (s *Service) ListBackupCollectionsWithin(ctx context.Context, id string, timeout time.Duration) (*BackupCollections, error) {
	if strings.TrimSpace(id) == "" {
		return nil, public("backup id required", ErrInvalid)
	}
	rec, err := s.cfg.Store.GetBackupRecord(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.archiveCache.remove(id)
			return nil, public("backup not found", ErrNotFound, err)
		}
		return nil, fmt.Errorf("load backup: %w", err)
	}
	out := &BackupCollections{BackupID: rec.ID, Database: rec.Database}

	switch rec.Status {
	case models.StatusCompleted:
		if cached, ok := s.archiveCache.get(rec.ID); ok {
			out.Source, out.Collections = CollectionsFromArchive, cached
			return out, nil
		}
	case models.StatusFailed:
		s.archiveCache.remove(rec.ID)
		return s.recordCollections(out, rec, "the backup failed; it has no complete archive"), nil
	case models.StatusPruned:
		s.archiveCache.remove(rec.ID)
		return s.recordCollections(out, rec, "the backup was pruned; its archive was deleted"), nil
	default:
		s.archiveCache.remove(rec.ID)
	}

	lister, ok := s.cfg.Restore.(ArchiveLister)
	if !ok {
		return s.recordCollections(out, rec, "reading backup archives is not available"), nil
	}
	limit := s.cfg.PreviewTimeout
	if limit <= 0 {
		limit = ArchivePreviewTimeout
	}
	if timeout <= 0 || timeout > limit {
		timeout = limit
	}
	readCtx, cancel := context.WithTimeoutCause(ctx, timeout, errPreviewTimeout)
	defer cancel()
	list, err := s.readArchive(readCtx, lister, rec)
	if err == nil {
		if list == nil {
			list = []models.BackupCollection{}
		}
		out.Source, out.Collections = CollectionsFromArchive, list
		if rec.Status == models.StatusCompleted {
			s.archiveCache.put(rec.ID, list)
		}
		return out, nil
	}
	if ctx.Err() != nil {
		// The client went away; nothing to report.
		return nil, ctx.Err()
	}
	s.logger.Warn("could not read the collection list of a backup archive; using the record",
		slog.String("backup_id", rec.ID), logsafe.Error(err))

	keyMissing := errors.Is(err, ErrKeyRequired)
	if keyMissing && len(rec.Collections) == 0 {
		return nil, fmt.Errorf("%w: backup %s is encrypted; %s", ErrKeyRequired, rec.ID, restore.KeyRequiredHint)
	}
	var reason string
	switch {
	case keyMissing:
		reason = "the backup is encrypted; " + restore.KeyRequiredHint
	case errors.Is(err, errPreviewTimeout) || errors.Is(err, context.DeadlineExceeded):
		reason = fmt.Sprintf("reading the archive timed out after %s", timeout)
	case errors.Is(err, restore.ErrNoArtifact):
		reason = "the backup has no stored archive"
	default:
		reason = "the archive's collection list could not be read: " + redact.Text(err.Error())
	}
	return s.recordCollections(out, rec, reason), nil
}

// errPreviewTimeout is the cause of a preview that exceeded ArchivePreviewTimeout.
var errPreviewTimeout = errors.New("archive preview timed out")

// maxConcurrentPreviews bounds the archive previews read at the same time, so a
// client listing many uncached backups cannot open an unbounded number of storage
// streams; the others wait (within their timeout) for a slot.
const maxConcurrentPreviews = 4

// readArchive runs lister for rec once a preview slot is free.
func (s *Service) readArchive(ctx context.Context, lister ArchiveLister, rec *models.BackupRecord) ([]models.BackupCollection, error) {
	select {
	case s.previewSlots <- struct{}{}:
		defer func() { <-s.previewSlots }()
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	return lister.ArchiveCollections(ctx, rec)
}

// recordCollections fills out from the record's own collection filter.
func (s *Service) recordCollections(out *BackupCollections, rec *models.BackupRecord, reason string) *BackupCollections {
	out.Source, out.Warning = CollectionsFromRecord, reason
	out.Collections = make([]models.BackupCollection, 0, len(rec.Collections))
	for _, name := range rec.Collections {
		if name = strings.TrimSpace(name); name != "" {
			out.Collections = append(out.Collections, models.BackupCollection{Name: name, Type: models.CollectionTypeCollection})
		}
	}
	return out
}

// collectionCache is a small, concurrency-safe LRU cache of archive collection lists
// keyed by backup ID. Backups are immutable, so entries never go stale; a deleted
// backup is looked up in the store before the cache.
type collectionCache struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	id    string
	value []models.BackupCollection
}

// newCollectionCache returns a cache holding at most size entries.
func newCollectionCache(size int) *collectionCache {
	return &collectionCache{size: size, order: list.New(), items: map[string]*list.Element{}}
}

// get returns a copy of the list cached for id.
func (c *collectionCache) get(id string) ([]models.BackupCollection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return slices.Clone(el.Value.(*cacheEntry).value), true
}

// put stores a copy of value for id, evicting the least recently used entry when full.
func (c *collectionCache) put(id string, value []models.BackupCollection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value = slices.Clone(value)
	if el, ok := c.items[id]; ok {
		el.Value.(*cacheEntry).value = value
		c.order.MoveToFront(el)
		return
	}
	c.items[id] = c.order.PushFront(&cacheEntry{id: id, value: value})
	for c.order.Len() > c.size {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*cacheEntry).id)
	}
}

// remove evicts the entry for id, if any.
func (c *collectionCache) remove(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[id]; ok {
		c.order.Remove(el)
		delete(c.items, id)
	}
}

// len returns the number of cached entries.
func (c *collectionCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
