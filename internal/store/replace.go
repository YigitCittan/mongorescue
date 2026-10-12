package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BackupInfix separates the database file name from the time and epoch of a copy
// that ReplaceDatabase keeps: mongorescue.db.bak-20261012T100000Z-epoch7.
const BackupInfix = ".bak-"

// DefaultBackupsKept is how many copies of replaced databases ReplaceDatabase keeps
// by default.
const DefaultBackupsKept = 3

// replaceJournalSuffix names the journal of a database replacement in progress,
// next to the database (see RecoverReplace).
const replaceJournalSuffix = ".replace.json"

// Sentinel errors of database replacement.
var (
	// ErrInvalidReplacement is returned by ReplaceDatabase when the new database
	// cannot be used: it does not open with the store's options (another secret
	// key, a newer schema) or fails its integrity check.
	ErrInvalidReplacement = errors.New("store: the replacement database cannot be used")

	// ErrInterruptedReplace is returned by RecoverReplace when a replacement was
	// interrupted in a state it cannot settle by itself: the database is missing.
	ErrInterruptedReplace = errors.New("store: a replacement of the metadata database was interrupted")
)

// Replacement describes a ReplaceDatabase call.
type Replacement struct {
	// Path is the metadata database; NewPath the database file replacing it, in
	// the same directory tree (a rename must move it).
	Path    string
	NewPath string
	// Epoch is the HA epoch of the replacement; it names the copy of the previous
	// database.
	Epoch uint64
	// Keep is how many copies of replaced databases are kept (DefaultBackupsKept
	// when 0); the oldest are removed.
	Keep int
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Now is the clock of the copy's name; nil means time.Now.
	Now func() time.Time
}

// Phases of a replacement, recorded in its journal.
const (
	phaseJournaled = "journaled" // nothing moved yet
	phaseKept      = "kept"      // the previous database has its backup copy
	phaseSwapping  = "swapping"  // the rename may have happened
	phaseSwapped   = "swapped"   // the replacement is in place
)

// replaceJournal is the journal of a replacement in progress.
type replaceJournal struct {
	NewPath string `json:"new_path"`
	// NewSHA256 identifies the replacement: the database is the replacement when
	// its content has this hash, whatever happened to NewPath.
	NewSHA256 string    `json:"new_sha256"`
	Backup    string    `json:"backup"`
	Epoch     uint64    `json:"epoch"`
	Phase     string    `json:"phase"`
	StartedAt time.Time `json:"started_at"`
}

// Test hooks of ReplaceDatabase: replaceCrash, when it returns true after a step,
// stops the replacement there as a crash would (nothing is cleaned up);
// replaceFail, when it returns an error at a step, makes the step fail like an I/O
// error would.
var (
	replaceCrash func(step string) bool
	replaceFail  func(step string) error
)

// errCrashed is returned by a replacement replaceCrash stopped.
var errCrashed = errors.New("store: replacement stopped by the test hook")

// crashed runs the crash hook after a step.
func crashed(step string) bool { return replaceCrash != nil && replaceCrash(step) }

// failAt runs the failure hook of a step.
func failAt(step string) error {
	if replaceFail != nil {
		return replaceFail(step)
	}
	return nil
}

