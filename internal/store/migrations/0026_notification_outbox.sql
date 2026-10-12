-- 0026_notification_outbox: the durable queue of notification deliveries (see
-- docs/notifications.md, "Delivery").
--
-- One row per delivery of an event to a channel, stored before it is sent and
-- deleted once it was delivered or given up, so a delivery interrupted by a crash
-- is sent again at the next start. id (AUTOINCREMENT, never reused) orders the
-- rows; the oldest row of a channel is sent first and alone. event is the
-- events.Event as JSON (redacted like every event; never a rendered message or a
-- channel secret). attempts counts the failed rounds, next_attempt_at (Unix
-- nanoseconds, UTC) is when the row is due, last_error the redacted error of the
-- last round. The queue is capped by the application, which drops the oldest rows.
--
-- No existing row changes, so nothing is backfilled.

CREATE TABLE notification_outbox (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    channel_id      TEXT    NOT NULL,
    channel_type    TEXT    NOT NULL,
    event           TEXT    NOT NULL CHECK (json_valid(event)),
    attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    last_error      TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX notification_outbox_channel ON notification_outbox (channel_id, id);
