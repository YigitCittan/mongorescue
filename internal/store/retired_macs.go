package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// retiredMAC is a retired imported-key MAC key with its ID (auth.ImportedKeyMACID).
type retiredMAC struct {
	KID string `json:"kid"`
	Key []byte `json:"key"`
}

// retiredMACsAt is the location of the sealed retired MAC keys.
var retiredMACsAt = secretbox.At(tableSettings, retiredMACKeysSetting, fieldSettingValue)

// readRetiredMACs returns the retired imported-key MAC keys, opened with box.
func readRetiredMACs(ctx context.Context, q queryer, box *secretbox.Box) ([]retiredMAC, error) {
	var sealed string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", retiredMACKeysSetting).Scan(&sealed)
	switch {
	case errors.Is(err, sql.ErrNoRows) || err == nil && sealed == "":
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read retired api key MAC keys: %w", err)
	}
	if !secretbox.IsSealed(sealed) {
		return nil, fmt.Errorf("%w: %s", ErrUnsealedSecret, retiredMACsAt)
	}
	plain, err := box.Open(retiredMACsAt, sealed)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", retiredMACsAt, err)
	}
	var keys []retiredMAC
	if err := json.Unmarshal([]byte(plain), &keys); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCorruptRecord, retiredMACsAt, err)
	}
	return keys, nil
}

// referencedKIDs returns the MAC key IDs the stored imported-key hashes name, and
// whether a hash without a key ID (which any key may have made) exists.
func referencedKIDs(ctx context.Context, q queryer) (kids []string, unnamed bool, err error) {
	rows, err := q.QueryContext(ctx, "SELECT key_hash FROM api_keys WHERE substr(key_hash, 1, ?) = ?",
		len(importedKeyHashPrefix), importedKeyHashPrefix)
	if err != nil {
		return nil, false, fmt.Errorf("read imported api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, false, err
		}
		switch kid, _ := auth.ImportedKeyHashKID(h); kid {
		case "":
			unnamed = true
		default:
			kids = append(kids, kid)
		}
	}
	return kids, unnamed, rows.Err()
}

// writeRetiredMACs keeps the retired MAC keys that a stored hash still needs (all
// of them while a hash without a key ID exists), sealed with box, and drops the row
// when none is needed. It reports how many were kept.
func writeRetiredMACs(ctx context.Context, tx *sql.Tx, box *secretbox.Box, keys []retiredMAC) (int, error) {
	kids, unnamed, err := referencedKIDs(ctx, tx)
	if err != nil {
		return 0, err
	}
	if !unnamed {
		keys = slices.DeleteFunc(keys, func(k retiredMAC) bool { return !slices.Contains(kids, k.KID) })
	}
	if len(keys) > maxRetiredMACKeys {
		keys = keys[:maxRetiredMACKeys]
	}
	if len(keys) == 0 {
		if _, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", retiredMACKeysSetting); err != nil {
			return 0, fmt.Errorf("drop retired api key MAC keys: %w", err)
		}
		return 0, nil
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return 0, err
	}
	sealed, err := box.Seal(retiredMACsAt, string(raw))
	if err != nil {
		return 0, fmt.Errorf("encrypt %s: %w", retiredMACsAt, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, retiredMACKeysSetting, sealed); err != nil {
		return 0, fmt.Errorf("store retired api key MAC keys: %w", err)
	}
	return len(keys), nil
}

// retireImportedKeyMAC adds mac (the imported-key MAC key of the old secret key) to
// the retired MAC keys, sealed under the next key, keeping only the keys a stored
// hash still names. The list was re-sealed under the next key already.
func (r resealer) retireImportedKeyMAC(ctx context.Context, tx *sql.Tx, mac []byte, res *KeyRotationResult) error {
	keys, err := readRetiredMACs(ctx, tx, r.next)
	if err != nil {
		return err
	}
	if len(mac) > 0 {
		kid := auth.ImportedKeyMACID(mac)
		keys = slices.DeleteFunc(keys, func(k retiredMAC) bool { return k.KID == kid })
		keys = append([]retiredMAC{{KID: kid, Key: mac}}, keys...)
	}
	n, err := writeRetiredMACs(ctx, tx, r.next, keys)
	if n > 0 {
		res.Resealed++
	}
	return err
}

// RetiredImportedKeyMACs returns the keys that MACed API keys imported from
// MONGORESCUE_API_KEY under earlier secret keys, newest first (see
// SecretKeyRotation.RetiredImportedKeyMAC).
func (s *SQLiteStore) RetiredImportedKeyMACs(ctx context.Context) ([][]byte, error) {
	defer s.lockKey()()
	if s.box == nil {
		return nil, ErrNoSecretBox
	}
	keys, err := readRetiredMACs(ctx, s.db, s.box)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	out := make([][]byte, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Key)
	}
	return out, nil
}

// pruneRetiredMACs drops the retired MAC keys no stored hash names any more.
func (s *SQLiteStore) pruneRetiredMACs(ctx context.Context, tx *sql.Tx) error {
	keys, err := readRetiredMACs(ctx, tx, s.box)
	if err != nil || len(keys) == 0 {
		return err
	}
	_, err = writeRetiredMACs(ctx, tx, s.box, keys)
	return err
}
