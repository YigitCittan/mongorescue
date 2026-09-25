package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

const (
	upsertChannelSQL = `INSERT INTO notification_channels (id, name, type, enabled, data)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, type = excluded.type,
			enabled = excluded.enabled, data = excluded.data`

	upsertRuleSQL = `INSERT INTO notification_rules (id, name, enabled, data)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET name = excluded.name, enabled = excluded.enabled, data = excluded.data`

	// rulesReferencingChannelSQL selects the rules whose channel_ids contain a channel.
	rulesReferencingChannelSQL = `SELECT data FROM notification_rules
		WHERE EXISTS (SELECT 1 FROM json_each(notification_rules.data, '$.channel_ids') AS c WHERE c.value = ?)`
)

// ListChannels returns all notification channels sorted by name. Channel secrets are
// returned as stored; masking is the caller's responsibility.
func (s *SQLiteStore) ListChannels(ctx context.Context) ([]*notify.Channel, error) {
	list, err := listRecords[notify.Channel](ctx, s.db, "SELECT data FROM notification_channels ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	for i, ch := range list {
		if list[i], err = s.openChannel(ch); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// GetChannel returns a notification channel or notify.ErrChannelNotFound.
func (s *SQLiteStore) GetChannel(ctx context.Context, id string) (*notify.Channel, error) {
	ch, err := getRecord[notify.Channel](ctx, s.db, notify.ErrChannelNotFound,
		"SELECT data FROM notification_channels WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return s.openChannel(ch)
}

// SaveChannel creates or replaces a notification channel.
func (s *SQLiteStore) SaveChannel(ctx context.Context, ch *notify.Channel) error {
	if ch == nil || ch.ID == "" {
		return fmt.Errorf("%w: channel with ID is required", ErrInvalidRecord)
	}
	return s.putChannel(ctx, s.db, ch)
}

// DeleteChannel removes a channel and its ID from every rule referencing it, in one
// transaction. It returns notify.ErrChannelNotFound when the channel does not exist.
func (s *SQLiteStore) DeleteChannel(ctx context.Context, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := execOne(ctx, tx, notify.ErrChannelNotFound,
			"DELETE FROM notification_channels WHERE id = ?", id); err != nil {
			return err
		}
		// Load every affected rule before writing, so no cursor is open during updates.
		rules, err := listRecords[notify.Rule](ctx, tx, rulesReferencingChannelSQL, id)
		if err != nil {
			return err
		}
		for _, r := range rules {
			r.ChannelIDs = slices.DeleteFunc(r.ChannelIDs, func(c string) bool { return c == id })
			if err := putRule(ctx, tx, r); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveDeliveryStatus records the last delivery outcome of an existing channel, or
// returns notify.ErrChannelNotFound when it was deleted meanwhile. The update is a
// single statement, so it cannot overwrite a concurrent SaveChannel with stale data.
func (s *SQLiteStore) SaveDeliveryStatus(ctx context.Context, id string, status notify.DeliveryStatus) error {
	data, err := encode(status)
	if err != nil {
		return err
	}
	return execOne(ctx, s.db, notify.ErrChannelNotFound,
		"UPDATE notification_channels SET data = json_set(data, '$.last_delivery', json(?)) WHERE id = ?", data, id)
}

// ListRules returns all notification rules sorted by name.
func (s *SQLiteStore) ListRules(ctx context.Context) ([]*notify.Rule, error) {
	return listRecords[notify.Rule](ctx, s.db, "SELECT data FROM notification_rules ORDER BY name, id")
}

// GetRule returns a notification rule or notify.ErrRuleNotFound.
func (s *SQLiteStore) GetRule(ctx context.Context, id string) (*notify.Rule, error) {
	return getRecord[notify.Rule](ctx, s.db, notify.ErrRuleNotFound,
		"SELECT data FROM notification_rules WHERE id = ?", id)
}

// SaveRule creates or replaces a notification rule.
func (s *SQLiteStore) SaveRule(ctx context.Context, r *notify.Rule) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("%w: rule with ID is required", ErrInvalidRecord)
	}
	return putRule(ctx, s.db, r)
}

// DeleteRule removes a notification rule or returns notify.ErrRuleNotFound.
func (s *SQLiteStore) DeleteRule(ctx context.Context, id string) error {
	return execOne(ctx, s.db, notify.ErrRuleNotFound, "DELETE FROM notification_rules WHERE id = ?", id)
}

// putChannel encrypts the secrets of ch and upserts it with its indexed columns.
func (s *SQLiteStore) putChannel(ctx context.Context, e execer, ch *notify.Channel) error {
	if s.box == nil {
		return ErrNoSecretBox
	}
	// Every value is sealed, even one that looks sealed already: user input such as
	// "sb1:..." must never be stored as is.
	sealed, err := ch.TransformSecrets(func(field, v string) (string, error) {
		return s.seal(secretbox.At(tableChannels, ch.ID, field), v)
	})
	if err != nil {
		return fmt.Errorf("store: encrypt channel %s: %w", ch.ID, err)
	}
	return writeChannel(ctx, e, sealed)
}

// openChannel decrypts the secrets of a stored channel.
func (s *SQLiteStore) openChannel(ch *notify.Channel) (*notify.Channel, error) {
	if s.box == nil {
		return nil, ErrNoSecretBox
	}
	out, err := ch.TransformSecrets(func(field, v string) (string, error) {
		return s.open(secretbox.At(tableChannels, ch.ID, field), v)
	})
	if err != nil {
		return nil, fmt.Errorf("store: decrypt channel %s: %w", ch.ID, err)
	}
	return out, nil
}

// writeChannel upserts ch (whose secrets are already encrypted) and its indexed columns.
func writeChannel(ctx context.Context, e execer, ch *notify.Channel) error {
	data, err := encode(ch)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertChannelSQL,
		ch.ID, ch.Name, string(ch.Type), boolInt(ch.Enabled), data); err != nil {
		return fmt.Errorf("store: save notification channel %s: %w", ch.ID, err)
	}
	return nil
}

// putRule upserts r and its indexed columns.
func putRule(ctx context.Context, e execer, r *notify.Rule) error {
	data, err := encode(r)
	if err != nil {
		return err
	}
	if _, err := e.ExecContext(ctx, upsertRuleSQL, r.ID, r.Name, boolInt(r.Enabled), data); err != nil {
		return fmt.Errorf("store: save notification rule %s: %w", r.ID, err)
	}
	return nil
}
