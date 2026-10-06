// Package reencrypt re-encrypts existing encrypted backups under the current backup
// encryption key after an age key rotation, as a resumable background job.
//
// Every archive is streamed from storage, decrypted with the current or a retired
// key, encrypted under the current key and written to a new object; nothing is
// buffered. The new object is read back and checked (its SHA-256, and a decryption
// whose plaintext hash must match the original's) before the backup record is
// pointed at it in one transaction. The old object is not deleted at once: the same
// transaction stores a deleted copy of the record (a tombstone) that hands the old
// archive to the delete grace period, so the purge removes it later and it can be
// undeleted until then.
//
// The job's progress is persisted after every backup. A job interrupted by a
// shutdown or a crash resumes on the next start: a half-written new object is
// removed and the backup processed again; finished backups are skipped.
package reencrypt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// stateKey is the integrity state document holding the job.
const stateKey = "encryption.reencrypt"

// maxErrors bounds the per-backup errors kept in the state.
const maxErrors = 50

// Job statuses.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Sentinel errors. Their messages are safe to show to clients.
var (
	// ErrBusy is returned while a job runs.
	ErrBusy = errors.New("reencrypt: a re-encryption job is already running")
	// ErrNoEncryptor is returned when backup encryption is disabled.
	ErrNoEncryptor = errors.New("reencrypt: backup encryption is disabled, so there is no key to re-encrypt to")
	// ErrNoDecryptor is returned when no key can decrypt existing backups.
	ErrNoDecryptor = errors.New("reencrypt: no encryption key can decrypt the existing backups")
	// ErrNotStarted is returned before Start.
	ErrNotStarted = errors.New("reencrypt: the service is not running")
	// ErrHashMismatch is returned when an archive does not match its recorded hash.
	ErrHashMismatch = errors.New("reencrypt: the archive does not match its recorded SHA-256")
	// ErrVerify is returned when the new archive does not read back as written.
	ErrVerify = errors.New("reencrypt: the re-encrypted archive did not verify")
)

// Store is the persistence port (implemented by *store.SQLiteStore).
type Store interface {
	ListBackupRecords(ctx context.Context, database string) ([]*models.BackupRecord, error)
	GetBackupRecord(ctx context.Context, id string) (*models.BackupRecord, error)
	SwapBackupArchive(ctx context.Context, sw store.ArchiveSwap) error
	LoadIntegrityState(ctx context.Context, key string, v any) (bool, error)
	SaveIntegrityState(ctx context.Context, key string, v any) error
}

// Item is the backup being re-encrypted.
type Item struct {
	BackupID string `json:"backup_id"`
	TargetID string `json:"storage_target_id"`
	OldKey   string `json:"old_key"`
	NewKey   string `json:"new_key"`
}

// ItemError is a backup that could not be re-encrypted.
type ItemError struct {
	BackupID string `json:"backup_id"`
	Error    string `json:"error"`
}

// State is the persisted job.
type State struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	RequestedBy string     `json:"requested_by,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	// Total counts the backups to process; Done, Skipped and Failed their outcomes.
	Total   int `json:"total"`
	Done    int `json:"done"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
	// Finished lists the backups processed (re-encrypted, skipped or failed).
	Finished []string `json:"finished,omitempty"`
	// Current is the backup in progress, cleaned up when the job resumes.
	Current *Item `json:"current,omitempty"`
	// Errors are the latest per-backup failures (at most 50).
	Errors []ItemError `json:"errors,omitempty"`
	// Error is why the job itself stopped.
	Error string `json:"error,omitempty"`
}

// Config holds the dependencies of a Service.
type Config struct {
	Store Store
	// Storage returns the driver of a storage target.
	Storage func(ctx context.Context, targetID string) (storage.Storage, error)
	// Encryptor returns the current encryptor (nil when encryption is disabled).
	Encryptor func() *encryption.Encryptor
	// Decryptor returns the decryptor with every current and retired key.
	Decryptor func() *encryption.Decryptor
	// Grace returns the delete grace period that protects the old archives.
	Grace  func() time.Duration
	Logger *slog.Logger
	Now    func() time.Time
	// AfterItem, when set, runs after every processed backup (tests).
	AfterItem func(backupID string) error
}

