package ha

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// StateFileName is the file in the data directory that holds the instance's HA
// state (see State).
const StateFileName = "ha.json"

// stateFileMode restricts the state file to the owner.
const stateFileMode = 0o600

// State is the HA state an instance keeps in its data directory.
type State struct {
	// InstanceID identifies the instance in the lease; generated once.
	InstanceID string `json:"instance_id"`
	// Cluster is the cluster of LastActiveEpoch.
	Cluster string `json:"cluster,omitempty"`
	// LastActiveEpoch is the last epoch under which this instance was active: its
	// metadata database is at least as new as any snapshot of that epoch.
	LastActiveEpoch uint64 `json:"last_active_epoch,omitempty"`
	// UpdatedAt is when the state was last written.
	UpdatedAt time.Time `json:"updated_at"`
}

// LoadState reads the state from dataDir, creating it with a new instance ID when it
// does not exist.
func LoadState(dataDir string) (State, error) {
	path := filepath.Join(dataDir, StateFileName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: a fixed name in the data directory.
	if errors.Is(err, fs.ErrNotExist) {
		id := make([]byte, 8)
		if _, err = rand.Read(id); err != nil {
			return State{}, fmt.Errorf("ha: generate instance ID: %w", err)
		}
		st := State{InstanceID: hex.EncodeToString(id)}
		return st, SaveState(dataDir, st)
	}
	if err != nil {
		return State{}, fmt.Errorf("ha: read %s: %w", StateFileName, err)
	}
	var st State
	if err = json.Unmarshal(data, &st); err != nil || st.InstanceID == "" {
		return State{}, fmt.Errorf("ha: %s is not valid; remove it to generate a new instance ID", path)
	}
	return st, nil
}

// SaveState writes st to dataDir atomically.
func SaveState(dataDir string, st State) error {
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("ha: encode state: %w", err)
	}
	path := filepath.Join(dataDir, StateFileName)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, stateFileMode); err != nil {
		return fmt.Errorf("ha: write %s: %w", StateFileName, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ha: write %s: %w", StateFileName, err)
	}
	return nil
}
