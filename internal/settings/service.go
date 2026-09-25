package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
)

// Repository is the persistence port for settings. Values are JSON documents keyed
// by setting key; implementations encrypt the values of keys for which IsSecret
// reports true and decrypt them when loading.
type Repository interface {
	// LoadSettings returns every stored setting and import marker (see IsKnown).
	LoadSettings(ctx context.Context) (map[string]string, error)
	// SaveSettings upserts values in one transaction.
	SaveSettings(ctx context.Context, values map[string]string) error
}

// Service owns the live settings. Every change is validated, persisted and then
// applied immediately: readers (engines, scheduler, HTTP server, auth) call Current,
// Encryptor or Decryptor for each operation, so no restart is needed. It is safe for
// concurrent use.
type Service struct {
	repo   Repository
	logger *slog.Logger
	now    func() time.Time

	// writeMu serialises updates; mu guards the snapshot below.
	writeMu sync.Mutex
	mu      sync.RWMutex
	cur     Settings
	stored  map[string]bool
	enc     *encryption.Encryptor
	dec     *encryption.Decryptor
}

// Option customises a Service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService loads the stored settings (defaults for keys never saved) and prepares
// the encryption keys.
func NewService(ctx context.Context, repo Repository, opts ...Option) (*Service, error) {
	s := &Service{repo: repo, logger: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	values, err := repo.LoadSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("settings: load: %w", err)
	}
	cur, err := decode(values)
	if err != nil {
		return nil, err
	}
	enc, dec, err := s.keys(cur.Encryption)
	if err != nil {
		return nil, fmt.Errorf("settings: stored encryption settings: %w", err)
	}
	s.cur, s.enc, s.dec = cur, enc, dec
	s.stored = make(map[string]bool, len(values))
	for k := range values {
		s.stored[k] = true
	}
	return s, nil
}

// keys builds the encryptor and decryptor of e.
func (s *Service) keys(e Encryption) (*encryption.Encryptor, *encryption.Decryptor, error) {
	enc, err := newEncryptor(e)
	if err != nil {
		return nil, nil, err
	}
	dec, err := newDecryptor(e, s.logger)
	if err != nil {
		return nil, nil, err
	}
	return enc, dec, nil
}

// Current returns a copy of the live settings, secrets included. Never serialize it
// to clients; use Masked.
func (s *Service) Current() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Clone()
}

// Masked returns the live settings with secrets masked, for API responses.
func (s *Service) Masked() Settings {
	return s.Current().Masked()
}

// Encryptor returns the encryptor for new backups, or nil when encryption is off.
func (s *Service) Encryptor() *encryption.Encryptor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enc
}

// Decryptor returns the decryptor for restores (current and retired keys), or nil
// when no key is configured.
func (s *Service) Decryptor() *encryption.Decryptor {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dec
}

// Update validates and applies a partial update and returns the new settings
// (masked). Changes take effect for every operation started afterwards.
func (s *Service) Update(ctx context.Context, p Patch) (Settings, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	cur := s.Current()
	next, err := p.apply(cur, s.now())
	if err != nil {
		return Settings{}, err
	}
	// The passphrase length rule applies to new passphrases only: one imported from an
	// older release must not block unrelated changes.
	if err = validate(&next, next.Encryption.Passphrase != cur.Encryption.Passphrase); err != nil {
		return Settings{}, err
	}
	changed, err := s.commit(ctx, cur, next)
	if err != nil {
		return Settings{}, err
	}
	if len(changed) > 0 {
		s.logger.Info("settings updated", slog.Any("keys", changed))
	}
	return next.Masked(), nil
}