// Service runs re-encryption jobs. It is safe for concurrent use.
type Service struct {
	cfg Config

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	running bool
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Grace == nil {
		cfg.Grace = func() time.Duration { return 0 }
	}
	return &Service{cfg: cfg}
}

// Start binds the service to ctx (the application lifecycle) and resumes a job that
// a shutdown or crash interrupted.
func (s *Service) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.ctx, s.cancel = ctx, cancel
	s.mu.Unlock()
	st, ok, err := s.load(ctx)
	if err != nil {
		s.cfg.Logger.Warn("could not read the re-encryption job", logsafe.Error(err))
		return
	}
	if ok && st.Status == StatusRunning {
		s.cfg.Logger.Info("resuming the interrupted re-encryption job", slog.String("job_id", st.ID))
		s.launch(st)
	}
}

// Wait blocks until a running job stopped (after the lifecycle context ended).
func (s *Service) Wait() { s.wg.Wait() }

// Stop cancels a running job, which resumes on the next start, and waits for it.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// Status returns the latest job (nil when none ran).
func (s *Service) Status(ctx context.Context) (*State, error) {
	st, ok, err := s.load(ctx)
	if err != nil || !ok {
		return nil, err
	}
	return st, nil
}

// Trigger starts a job re-encrypting every encrypted backup under the current key.
func (s *Service) Trigger(ctx context.Context, requestedBy string) (*State, error) {
	if s.cfg.Encryptor == nil || s.cfg.Encryptor() == nil {
		return nil, ErrNoEncryptor
	}
	if s.cfg.Decryptor == nil || s.cfg.Decryptor() == nil {
		return nil, ErrNoDecryptor
	}
	s.mu.Lock()
	running, started := s.running, s.ctx != nil
	s.mu.Unlock()
	if !started {
		return nil, ErrNotStarted
	}
	if running {
		return nil, ErrBusy
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	st := &State{ID: "reenc_" + hex.EncodeToString(id), Status: StatusRunning, RequestedBy: requestedBy, StartedAt: s.cfg.Now().UTC()}
	if err := s.save(ctx, st); err != nil {
		return nil, err
	}
	// The job owns st from now on; the caller gets a copy taken before it runs.
	// A new job's slices and pointers are still nil, so a shallow copy is enough.
	snapshot := *st
	if !s.launch(st) {
		return nil, ErrBusy
	}
	return &snapshot, nil
}

// launch runs st in the background under the lifecycle context.
func (s *Service) launch(st *State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running || s.ctx == nil {
		return false
	}
	s.running = true
	ctx := s.ctx
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		s.run(ctx, st)
	}()
	return true
}

// run processes every encrypted backup not finished yet.
func (s *Service) run(ctx context.Context, st *State) {
	log := s.cfg.Logger.With(slog.String("job_id", st.ID))
	fail := func(err error) {
		if ctx.Err() != nil {
			return // shutdown: the job resumes on the next start
		}
		now := s.cfg.Now().UTC()
		st.Status, st.Error, st.FinishedAt = StatusFailed, err.Error(), &now
		s.persist(ctx, st, log)
		log.Error("re-encryption job failed", logsafe.Error(err))
	}
	if st.Current != nil {
		if err := s.cleanUp(ctx, st); err != nil {
			fail(err)
			return
		}
	}
	records, err := s.cfg.Store.ListBackupRecords(ctx, "")
	if err != nil {
		fail(err)
		return
	}
	todo := make([]*models.BackupRecord, 0, len(records))
	for _, r := range records {
		if r.Encrypted && r.Status == models.StatusCompleted && r.StorageKey != "" {
			todo = append(todo, r)
		}
	}
	if st.Total == 0 {
		st.Total = len(todo)
	}
	for _, rec := range todo {
		if slices.Contains(st.Finished, rec.ID) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		err := s.process(ctx, st, rec)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrArchiveChanged), errors.Is(err, store.ErrNotFound):
			st.Skipped++
		case err != nil:
			st.Failed++
			st.Errors = append(st.Errors, ItemError{BackupID: rec.ID, Error: err.Error()})
			if len(st.Errors) > maxErrors {
				st.Errors = st.Errors[len(st.Errors)-maxErrors:]
			}
			log.Warn("backup could not be re-encrypted", slog.String("backup_id", rec.ID), logsafe.Error(err))
		default:
			st.Done++
		}
		st.Current = nil
		st.Finished = append(st.Finished, rec.ID)
		s.persist(ctx, st, log)
		if s.cfg.AfterItem != nil {
			if err := s.cfg.AfterItem(rec.ID); err != nil {
				return
			}
		}
	}
	now := s.cfg.Now().UTC()
	st.Status, st.FinishedAt = StatusCompleted, &now
	if st.Failed > 0 {
		st.Status, st.Error = StatusFailed, fmt.Sprintf("%d backups could not be re-encrypted", st.Failed)
	}
	s.persist(ctx, st, log)
	log.Info("re-encryption job finished", slog.Int("done", st.Done), slog.Int("skipped", st.Skipped), slog.Int("failed", st.Failed))
}

