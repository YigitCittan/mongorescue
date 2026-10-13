package ha

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// SnapshotsPrefix is the key prefix of the metadata snapshots of every cluster;
// each cluster writes below SnapshotsPrefix + cluster + "/". It does not collide
// with the per-installation self-backups of internal/metabackup, whose sub-prefixes
// are 16 hex characters.
const SnapshotsPrefix = "_mongorescue/metadata/ha/"

// Snapshot timing and retention.
const (
	// DefaultSnapshotGap is the shortest time between two snapshots that runs
	// asked for (see Snapshotter.Kick).
	DefaultSnapshotGap = 10 * time.Second
	// SnapshotsKept is how many snapshots retention keeps; it never deletes the
	// newest one.
	SnapshotsKept = 12
	// snapshotWorkFactor is the scrypt work factor of the snapshot passphrase. The
	// passphrase is a 256-bit key derived from secret.key, so a slow derivation
	// adds nothing; this keeps each snapshot's key derivation to about 1 MiB.
	snapshotWorkFactor = 10
	// snapshotPurpose derives the snapshot passphrase from secret.key.
	snapshotPurpose = "ha-metadata-snapshot"
)

// ErrSnapshotUnreadable reports a snapshot this instance cannot decrypt: its secret
// key differs from the one of the instance that wrote it, or the snapshot was
// altered.
var ErrSnapshotUnreadable = errors.New("ha: this instance cannot decrypt the cluster's metadata snapshots; give every instance of the cluster the same MONGORESCUE_SECRET_KEY")

// snapshotName is the file name of a snapshot: the UTC time to the millisecond and
// the epoch, zero-padded so names sort by time within an epoch.
var snapshotName = regexp.MustCompile(`^mongorescue-(\d{8}T\d{9})Z-e(\d{20})\.db\.age$`)

// snapshotLayout is the time layout of a snapshot name (without the Z).
const snapshotLayout = "20060102T150405.000"

// SnapshotPrefix returns the key prefix of the snapshots of cluster.
func SnapshotPrefix(cluster string) string { return SnapshotsPrefix + cluster + "/" }

// SnapshotKey returns the key of a snapshot of cluster taken at t under epoch.
func SnapshotKey(cluster string, t time.Time, epoch uint64) string {
	stamp := strings.Replace(t.UTC().Format(snapshotLayout), ".", "", 1)
	return fmt.Sprintf("%smongorescue-%sZ-e%020d.db.age", SnapshotPrefix(cluster), stamp, epoch)
}

// SnapshotInfo describes a stored snapshot. Its fields come from the object key
// and listing, which anyone with write access to the HA target can choose; only
// CheckSnapshot and FetchSnapshot authenticate them.
type SnapshotInfo struct {
	// Key is the storage key.
	Key string
	// CreatedAt is when the snapshot was taken (from its name).
	CreatedAt time.Time
	// Epoch is the lease epoch of the instance that wrote it (from its name).
	Epoch uint64
	// SizeBytes is the stored size.
	SizeBytes int64
	// ModTime and ETag identify the stored object version (from the listing).
	ModTime time.Time
	ETag    string
}

// Mark returns how new the snapshot is (from its name: authenticate it first).
func (i SnapshotInfo) Mark() SnapshotMark { return SnapshotMark{Epoch: i.Epoch, At: i.CreatedAt} }

// ParseSnapshotKey reads the time and epoch from the name of snapshot key; ok is
// false when it is not a snapshot name. The values are not authenticated.
func ParseSnapshotKey(key string) (at time.Time, epoch uint64, ok bool) { return parseSnapshotKey(key) }