// ReplaceDatabase replaces the metadata database at r.Path with the database file
// r.NewPath, such as a metadata snapshot of another instance, and returns the path
// of the copy of the previous database. It holds the data directory lock
// (LockDataDir) throughout, so no store may have r.Path open.
//
// r.NewPath is checked first: it must open with opts (WithSecretBox checks the
// secret key), migrate to this version's schema and pass PRAGMA quick_check. Both
// databases are then checkpointed (wal_checkpoint(TRUNCATE)) and closed, so
// neither depends on a write-ahead log. The replacement is crash-safe: a journal
// next to the database records it first; the previous database is linked (or
// copied) to r.Path + BackupInfix + "<UTC time>-epoch<N>"; r.NewPath is renamed
// over r.Path; the directory is synced after each step and the journal removed at
// the end. RecoverReplace settles a replacement a crash interrupted. Only the
// oldest copies beyond r.Keep are removed, so the previous database is never lost.
func ReplaceDatabase(ctx context.Context, r Replacement, opts ...Option) (string, error) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Keep <= 0 {
		r.Keep = DefaultBackupsKept
	}
	dir := filepath.Dir(r.Path)
	lock, err := LockDataDir(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Release() }()
	if err = RecoverReplace(r.Path, r.Logger); err != nil {
		return "", err
	}

	// 1. Check the replacement and leave it without a log.
	candidate, err := OpenSQLite(ctx, r.NewPath, r.Logger, opts...)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidReplacement, err)
	}
	var check string
	err = candidate.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check)
	if err == nil && check == "ok" {
		err = checkpoint(ctx, candidate.db)
	}
	closeErr := candidate.Close()
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %w", ErrInvalidReplacement, err)
	case check != "ok":
		return "", fmt.Errorf("%w: integrity check: %s", ErrInvalidReplacement, check)
	case closeErr != nil:
		return "", closeErr
	}
	if err = removeLogs(r.NewPath); err != nil {
		return "", err
	}
	newSum, err := fileSHA256(r.NewPath)
	if err != nil {
		return "", err
	}

	// 2. Checkpoint the current database, so its file holds everything.
	_, statErr := os.Stat(r.Path)
	exists := statErr == nil
	if !exists && !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("store: stat the database: %w", statErr)
	}
	if exists {
		if err = checkpointFile(ctx, r.Path); err != nil {
			return "", fmt.Errorf("store: checkpoint the database before it is replaced: %w", err)
		}
		if err = removeLogs(r.Path); err != nil {
			return "", err
		}
	}

	// 3. Journal the replacement.
	bak := ""
	if exists {
		bak = fmt.Sprintf("%s%s%s-epoch%d", r.Path, BackupInfix, r.Now().UTC().Format("20060102T150405Z"), r.Epoch)
		if _, err = os.Stat(bak); err == nil {
			bak += "-" + strconv.FormatInt(r.Now().UnixNano(), 10)
		}
	}
	journalPath := r.Path + replaceJournalSuffix
	journal := replaceJournal{NewPath: r.NewPath, NewSHA256: newSum, Backup: bak, Epoch: r.Epoch, Phase: phaseJournaled, StartedAt: r.Now().UTC()}
	if err = writeJournal(journalPath, journal); err != nil {
		return "", err
	}
	syncDir(dir)
	if crashed(phaseJournaled) {
		return "", errCrashed
	}
	// An error before the swap leaves the previous database in place: the journal
	// goes (with a partial copy), so nothing reports an interrupted replacement.
	abort := func(cause error) (string, error) {
		_ = removeIfExists(journalPath)
		syncDir(dir)
		r.Logger.Warn("the metadata database was not replaced; the previous one stays in use", slog.Any("error", cause))
		return "", cause
	}
	setPhase := func(phase string) error {
		journal.Phase = phase
		if werr := writeJournal(journalPath, journal); werr != nil {
			return werr
		}
		syncDir(dir)
		return nil
	}

	// 4. Keep the previous database: a hard link costs nothing, and the rename
	// below then replaces only the name r.Path.
	if exists {
		err = failAt(phaseKept)
		if err == nil {
			if linkErr := os.Link(r.Path, bak); linkErr != nil {
				err = copyFile(r.Path, bak)
			}
		}
		if err != nil {
			_ = removeIfExists(bak)
			return abort(fmt.Errorf("store: keep the previous database: %w", err))
		}
		syncDir(dir)
	}
	if err = setPhase(phaseKept); err != nil {
		return abort(err)
	}
	if crashed(phaseKept) {
		return "", errCrashed
	}

	// 5. Swap.
	if err = setPhase(phaseSwapping); err != nil {
		return abort(err)
	}
	if err = os.Rename(r.NewPath, r.Path); err != nil {
		return abort(fmt.Errorf("store: replace the database: %w", err))
	}
	syncDir(dir)
	if crashed(phaseSwapping) {
		return "", errCrashed
	}
	if err = setPhase(phaseSwapped); err != nil {
		r.Logger.Warn("the metadata database was replaced, but its journal could not be updated; the next start settles it", slog.Any("error", err))
	}
	if crashed(phaseSwapped) {
		return "", errCrashed
	}
	if err = restrictPermissions(r.Path); err != nil {
		return "", err
	}

	// 6. Done.
	if err = removeIfExists(r.Path + replaceJournalSuffix); err != nil {
		return "", err
	}
	syncDir(dir)
	pruneBackups(r.Path, r.Keep, r.Logger)
	r.Logger.Info("replaced the metadata database", slog.String("path", r.Path), slog.String("previous", bak))
	return bak, nil
}