// cleanUp settles the backup an interrupted job was working on: when its record
// already points at the new archive it is done, otherwise the half-written new
// object is removed and the backup is processed again.
func (s *Service) cleanUp(ctx context.Context, st *State) error {
	cur := st.Current
	rec, err := s.cfg.Store.GetBackupRecord(ctx, cur.BackupID)
	switch {
	case err == nil && rec.StorageKey == cur.NewKey:
		st.Done++
		st.Finished = append(st.Finished, cur.BackupID)
	case err == nil || errors.Is(err, store.ErrNotFound):
		drv, derr := s.cfg.Storage(ctx, cur.TargetID)
		if derr != nil {
			return derr
		}
		if derr = drv.Delete(ctx, cur.NewKey); derr != nil && !errors.Is(derr, storage.ErrNotFound) {
			return fmt.Errorf("reencrypt: remove the partial archive of backup %s: %w", cur.BackupID, derr)
		}
	default:
		return err
	}
	st.Current = nil
	return s.save(ctx, st)
}

// newKeyFor names the re-encrypted archive of key: the same directory and
// extensions, the backup ID with a "-rk<unix time>" suffix.
func newKeyFor(key string, at time.Time) string {
	dir, base := path.Split(key)
	name, ext, _ := strings.Cut(base, ".")
	if ext != "" {
		ext = "." + ext
	}
	return fmt.Sprintf("%s%s-rk%d%s", dir, name, at.Unix(), ext)
}

// process re-encrypts one backup (see the package comment).
func (s *Service) process(ctx context.Context, st *State, rec *models.BackupRecord) error {
	enc, dec := s.cfg.Encryptor(), s.cfg.Decryptor()
	if enc == nil {
		return ErrNoEncryptor
	}
	if dec == nil {
		return ErrNoDecryptor
	}
	drv, err := s.cfg.Storage(ctx, rec.StorageTargetID)
	if err != nil {
		return err
	}
	now := s.cfg.Now().UTC()
	item := &Item{BackupID: rec.ID, TargetID: rec.StorageTargetID, OldKey: rec.StorageKey, NewKey: newKeyFor(rec.StorageKey, now)}
	if !strings.HasSuffix(item.NewKey, encryption.FileExtension) {
		item.NewKey += encryption.FileExtension
	}
	st.Current = item
	if err = s.save(ctx, st); err != nil {
		return err
	}
	removeNew := func() { _ = drv.Delete(context.WithoutCancel(ctx), item.NewKey) }

	oldHash, plainHash, newHash, size, err := s.copy(ctx, drv, item, enc, dec)
	if err != nil {
		removeNew()
		return err
	}
	if rec.SHA256 != "" && !strings.EqualFold(rec.SHA256, oldHash) {
		removeNew()
		return ErrHashMismatch
	}
	if err = verify(ctx, drv, item.NewKey, dec, newHash, plainHash); err != nil {
		removeNew()
		return err
	}
	grace := s.cfg.Grace()
	tomb := *rec
	tomb.ID = rec.ID + "-reenc-" + strings.TrimPrefix(st.ID, "reenc_")
	tomb.Status, tomb.StatusBeforeDelete = models.StatusDeleted, rec.Status
	purge := now.Add(grace)
	tomb.DeletedAt, tomb.PurgeAfter = &now, &purge
	tomb.DeletedBy, tomb.DeleteReason = "system", "archive replaced by its re-encryption under the current key (backup "+rec.ID+")"
	tomb.Pinned, tomb.Manifest = false, nil
	err = s.cfg.Store.SwapBackupArchive(ctx, store.ArchiveSwap{
		BackupID: rec.ID, OldKey: item.OldKey, NewKey: item.NewKey, SizeBytes: size, SHA256: newHash,
		EncryptionMode: string(enc.Mode()), Tombstone: &tomb,
	})
	if err != nil {
		removeNew()
		return err
	}
	return nil
}

