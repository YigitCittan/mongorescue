package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/cost"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// Compile-time check that the SQLite store is the cost tracker's repository.
var _ cost.Repository = (*SQLiteStore)(nil)

// usageKey groups archives that share one stored object.
type usageKey struct{ target, key string }

// LiveUsage aggregates the archives and oplog chunks that exist now per target,
// kind, job, database and PITR stream (cost.Repository). Primary archives are the
// backups that are completed or soft-deleted (their archive stays until the purge);
// records that name the same target and key count once. Copies are the done copies
// of any backup record (a retained copy outlives its primary's record status) and
// are priced at the primary's size. PITR rows are the committed and superseded
// chunks plus the done copies of chunks. A record without a storage target has
// TargetID "".
func (s *SQLiteStore) LiveUsage(ctx context.Context) ([]cost.Row, error) {
	acc := newUsageAcc()
	if err := s.primaryUsage(ctx, acc); err != nil {
		return nil, err
	}
	if err := s.copyUsage(ctx, acc); err != nil {
		return nil, err
	}
	if err := s.chunkUsage(ctx, acc); err != nil {
		return nil, err
	}
	return acc.rows(), nil
}

// usageAcc sums rows by their group, in first-seen order.
type usageAcc struct {
	idx  map[cost.Row]int
	list []cost.Row
}

func newUsageAcc() *usageAcc { return &usageAcc{idx: map[cost.Row]int{}} }

// add counts objects objects of bytes bytes in the group of r.
func (a *usageAcc) add(r cost.Row, objects, bytes int64) {
	k := r
	k.Objects, k.Bytes = 0, 0
	i, ok := a.idx[k]
	if !ok {
		i = len(a.list)
		a.idx[k] = i
		a.list = append(a.list, k)
	}
	a.list[i].Objects += objects
	a.list[i].Bytes += bytes
}

func (a *usageAcc) rows() []cost.Row { return a.list }

func (s *SQLiteStore) primaryUsage(ctx context.Context, acc *usageAcc) error {
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(json_extract(data, '$.storage_target_id'), ''), job_id, database_name,
			coalesce(json_extract(data, '$.storage_key'), ''), coalesce(json_extract(data, '$.size_bytes'), 0)
		FROM backups WHERE status IN ('completed', 'deleted') ORDER BY started_at, id`)
	if err != nil {
		return fmt.Errorf("store: aggregate backup sizes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[usageKey]bool{}
	for rows.Next() {
		var target, job, db, key string
		var size int64
		if err = rows.Scan(&target, &job, &db, &key, &size); err != nil {
			return fmt.Errorf("store: scan backup size: %w", err)
		}
		if key == "" {
			continue
		}
		k := usageKey{target, key}
		if seen[k] {
			continue
		}
		seen[k] = true
		acc.add(cost.Row{TargetID: target, Kind: cost.KindBackup, JobID: job, Database: db}, 1, max(size, 0))
	}
	return rows.Err()
}

func (s *SQLiteStore) copyUsage(ctx context.Context, acc *usageAcc) error {
	rows, err := s.db.QueryContext(ctx, `SELECT coalesce(json_extract(c.value, '$.target_id'), ''), b.job_id, b.database_name,
			coalesce(json_extract(c.value, '$.retained'), 0), coalesce(json_extract(c.value, '$.storage_key'), ''),
			coalesce(json_extract(b.data, '$.size_bytes'), 0)
		FROM backups b, json_each(b.data, '$.copies') c
		WHERE json_extract(c.value, '$.status') = 'done' ORDER BY b.started_at, b.id`)
	if err != nil {
		return fmt.Errorf("store: aggregate copy sizes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := map[usageKey]bool{}
	for rows.Next() {
		var target, job, db, key string
		var retained, size int64
		if err = rows.Scan(&target, &job, &db, &retained, &key, &size); err != nil {
			return fmt.Errorf("store: scan copy size: %w", err)
		}
		k := usageKey{target, key}
		if key == "" || seen[k] {
			continue
		}
		seen[k] = true
		kind := cost.KindCopy
		if retained != 0 {
			kind = cost.KindRetained
		}
		acc.add(cost.Row{TargetID: target, Kind: kind, JobID: job, Database: db}, 1, max(size, 0))
	}
	return rows.Err()
}

func (s *SQLiteStore) chunkUsage(ctx context.Context, acc *usageAcc) error {
	for _, q := range []struct {
		name  string
		query string
		kind  cost.Kind
	}{
		{"chunk", `SELECT target_id, stream_id, count(*), coalesce(sum(size_bytes), 0) FROM oplog_chunks
			WHERE status IN ('committed', 'superseded') GROUP BY target_id, stream_id`, cost.KindPITR},
		{"chunk copy", `SELECT cc.target_id, cc.stream_id, count(*), coalesce(sum(c.size_bytes), 0) FROM oplog_chunk_copies cc
			JOIN oplog_chunks c ON c.id = cc.chunk_id WHERE cc.status = 'done' GROUP BY cc.target_id, cc.stream_id`, cost.KindCopy},
	} {
		if err := s.scanChunkUsage(ctx, acc, q.name, q.query, q.kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) scanChunkUsage(ctx context.Context, acc *usageAcc, name, query string, kind cost.Kind) error {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("store: aggregate %s sizes: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var target, stream string
		var n, size int64
		if err = rows.Scan(&target, &stream, &n, &size); err != nil {
			return fmt.Errorf("store: scan %s size: %w", name, err)
		}
		acc.add(cost.Row{TargetID: target, Kind: kind, StreamID: stream}, n, size)
	}
	return rows.Err()
}

// ReplaceUsageDay replaces the snapshot of day (a "2006-01-02" UTC day) with rows
// in one transaction (cost.Repository).
func (s *SQLiteStore) ReplaceUsageDay(ctx context.Context, day string, rows []cost.Row) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM storage_usage_daily WHERE day = ?", day); err != nil {
			return fmt.Errorf("store: clear the usage snapshot of %s: %w", day, err)
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO storage_usage_daily (day, target_id, kind, job_id, database_name, stream_id, objects, bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (day, target_id, kind, job_id, database_name, stream_id)
			DO UPDATE SET objects = objects + excluded.objects, bytes = bytes + excluded.bytes`)
		if err != nil {
			return fmt.Errorf("store: prepare the usage snapshot: %w", err)
		}
		defer func() { _ = stmt.Close() }()
		for _, r := range rows {
			if _, err = stmt.ExecContext(ctx, day, r.TargetID, string(r.Kind), r.JobID, r.Database, r.StreamID, r.Objects, r.Bytes); err != nil {
				return fmt.Errorf("store: write the usage snapshot of %s: %w", day, err)
			}
		}
		return nil
	})
}

