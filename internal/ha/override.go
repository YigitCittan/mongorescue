package ha

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OverrideFileName is the file in the data directory through which an operator
// forces the next takeover of the instance (`mongorescue ha force-takeover`).
const OverrideFileName = "ha-override.json"

// ErrInvalidOverride reports a forced takeover request that cannot be used.
var ErrInvalidOverride = errors.New("ha: invalid forced takeover")

// Override is an operator's request to force the next takeover of an instance
// past the rollback checks: restore a named snapshot (its header is still
// verified when it has one), or keep the instance's own database. It is used once.
type Override struct {
	// Cluster is the cluster the request is for.
	Cluster string `json:"cluster"`
	// Snapshot is the key of the snapshot to restore.
	Snapshot string `json:"snapshot,omitempty"`
	// KeepLocal keeps the instance's own database instead.
	KeepLocal bool `json:"keep_local_database,omitempty"`
	// RequestedBy names the operator (the operating system user of the command).
	RequestedBy string `json:"requested_by"`
	// RequestedAt is when the request was made.
	RequestedAt time.Time `json:"requested_at"`
}

// Validate checks o: a valid cluster, and either a snapshot of that cluster or
// KeepLocal.
func (o Override) Validate() error {
	if err := ValidateCluster(o.Cluster); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidOverride, err)
	}
	switch {
	case o.KeepLocal && o.Snapshot != "":
		return fmt.Errorf("%w: name a snapshot or keep the local database, not both", ErrInvalidOverride)
	case o.KeepLocal:
		return nil
	case o.Snapshot == "":
		return fmt.Errorf("%w: name a snapshot or keep the local database", ErrInvalidOverride)
	}
	if _, _, ok := parseSnapshotKey(o.Snapshot); !ok || !strings.HasPrefix(o.Snapshot, SnapshotPrefix(o.Cluster)) {
		return fmt.Errorf("%w: %q is not a snapshot key of cluster %q (%smongorescue-<time>Z-e<epoch>.db.age)",
			ErrInvalidOverride, o.Snapshot, o.Cluster, SnapshotPrefix(o.Cluster))
	}
	return nil
}

// WriteOverride stores o in dataDir for the instance's next takeover.
func WriteOverride(dataDir string, o Override) error {
	if err := o.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("ha: encode the forced takeover: %w", err)
	}
	path := filepath.Join(dataDir, OverrideFileName)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, stateFileMode); err != nil {
		return fmt.Errorf("ha: write %s: %w", OverrideFileName, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ha: write %s: %w", OverrideFileName, err)
	}
	return nil
}

// TakeOverride reads and removes the forced takeover request of dataDir: nil
// when there is none. A request that cannot be read or is invalid is removed too
// and returned as an error.
func TakeOverride(dataDir string) (*Override, error) {
	path := filepath.Join(dataDir, OverrideFileName)
	data, err := os.ReadFile(path) //nolint:gosec // G304: a fixed name in the data directory.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ha: read %s: %w", OverrideFileName, err)
	}
	if err = os.Remove(path); err != nil {
		return nil, fmt.Errorf("ha: remove %s: %w", OverrideFileName, err)
	}
	var o Override
	if err = json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrInvalidOverride, OverrideFileName, err)
	}
	if err = o.Validate(); err != nil {
		return nil, err
	}
	return &o, nil
}