// hashingReader hashes and counts what is read through it.
type hashingReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}

// copy streams the old archive through decryption and encryption into the new
// object, hashing the old ciphertext, the plaintext and the new ciphertext.
func (s *Service) copy(ctx context.Context, drv storage.Storage, item *Item, enc *encryption.Encryptor, dec *encryption.Decryptor) (oldHash, plainHash, newHash string, size int64, err error) {
	src, err := drv.Retrieve(ctx, item.OldKey)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("reencrypt: read %s: %w", item.BackupID, err)
	}
	defer func() { _ = src.Close() }()
	oldR := &hashingReader{r: src, h: sha256.New()}
	plain, err := dec.Decrypt(oldR)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("reencrypt: decrypt %s: %w", item.BackupID, err)
	}
	plainR := &hashingReader{r: plain, h: sha256.New()}
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	var encErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		w, e := enc.Encrypt(pw)
		if e == nil {
			if _, e = io.Copy(w, plainR); e == nil {
				e = w.Close()
			}
		}
		encErr = e
		_ = pw.CloseWithError(e)
	}()
	newR := &hashingReader{r: pr, h: sha256.New()}
	_, saveErr := drv.Save(ctx, item.NewKey, newR)
	_ = pr.CloseWithError(errors.New("reencrypt: upload ended"))
	wg.Wait()
	if err = errors.Join(saveErr, encErr); err != nil {
		return "", "", "", 0, fmt.Errorf("reencrypt: write %s: %w", item.BackupID, err)
	}
	// Drain what the decryptor left (the age trailer) so the old hash covers it all.
	if _, err = io.Copy(io.Discard, oldR); err != nil {
		return "", "", "", 0, fmt.Errorf("reencrypt: read %s: %w", item.BackupID, err)
	}
	return hex.EncodeToString(oldR.h.Sum(nil)), hex.EncodeToString(plainR.h.Sum(nil)),
		hex.EncodeToString(newR.h.Sum(nil)), newR.n, nil
}

// verify reads the new object back: its SHA-256 must be newHash and its plaintext
// must hash to plainHash.
func verify(ctx context.Context, drv storage.Storage, key string, dec *encryption.Decryptor, newHash, plainHash string) error {
	src, err := drv.Retrieve(ctx, key)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVerify, err)
	}
	defer func() { _ = src.Close() }()
	cipher := &hashingReader{r: src, h: sha256.New()}
	plain, err := dec.Decrypt(cipher)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVerify, err)
	}
	ph := sha256.New()
	if _, err = io.Copy(ph, plain); err != nil {
		return fmt.Errorf("%w: %w", ErrVerify, err)
	}
	if _, err = io.Copy(io.Discard, cipher); err != nil {
		return fmt.Errorf("%w: %w", ErrVerify, err)
	}
	if hex.EncodeToString(cipher.h.Sum(nil)) != newHash || hex.EncodeToString(ph.Sum(nil)) != plainHash {
		return ErrVerify
	}
	return nil
}

// load reads the persisted job.
func (s *Service) load(ctx context.Context) (*State, bool, error) {
	var st State
	ok, err := s.cfg.Store.LoadIntegrityState(ctx, stateKey, &st)
	if err != nil {
		return nil, false, fmt.Errorf("reencrypt: read the job: %w", err)
	}
	return &st, ok, nil
}

// save persists st.
func (s *Service) save(ctx context.Context, st *State) error {
	if err := s.cfg.Store.SaveIntegrityState(context.WithoutCancel(ctx), stateKey, st); err != nil {
		return fmt.Errorf("reencrypt: save the job: %w", err)
	}
	return nil
}

// persist saves st, logging a failure.
func (s *Service) persist(ctx context.Context, st *State, log *slog.Logger) {
	if err := s.save(ctx, st); err != nil {
		log.Warn("could not save the re-encryption job", logsafe.Error(err))
	}
}