// UsageHistory returns the snapshot rows of the days from through to, inclusive,
// oldest first (cost.Repository).
func (s *SQLiteStore) UsageHistory(ctx context.Context, from, to string) ([]cost.DayRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT day, target_id, kind, job_id, database_name, stream_id, objects, bytes
		FROM storage_usage_daily WHERE day >= ? AND day <= ? ORDER BY day, target_id, kind, job_id, database_name, stream_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: read the usage history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []cost.DayRow
	for rows.Next() {
		var r cost.DayRow
		var kind string
		if err = rows.Scan(&r.Day, &r.TargetID, &kind, &r.JobID, &r.Database, &r.StreamID, &r.Objects, &r.Bytes); err != nil {
			return nil, fmt.Errorf("store: scan the usage history: %w", err)
		}
		r.Kind = cost.Kind(kind)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneUsage deletes the snapshots of the days before the given day (cost.Repository).
func (s *SQLiteStore) PruneUsage(ctx context.Context, before string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM storage_usage_daily WHERE day < ?", before)
	if err != nil {
		return 0, fmt.Errorf("store: prune the usage history: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected: %w", err)
	}
	return n, nil
}

// EgressReads returns the reads from storage targets at or after since
// (cost.Repository): the last verification of each archive and copy, the read of a
// primary archive for each completed copy, and the restore tests that read an
// archive to its end. A time is compared in Go after a coarse date filter in SQL,
// so zone offsets in stored times do not matter.
func (s *SQLiteStore) EgressReads(ctx context.Context, since time.Time) ([]cost.Read, error) {
	day := since.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	var out []cost.Read
	collect := func(name, query string, args []any, source cost.EgressSource) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("store: read %s events: %w", name, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var target, at string
			var size int64
			if err = rows.Scan(&target, &size, &at); err != nil {
				return fmt.Errorf("store: scan %s event: %w", name, err)
			}
			when, perr := time.Parse(time.RFC3339Nano, at)
			if perr != nil || when.Before(since) || size <= 0 {
				continue
			}
			out = append(out, cost.Read{TargetID: target, Source: source, Bytes: size, At: when.UTC()})
		}
		return rows.Err()
	}
	if err := collect("verification", `SELECT coalesce(json_extract(data, '$.storage_target_id'), ''), coalesce(json_extract(data, '$.size_bytes'), 0),
			json_extract(data, '$.verified_at') FROM backups
		WHERE json_extract(data, '$.verified_at') IS NOT NULL AND substr(json_extract(data, '$.verified_at'), 1, 10) >= ?`,
		[]any{day}, cost.EgressVerification); err != nil {
		return nil, err
	}
	if err := collect("copy verification", `SELECT coalesce(json_extract(c.value, '$.target_id'), ''), coalesce(json_extract(b.data, '$.size_bytes'), 0),
			json_extract(c.value, '$.verified_at') FROM backups b, json_each(b.data, '$.copies') c
		WHERE json_extract(c.value, '$.status') = 'done' AND json_extract(c.value, '$.verified_at') IS NOT NULL
			AND substr(json_extract(c.value, '$.verified_at'), 1, 10) >= ?`,
		[]any{day}, cost.EgressVerification); err != nil {
		return nil, err
	}
	if err := collect("copy", `SELECT coalesce(json_extract(b.data, '$.storage_target_id'), ''), coalesce(json_extract(b.data, '$.size_bytes'), 0),
			json_extract(c.value, '$.copied_at') FROM backups b, json_each(b.data, '$.copies') c
		WHERE json_extract(c.value, '$.status') = 'done' AND json_extract(c.value, '$.copied_at') IS NOT NULL
			AND substr(json_extract(c.value, '$.copied_at'), 1, 10) >= ?`,
		[]any{day}, cost.EgressCopy); err != nil {
		return nil, err
	}
	// A restore test reads the drill's copy target when it names one, else the
	// primary. Failed tests may not have read anything and are left out.
	if err := collect("restore test", `SELECT coalesce(nullif(json_extract(rt.data, '$.source_target_id'), ''), json_extract(b.data, '$.storage_target_id'), ''),
			coalesce(json_extract(b.data, '$.size_bytes'), 0), json_extract(rt.data, '$.started_at')
		FROM restore_tests rt JOIN backups b ON b.id = rt.backup_id
		WHERE rt.status IN (?, ?) AND rt.started_at >= ?`,
		[]any{string(models.RestoreTestOK), string(models.RestoreTestMismatch), timeKey(since)}, cost.EgressRestoreTest); err != nil {
		return nil, err
	}
	return out, nil
}