// parseSnapshotKey reads the time and epoch from a snapshot key.
func parseSnapshotKey(key string) (time.Time, uint64, bool) {
	m := snapshotName.FindStringSubmatch(key[strings.LastIndex(key, "/")+1:])
	if m == nil {
		return time.Time{}, 0, false
	}
	stamp := m[1][:15] + "." + m[1][15:]
	at, err := time.ParseInLocation(snapshotLayout, stamp, time.UTC)
	if err != nil {
		return time.Time{}, 0, false
	}
	epoch, err := strconv.ParseUint(m[2], 10, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	return at, epoch, true
}

// ListSnapshots returns the snapshots of cluster on st in the order of their names:
// the highest epoch, then the newest. The names are not authenticated; decisions
// go through SelectSnapshots, which verifies each snapshot's header.
func ListSnapshots(ctx context.Context, st storage.Storage, cluster string) ([]SnapshotInfo, error) {
	objs, err := st.List(ctx, SnapshotPrefix(cluster))
	if err != nil {
		return nil, fmt.Errorf("ha: list metadata snapshots: %w", err)
	}
	var out []SnapshotInfo
	for _, o := range objs {
		if o == nil || !strings.HasPrefix(o.Key, SnapshotPrefix(cluster)) {
			continue
		}
		at, epoch, ok := parseSnapshotKey(o.Key)
		if !ok {
			continue
		}
		out = append(out, SnapshotInfo{Key: o.Key, CreatedAt: at, Epoch: epoch, SizeBytes: o.SizeBytes, ModTime: o.ModTime, ETag: o.ETag})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Epoch != out[j].Epoch {
			return out[i].Epoch > out[j].Epoch
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// SnapshotPassphrase derives the passphrase of the snapshots from secret.key: every
// instance with the same key reads them, and nothing else is needed.
func SnapshotPassphrase(secretKey []byte) (string, error) {
	sub, err := secretbox.DeriveSubkey(secretKey, snapshotPurpose)
	if err != nil {
		return "", fmt.Errorf("ha: derive the snapshot key: %w", err)
	}
	return hex.EncodeToString(sub), nil
}

// snapshotDecryptor returns the decryptor of the snapshots.
func snapshotDecryptor(passphrase string) (*encryption.Decryptor, error) {
	return encryption.NewDecryptor(encryption.DecryptorConfig{Passphrase: passphrase})
}

// snapshotDecryptError wraps a decryption failure in ErrSnapshotUnreadable.
func snapshotDecryptError(key string, err error) error {
	if errors.Is(err, encryption.ErrDecryptionFailed) {
		return fmt.Errorf("%w (snapshot %s): %w", ErrSnapshotUnreadable, key, err)
	}
	return fmt.Errorf("ha: read snapshot %s: %w", key, err)
}

// ctxReader stops a copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// VacuumStore writes a consistent copy of the metadata database (implemented by
// *store.SQLiteStore).
type VacuumStore interface {
	// VacuumInto writes a snapshot of the database to the new file path.
	VacuumInto(ctx context.Context, path string) error
}

// SnapshotterConfig holds the dependencies of a Snapshotter. Cluster, InstanceID,
// Store, Storage, Fence, Passphrase and TempDir are required.
type SnapshotterConfig struct {
	// Cluster is the cluster name.
	Cluster string
	// InstanceID identifies this instance in the snapshots' headers.
	InstanceID string
	// Store is the metadata database.
	Store VacuumStore
	// Storage is the HA target.
	Storage storage.Storage
	// Fence must pass before a snapshot is written; its epoch names the snapshot.
	Fence models.Fence
	// Passphrase encrypts the snapshots (see SnapshotPassphrase).
	Passphrase string
	// TempDir holds the temporary copy of the database (the data directory).
	TempDir string
	// Interval is the longest time between two snapshots; MinGap the shortest
	// between two snapshots that Kick asked for (default DefaultSnapshotGap).
	Interval time.Duration
	MinGap   time.Duration
	// Keep is how many snapshots retention keeps (default SnapshotsKept).
	Keep int
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Observe receives the outcome of every snapshot (metrics); may be nil.
	Observe func(ok bool)
	// OnWritten is called after every snapshot is stored, before retention: the
	// lease names it as the last snapshot (Manager.RecordSnapshot) and the
	// instance's high-water mark rises. May be nil.
	OnWritten func(ctx context.Context, info SnapshotInfo)
	// Checker checks the snapshots for retention; nil means a checker of its own
	// that logs refused snapshots and calls OnReject.
	Checker *SnapshotChecker
	// OnReject is called once per stored object for every snapshot the checker of
	// its own finds refused (see SnapshotCheck.Refused), after it was logged
	// (metrics); may be nil. It is not used with Checker.
	OnReject func(SnapshotCheck)
}

// Snapshotter writes the metadata snapshots of the active instance: at least every
// Interval, and after runs (Kick), at most one per MinGap. It is safe for concurrent
// use.
type Snapshotter struct {
	cfg  SnapshotterConfig
	kick chan struct{}
	// runMu makes snapshots single-flight.
	runMu sync.Mutex
}

// NewSnapshotter returns a Snapshotter.
func NewSnapshotter(cfg SnapshotterConfig) (*Snapshotter, error) {
	if cfg.Store == nil || cfg.Storage == nil || cfg.Fence == nil || cfg.Passphrase == "" || cfg.TempDir == "" || cfg.InstanceID == "" {
		return nil, fmt.Errorf("%w: a snapshotter needs an instance ID, a store, a storage, a fence, a passphrase and a directory", ErrInvalidConfig)
	}
	if err := ValidateCluster(cfg.Cluster); err != nil {
		return nil, err
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.MinGap <= 0 {
		cfg.MinGap = DefaultSnapshotGap
	}
	if cfg.Keep <= 0 {
		cfg.Keep = SnapshotsKept
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Checker == nil {
		logger, onReject := cfg.Logger, cfg.OnReject
		cfg.Checker = NewSnapshotChecker(cfg.Storage, cfg.Cluster, cfg.Passphrase, func(c SnapshotCheck) {
			logger.Error("HA snapshot refused: its authenticated header does not match its name or cluster, or it does not decrypt; someone with write access to the HA target may have copied or altered it. It is kept for investigation and never restored",
				logsafe.Attr("key", c.Key), slog.String("error", redact.Text(c.Err.Error())))
			if onReject != nil {
				onReject(c)
			}
		})
	}
	return &Snapshotter{cfg: cfg, kick: make(chan struct{}, 1)}, nil
}

// Kick asks for a snapshot soon (after a finished run); it never blocks.
func (s *Snapshotter) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run writes a snapshot at once, so a new active instance's epoch has one right
// away, then every Interval and after every Kick (at most one per MinGap), until
// ctx ends.
func (s *Snapshotter) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-s.kick:
			// Coalesce the kicks of runs that finish together.
			wait := time.NewTimer(s.cfg.MinGap)
			select {
			case <-ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
		}
		if _, err := s.Snapshot(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("HA metadata snapshot failed; the next one is retried", slog.String("error", redact.Text(err.Error())))
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.cfg.Interval)
	}
}

// Snapshot writes one snapshot now and applies retention. It refuses with the
// fence's error when this instance does not hold the lease.
func (s *Snapshotter) Snapshot(ctx context.Context) (info SnapshotInfo, err error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	defer func() {
		if s.cfg.Observe != nil {
			s.cfg.Observe(err == nil)
		}
	}()
	if err = s.cfg.Fence.Check(); err != nil {
		return SnapshotInfo{}, err
	}
	epoch := s.cfg.Fence.Epoch()
	// The key names the time to the millisecond; the header authenticates the same.
	now := s.cfg.Now().UTC().Truncate(time.Millisecond)
	header, err := SnapshotHeader{Format: SnapshotFormat, Cluster: s.cfg.Cluster, Epoch: epoch, InstanceID: s.cfg.InstanceID,
		CreatedAt: now}.encode()
	if err != nil {
		return SnapshotInfo{}, err
	}
	dir, err := os.MkdirTemp(s.cfg.TempDir, ".ha-snapshot-*")
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("ha: snapshot directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "mongorescue.db")
	if err = s.cfg.Store.VacuumInto(ctx, path); err != nil {
		return SnapshotInfo{}, fmt.Errorf("ha: copy the metadata database: %w", err)
	}
	f, err := os.Open(path) //nolint:gosec // G304: a file this function created.
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("ha: open the snapshot copy: %w", err)
	}
	defer func() { _ = f.Close() }()
	enc, err := encryption.NewScryptEncryptor(s.cfg.Passphrase, snapshotWorkFactor)
	if err != nil {
		return SnapshotInfo{}, err
	}
	// The fence is checked again right before the upload: a snapshot of an
	// instance that lost the lease meanwhile must not be written.
	if err = s.cfg.Fence.Check(); err != nil {
		return SnapshotInfo{}, err
	}
	key := SnapshotKey(s.cfg.Cluster, now, epoch)
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() {
		w, encErr := enc.Encrypt(pw)
		if encErr == nil {
			// The header goes first into the encrypted stream, so age authenticates
			// it with the database.
			if _, encErr = w.Write(header); encErr == nil {
				_, encErr = io.Copy(w, f)
			}
			if closeErr := w.Close(); encErr == nil {
				encErr = closeErr
			}
		}
		_ = pw.CloseWithError(encErr)
	})
	obj, err := s.cfg.Storage.Save(ctx, key, pr)
	_ = pr.CloseWithError(errors.New("ha: snapshot upload ended"))
	wg.Wait()
	if err != nil {
		return SnapshotInfo{}, fmt.Errorf("ha: upload the metadata snapshot: %w", err)
	}
	info = SnapshotInfo{Key: key, CreatedAt: now, Epoch: epoch}
	if obj != nil {
		info.SizeBytes = obj.SizeBytes
	}
	s.cfg.Logger.Debug("HA metadata snapshot written", logsafe.Attr("key", key), slog.Uint64("epoch", epoch))
	if s.cfg.OnWritten != nil {
		s.cfg.OnWritten(ctx, info)
	}
	s.prune(ctx, epoch)
	return info, nil
}

// prune deletes the snapshots retention does not keep (see keptSnapshots) for the
// current epoch. It reads the whole listing and decides on verified snapshots
// only: a name nobody authenticated never displaces one. Refused snapshots are
// kept for investigation (the checker reports them); legacy snapshots (no header)
// are deleted once a verified one exists, since they are never restored then. A
// snapshot whose check failed (a storage error, a newer format) may be the newest:
// nothing that sorts before it is deleted, while older snapshots are pruned as
// usual. Failures are logged.
func (s *Snapshotter) prune(ctx context.Context, current uint64) {
	sel, err := s.cfg.Checker.Select(ctx, 0)
	if err != nil {
		s.cfg.Logger.Warn("HA snapshot retention: cannot list the snapshots", slog.String("error", redact.Text(err.Error())))
		return
	}
	// Snapshots sorting before a failed one are not deleted.
	lastFailed := -1
	for _, c := range sel.Failed {
		lastFailed = max(lastFailed, c.index)
	}
	verified := make([]SnapshotInfo, 0, len(sel.Verified))
	for _, c := range sel.Verified {
		verified = append(verified, c.SnapshotInfo)
	}
	kept := keptSnapshots(verified, current, max(s.cfg.Keep, 1))
	var drop []SnapshotCheck
	for _, c := range sel.Verified {
		if !kept[c.Key] {
			drop = append(drop, c)
		}
	}
	if len(verified) > 0 {
		drop = append(drop, sel.Legacy...)
	}
	for _, old := range drop {
		if old.index < lastFailed {
			continue
		}
		if delErr := s.cfg.Storage.Delete(ctx, old.Key); delErr != nil && !errors.Is(delErr, storage.ErrNotFound) {
			s.cfg.Logger.Warn("HA snapshot retention: cannot delete a snapshot", logsafe.Attr("key", old.Key),
				slog.String("error", redact.Text(delErr.Error())))
		}
	}
}

// keptSnapshots returns the keys retention keeps of list (ordered as ListSnapshots
// orders it): the first keep, the keep newest of the current epoch, and the newest
// by its time whatever its epoch. Snapshots of the current epoch beyond the keep
// newest go, so the prefix does not grow without end within one long epoch.
func keptSnapshots(list []SnapshotInfo, current uint64, keep int) map[string]bool {
	kept := make(map[string]bool, 2*keep+1)
	var newest SnapshotInfo
	inEpoch := 0
	for i, sn := range list {
		if i < keep {
			kept[sn.Key] = true
		}
		if sn.Epoch == current && inEpoch < keep {
			kept[sn.Key] = true
			inEpoch++
		}
		if newest.Key == "" || sn.CreatedAt.After(newest.CreatedAt) {
			newest = sn
		}
	}
	if newest.Key != "" {
		kept[newest.Key] = true
	}
	return kept
}