// commit persists the keys that differ between cur and next and swaps the snapshot.
// Caller holds writeMu.
func (s *Service) commit(ctx context.Context, cur, next Settings) ([]string, error) {
	before, err := encode(cur)
	if err != nil {
		return nil, err
	}
	after, err := encode(next)
	if err != nil {
		return nil, err
	}
	changes := map[string]string{}
	for k, v := range after {
		if before[k] != v {
			changes[k] = v
		}
	}
	if len(changes) == 0 {
		return nil, nil
	}
	enc, dec, err := s.keys(next.Encryption)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := s.repo.SaveSettings(ctx, changes); err != nil {
		return nil, fmt.Errorf("settings: save: %w", err)
	}
	s.mu.Lock()
	s.cur, s.enc, s.dec = next, enc, dec
	for k := range changes {
		s.stored[k] = true
	}
	s.mu.Unlock()
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

// Import is one value from a deprecated environment variable or legacy configuration
// file.
type Import struct {
	// Key is the setting key.
	Key string
	// Value is the JSON-encodable value in the setting's API representation (e.g. a
	// duration string).
	Value any
	// Source names where the value came from (e.g. "MONGORESCUE_BACKUP_TIMEOUT").
	Source string
}

// ImportResult reports what Import did with each source.
type ImportResult struct {
	// Imported lists the sources whose values were stored.
	Imported []string
	// Ignored lists sources skipped because the setting already had a stored value or
	// the source had been imported before.
	Ignored []string
	// Invalid lists sources whose values failed validation.
	Invalid []string
}

// Import stores values from deprecated sources, once: a value is only imported when
// its setting has no stored value yet and its source has not been imported before.
// Every source is marked as imported, so later starts ignore it. Invalid values are
// skipped. The minimum passphrase length is not enforced for imports, so existing
// encrypted backups stay restorable.
func (s *Service) Import(ctx context.Context, items []Import) (ImportResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var res ImportResult
	cur := s.Current()
	next := cur.Clone()
	markers := map[string]string{}
	stamp, err := json.Marshal(s.now().UTC())
	if err != nil {
		return res, err
	}
	for _, it := range items {
		s.mu.RLock()
		seen := s.stored[markerPrefix+it.Source]
		has := s.stored[it.Key]
		s.mu.RUnlock()
		if _, dup := markers[it.Source]; seen || dup {
			if !slices.Contains(res.Ignored, it.Source) && !slices.Contains(res.Imported, it.Source) {
				res.Ignored = append(res.Ignored, it.Source)
			}
			continue
		}
		if has {
			markers[it.Source] = string(stamp)
			res.Ignored = append(res.Ignored, it.Source)
			continue
		}
		candidate, err := withValue(next, it.Key, it.Value)
		if err == nil {
			err = validate(&candidate, false)
		}
		if err != nil {
			s.logger.Warn("ignoring invalid value of a deprecated setting", slog.String("source", it.Source), slog.Any("error", err))
			markers[it.Source] = string(stamp)
			res.Invalid = append(res.Invalid, it.Source)
			continue
		}
		next = candidate
		markers[it.Source] = string(stamp)
		res.Imported = append(res.Imported, it.Source)
	}
	if _, err := s.commit(ctx, cur, next); err != nil {
		return ImportResult{}, err
	}
	if err := s.markImported(ctx, markers); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}

// withValue returns s with key set to value (given in its JSON representation).
func withValue(s Settings, key string, value any) (Settings, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return s, fmt.Errorf("%w: %s: %w", ErrInvalid, key, err)
	}
	next := s.Clone()
	for _, d := range keyDefs {
		if d.name != key {
			continue
		}
		if err := d.set(&next, raw); err != nil {
			return s, fmt.Errorf("%w: %s: %w", ErrInvalid, key, err)
		}
		return next, nil
	}
	return s, fmt.Errorf("%w: unknown setting %q", ErrInvalid, key)
}

// WasImported reports whether source (a deprecated environment variable or legacy
// file) has been imported before.
func (s *Service) WasImported(source string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stored[markerPrefix+source]
}

// MarkImported records sources as imported, so later starts ignore them.
func (s *Service) MarkImported(ctx context.Context, sources ...string) error {
	stamp, err := json.Marshal(s.now().UTC())
	if err != nil {
		return err
	}
	markers := make(map[string]string, len(sources))
	for _, src := range sources {
		markers[src] = string(stamp)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.markImported(ctx, markers)
}

// markImported persists markers (source → timestamp). Caller holds writeMu.
func (s *Service) markImported(ctx context.Context, markers map[string]string) error {
	if len(markers) == 0 {
		return nil
	}
	values := make(map[string]string, len(markers))
	for src, v := range markers {
		values[markerPrefix+src] = v
	}
	if err := s.repo.SaveSettings(ctx, values); err != nil {
		return fmt.Errorf("settings: record imports: %w", err)
	}
	s.mu.Lock()
	for k := range values {
		s.stored[k] = true
	}
	s.mu.Unlock()
	return nil
}
