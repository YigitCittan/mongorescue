package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/notify"
)

var _ notify.Outbox = (*SQLiteStore)(nil)

const (
	insertOutboxSQL = `INSERT INTO notification_outbox (channel_id, channel_type, event, attempts, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`

	// trimOutboxSQL deletes the oldest rows beyond the cap.
	trimOutboxSQL = `DELETE FROM notification_outbox WHERE id IN (SELECT id FROM notification_outbox ORDER BY id LIMIT ?)
		RETURNING id, channel_id, channel_type, event, attempts, next_attempt_at, created_at`

	// outboxHeadsSQL selects the oldest row of every channel.
	outboxHeadsSQL = `SELECT o.id, o.channel_id, o.channel_type, o.event, o.attempts, o.next_attempt_at, o.created_at
		FROM notification_outbox AS o
		WHERE o.id = (SELECT MIN(i.id) FROM notification_outbox AS i WHERE i.channel_id = o.channel_id)
		ORDER BY o.id`
)

// EnqueueDeliveries stores entries in one transaction, then drops the oldest
// entries beyond limit (no cap when limit <= 0) and returns them (see
// notify.Outbox). The event is stored as JSON; channel configuration is not.
func (s *SQLiteStore) EnqueueDeliveries(ctx context.Context, entries []notify.OutboxEntry, limit int) ([]notify.OutboxEntry, error) {
	var dropped []notify.OutboxEntry
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		dropped = nil
		for _, e := range entries {
			data, err := encode(e.Event)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, insertOutboxSQL, e.ChannelID, string(e.ChannelType), data, e.Attempts,
				timeKey(e.NextAttemptAt), timeKey(e.CreatedAt)); err != nil {
				return fmt.Errorf("store: queue notification for channel %s: %w", e.ChannelID, err)
			}
		}
		if limit <= 0 {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM notification_outbox").Scan(&n); err != nil {
			return fmt.Errorf("store: count queued notifications: %w", err)
		}
		if n <= limit {
			return nil
		}
		rows, err := tx.QueryContext(ctx, trimOutboxSQL, n-limit)
		if err != nil {
			return fmt.Errorf("store: drop the oldest queued notifications: %w", err)
		}
		dropped, err = scanOutbox(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return dropped, nil
}

// OutboxHeads returns the oldest queued delivery of every channel, oldest first.
func (s *SQLiteStore) OutboxHeads(ctx context.Context) ([]notify.OutboxEntry, error) {
	rows, err := s.db.QueryContext(ctx, outboxHeadsSQL)
	if err != nil {
		return nil, fmt.Errorf("store: list queued notifications: %w", err)
	}
	return scanOutbox(rows)
}

// DeleteDelivery removes the queued delivery id; a missing one is not an error.
func (s *SQLiteStore) DeleteDelivery(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM notification_outbox WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: remove queued notification %d: %w", id, err)
	}
	return nil
}

// RetryDelivery records a failed round of the queued delivery id.
func (s *SQLiteStore) RetryDelivery(ctx context.Context, id int64, attempts int, next time.Time, lastError string) error {
	if _, err := s.db.ExecContext(ctx,
		"UPDATE notification_outbox SET attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?",
		attempts, timeKey(next), lastError, id); err != nil {
		return fmt.Errorf("store: reschedule queued notification %d: %w", id, err)
	}
	return nil
}

// scanOutbox reads and closes rows of (id, channel_id, channel_type, event,
// attempts, next_attempt_at, created_at). A row whose event cannot be decoded
// keeps its IDs and an empty event type, so the dispatcher removes it.
func scanOutbox(rows *sql.Rows) ([]notify.OutboxEntry, error) {
	defer func() { _ = rows.Close() }()
	var out []notify.OutboxEntry
	for rows.Next() {
		var (
			e             notify.OutboxEntry
			channelType   string
			data          string
			next, created int64
		)
		if err := rows.Scan(&e.ID, &e.ChannelID, &channelType, &data, &e.Attempts, &next, &created); err != nil {
			return nil, fmt.Errorf("store: read queued notification: %w", err)
		}
		e.ChannelType = notify.ChannelType(channelType)
		e.NextAttemptAt, e.CreatedAt = time.Unix(0, next).UTC(), time.Unix(0, created).UTC()
		if ev, err := decode[events.Event](data); err == nil {
			e.Event = *ev
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read queued notifications: %w", err)
	}
	return out, nil
}