// RecoverReplace settles a replacement of the database at path that a crash
// interrupted (see ReplaceDatabase), and reports it in the log. Before the swap the
// previous database is still in place and the replacement file is removed; after
// it the replacement is in place. When the database is missing it returns an error
// wrapping ErrInterruptedReplace instead of letting it be recreated empty. Call it
// before the database is opened; it does nothing when no replacement was
// interrupted.
func RecoverReplace(path string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	journalPath := path + replaceJournalSuffix
	data, err := os.ReadFile(journalPath) //nolint:gosec // G304: next to the database.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read %s: %w", filepath.Base(journalPath), err)
	}
	var j replaceJournal
	if err = json.Unmarshal(data, &j); err != nil {
		return fmt.Errorf("%w: %s is unreadable; check %s and its copies (%s*) by hand, then remove it", ErrInterruptedReplace, journalPath, path, path+BackupInfix)
	}
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("%w: %s is missing (phase %s); restore it from %s, then remove %s", ErrInterruptedReplace, path, j.Phase, j.Backup, journalPath)
	}
	// The content decides, not the phase alone: the rename may have happened
	// without the journal saying so, and the replacement file may be gone.
	swapped := j.Phase == phaseSwapped
	if j.NewSHA256 != "" {
		sum, sumErr := fileSHA256(path)
		if sumErr != nil {
			return fmt.Errorf("store: settle an interrupted replacement: %w", sumErr)
		}
		swapped = sum == j.NewSHA256
	} else if _, newErr := os.Stat(j.NewPath); newErr != nil {
		swapped = true
	}
	if swapped {
		logger.Warn("a replacement of the metadata database was interrupted after the swap; the new database is in use",
			slog.String("path", path), slog.String("phase", j.Phase), slog.String("previous", j.Backup))
	} else {
		if err = removeIfExists(j.NewPath); err != nil {
			return err
		}
		if err = removeLogs(j.NewPath); err != nil {
			return err
		}
		logger.Warn("a replacement of the metadata database was interrupted before the swap; the previous database stays in use",
			slog.String("path", path), slog.String("phase", j.Phase), slog.String("replacement", j.NewPath))
	}
	if err = removeIfExists(journalPath); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// checkpoint copies the whole write-ahead log of db into the database file and
// truncates it; it fails when a reader kept part of the log in use.
func checkpoint(ctx context.Context, db *sql.DB) error {
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint: the database is busy")
	}
	return nil
}

// checkpointFile checkpoints the database file at path without migrating it.
func checkpointFile(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	err = checkpoint(ctx, db)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	return err
}

// fileSHA256 returns the hex SHA-256 of the file at path, streamed.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a database file of the data directory.
	if err != nil {
		return "", fmt.Errorf("store: hash %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", fmt.Errorf("store: hash %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeLogs removes the write-ahead log and shared memory files of a checkpointed
// and closed database.
func removeLogs(path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := removeIfExists(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

// writeJournal writes j to path and syncs it.
func writeJournal(path string, j replaceJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("store: encode the replacement journal: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, dbFileMode) //nolint:gosec // G304: next to the database.
	if err != nil {
		return fmt.Errorf("store: write the replacement journal: %w", err)
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("store: write the replacement journal: %w", err)
	}
	return nil
}

// DatabaseBackups returns the copies of replaced databases kept next to path,
// oldest first.
func DatabaseBackups(path string) ([]string, error) {
	matches, err := filepath.Glob(path + BackupInfix + "*")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range matches {
		if strings.HasSuffix(m, "-wal") || strings.HasSuffix(m, "-shm") {
			continue
		}
		out = append(out, m)
	}
	sort.Strings(out) // the UTC time in the name sorts them
	return out, nil
}

// pruneBackups removes the oldest copies beyond keep, with their logs.
func pruneBackups(path string, keep int, logger *slog.Logger) {
	backups, err := DatabaseBackups(path)
	if err != nil || len(backups) <= keep {
		return
	}
	for _, old := range backups[:len(backups)-keep] {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if rmErr := removeIfExists(old + suffix); rmErr != nil {
				logger.Warn("cannot remove an old copy of the metadata database", slog.String("path", old+suffix), slog.Any("error", rmErr))
			}
		}
	}
}

// removeIfExists removes path, ignoring a missing file.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// copyFile copies src to the new file dst (mode 0600).
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: the database path of the data directory.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, dbFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// syncDir flushes a directory entry change to disk where the platform allows it.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the data directory.
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
