package keywrap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// JournalFileName is the journal of key wrapping changes in the data directory:
// one JSON Change per line, written by Load and the offline commands and
// published (then removed) by the next start, which has the event bus.
const JournalFileName = "key-wrap-journal.jsonl"

// Actions of a Change.
const (
	// ActionWrapped is a plaintext secret.key that was wrapped at startup.
	ActionWrapped = "wrapped"
	// ActionRewrapped is a wrapped key moved to another KMS key.
	ActionRewrapped = "rewrapped"
	// ActionUnwrappedToFile is a wrapped key written back as a plaintext file.
	ActionUnwrappedToFile = "unwrapped_to_file"
)

// ActorSystem is the actor of a change MongoRescue made by itself.
const ActorSystem = "system"

// maxJournalLine bounds one journal line.
const maxJournalLine = 16 << 10

// Change is one change of the key wrapping. It carries no key material.
type Change struct {
	// Action is one of the Action constants.
	Action string `json:"action"`
	// At is when it happened (UTC).
	At time.Time `json:"at"`
	// Actor is who made it: ActorSystem, or "local:<OS user>" for a command.
	Actor string `json:"actor"`
	// Provider, KeyURI and KeyID name the KMS key after the change ("" after
	// ActionUnwrappedToFile).
	Provider string `json:"provider,omitempty"`
	KeyURI   string `json:"key_uri,omitempty"`
	KeyID    string `json:"key_id,omitempty"`
	// PreviousProvider and PreviousKeyURI name the KMS key before a rewrap or an
	// unwrap.
	PreviousProvider string `json:"previous_provider,omitempty"`
	PreviousKeyURI   string `json:"previous_key_uri,omitempty"`
	// Fingerprint is secretbox.Fingerprint of secret.key.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Files names the key files that changed.
	Files []string `json:"files,omitempty"`
}

// Detail returns a one-line description of c for events and audit entries.
func (c Change) Detail() string {
	to := Spec{Provider: c.Provider, KeyURI: c.KeyURI}
	from := Spec{Provider: c.PreviousProvider, KeyURI: c.PreviousKeyURI}
	var s string
	switch c.Action {
	case ActionWrapped:
		s = "secret.key is now wrapped with " + to.String() + "; the plaintext key file was removed"
	case ActionRewrapped:
		s = "secret.key was rewrapped from " + from.String() + " to " + to.String()
	case ActionUnwrappedToFile:
		s = "secret.key was unwrapped from " + from.String() + " and written back as a plaintext file; it is no longer protected by the KMS"
	default:
		s = c.Action
	}
	if c.Fingerprint != "" {
		s += " (key fingerprint " + c.Fingerprint + ")"
	}
	return s
}

// AppendJournal appends c to the journal of dataDir and syncs it.
func AppendJournal(dataDir string, c Change) (err error) {
	line, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("keywrap: encode journal entry: %w", err)
	}
	path := filepath.Join(dataDir, JournalFileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: a file in the configured data directory.
	if err != nil {
		return fmt.Errorf("keywrap: open journal: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("keywrap: close journal: %w", cerr)
		}
	}()
	if _, err = f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("keywrap: write journal: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("keywrap: sync journal: %w", err)
	}
	return nil
}

// ReadJournal returns the journaled changes of dataDir, oldest first (none when
// there is no journal). Lines that do not parse (a torn write) are skipped.
func ReadJournal(dataDir string) ([]Change, error) {
	f, err := os.Open(filepath.Join(dataDir, JournalFileName)) //nolint:gosec // G304: a file in the configured data directory.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("keywrap: open journal: %w", err)
	}
	defer func() { _ = f.Close() }()
	var out []Change
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxJournalLine)
	for sc.Scan() {
		var c Change
		if json.Unmarshal(sc.Bytes(), &c) == nil && c.Action != "" {
			out = append(out, c)
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("keywrap: read journal: %w", err)
	}
	return out, nil
}

// ClearJournal removes the journal of dataDir.
func ClearJournal(dataDir string) error {
	return secretbox.RemoveKeyFile(filepath.Join(dataDir, JournalFileName))
}
