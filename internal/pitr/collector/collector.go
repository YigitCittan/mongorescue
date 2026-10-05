// Package collector runs the PITR oplog collector of every enabled PITR stream
// (docs/design/pitr.md): one goroutine per stream with one long-lived session to
// its replica set reads the majority-committed oplog in bounded ranges, streams
// each range through the chunk pipeline (scanner, gzip, age, SHA-256) to the
// stream's storage target and commits the chunk and the collector's position in one
// transaction. It also ends chains on gaps and divergences, schedules the stream's
// base backups and applies its retention.
//
// The package is a business package: it depends on ports (the PITR repository, an
// oplog session opener, storage drivers, a base backup starter), never on the HTTP
// server or the MongoDB driver. The service is off unless a stream is enabled: it
// then only lists the streams now and then.
package collector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// Chunk interval bounds (docs/design/pitr.md, decision 9).
const (
	// DefaultChunkSeconds is the default chunk interval.
	DefaultChunkSeconds = 60
	// MinChunkSeconds and MaxChunkSeconds bound the chunk interval.
	MinChunkSeconds = 15
	MaxChunkSeconds = 900
	// DefaultMaxChunkBytes is the uncompressed size above which the interval is
	// halved.
	DefaultMaxChunkBytes int64 = 4 << 30
)

// KeyPrefix is the storage key prefix of oplog chunks:
// KeyPrefix + <conn_id>/<rs>/<chain>/<from>-<to>.bson.gz.age.
const KeyPrefix = "_mongorescue/oplog/"

// Sentinel errors.
var (
	// ErrEncryptionRequired is returned while backup encryption is off: oplog
	// chunks hold every write, deleted data included, so they are always encrypted.
	ErrEncryptionRequired = errors.New("collector: PITR requires backup encryption")
	// ErrUnavailable is returned when the service is not running.
	ErrUnavailable = errors.New("collector: the PITR collector is not running")
)

// Session is one long-lived connection to a stream's replica set (implemented by
// *mongoconn.OplogSession). Errors must never include credentials.
type Session interface {
	// OplogWindow returns the oplog window and the majority-committed optime.
	OplogWindow(ctx context.Context) (pitr.OplogWindow, error)
	// ReadOplog copies the oplog range r to w, proving its continuity
	// (pitr.ErrOplogGap, pitr.ErrOplogBehind).
	ReadOplog(ctx context.Context, r pitr.OplogRange, w io.Writer) (pitr.OplogStats, error)
	// EntryAt returns the term of the entry at ts, or found false.
	EntryAt(ctx context.Context, ts pitr.Timestamp) (term int64, found bool, err error)
	// EntryAtOrAfter returns the first entry at or after ts, or found false.
	EntryAtOrAfter(ctx context.Context, ts pitr.Timestamp) (op pitr.OpTime, found bool, err error)
	// Close disconnects the session.
	Close()
}

// Opener opens the session of a stream (its connection, with its read
// preference).
type Opener func(ctx context.Context, s *pitr.Stream) (Session, error)

// StorageFunc returns the storage driver of a storage target.
type StorageFunc func(ctx context.Context, targetID string) (storage.Storage, error)

// BaseStarter starts a base backup of a stream in the background (implemented by
// operations.Service.StartBaseBackup).
type BaseStarter func(ctx context.Context, streamID string, trigger models.BackupTrigger) (*models.BackupRecord, error)

// BaseLister returns the base backups of a stream, newest first (implemented by
// store.SQLiteStore.ListBaseBackups).
type BaseLister func(ctx context.Context, streamID string) ([]*models.BackupRecord, error)

// Observer receives the collector's metrics (implemented by *metrics.Metrics).
type Observer interface {
	SetPITRCollectorUp(stream string, up bool)
	ObservePITRChunk(stream string, ok bool, sizeBytes int64, end time.Time)
	SetPITRLag(stream string, lag, headroom time.Duration)
	SetPITRWindow(stream string, start, end time.Time)
	IncPITRChainBreak(stream, reason string)
	ForgetPITRStream(stream string)
}

// Clock is the time source; tests inject a fake one.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// After waits for d like time.After.
	After(d time.Duration) <-chan time.Time
}

// realClock is the wall clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config holds the dependencies of a Service. Repo, Open, Storage and Encryptor
// are required.
type Config struct {
	// Repo persists streams, chains, chunks and the collector state.
	Repo pitr.Repository
	// Open opens the session of a stream.
	Open Opener
	// Storage resolves storage drivers.
	Storage StorageFunc
	// Encryptor returns the backup encryptor (nil while encryption is off).
	Encryptor func() *encryption.Encryptor
	// StartBase starts base backups (the schedule, gaps and TakeBase); nil
	// disables them.
	StartBase BaseStarter
	// Bases lists the base backups of a stream; nil disables the base schedule,
	// base retention and windows.
	Bases BaseLister
	// Publisher receives the pitr.* events; nil disables them.
	Publisher events.Publisher
	// Observer receives metrics; nil disables them.
	Observer Observer
	// Logger receives operational logs; nil means slog.Default().
	Logger *slog.Logger
	// Clock is the time source; nil means the wall clock.
	Clock Clock
	// MaxChunkBytes is the uncompressed chunk size above which the interval is
	// halved; 0 means DefaultMaxChunkBytes.
	MaxChunkBytes int64
	// ReconcileInterval is how often the service re-reads the streams and runs
	// the base schedule (default 30 s); RetentionInterval how often it applies
	// retention (default 10 min).
	ReconcileInterval time.Duration
	RetentionInterval time.Duration
	// DeleteGrace returns the delete grace period of retention; nil means
	// models.DefaultDeleteGraceDays.
	DeleteGrace func() time.Duration
	// Inspect inspects the replica set of a connection when a stream is created
	// or enabled; nil refuses them.
	Inspect Inspector
	// ResolveTarget resolves the storage target of a stream; nil keeps the given
	// ID.
	ResolveTarget TargetResolver
	// NextRun returns the first activation of a cron expression after from
	// (implemented with scheduler.NextRuns); nil disables the base schedule.
	NextRun func(expr string, from time.Time) (time.Time, bool)
}

// Service runs the collectors of the enabled streams. It is safe for concurrent
// use.
type Service struct {
	cfg    Config
	logger *slog.Logger

	lifeMu  sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
	kick    chan struct{}

	mu      sync.Mutex
	workers map[string]*runningWorker
	// lastRetention is when retention last ran.
	lastRetention time.Time
}

// runningWorker is the collector goroutine of one stream.
type runningWorker struct {
	updatedAt time.Time
	cancel    context.CancelFunc
	done      chan struct{}
	w         *worker
}

// New returns a Service. It panics when a required dependency is missing, which is
// a wiring bug.
func New(cfg Config) *Service {
	if cfg.Repo == nil || cfg.Open == nil || cfg.Storage == nil || cfg.Encryptor == nil {
		panic("collector: Repo, Open, Storage and Encryptor are required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.MaxChunkBytes <= 0 {
		cfg.MaxChunkBytes = DefaultMaxChunkBytes
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 30 * time.Second
	}
	if cfg.RetentionInterval <= 0 {
		cfg.RetentionInterval = 10 * time.Minute
	}
	return &Service{cfg: cfg, logger: cfg.Logger, workers: map[string]*runningWorker{}, kick: make(chan struct{}, 1)}
}

// now returns the current time in UTC.
func (s *Service) now() time.Time { return s.cfg.Clock.Now().UTC() }

// Start runs the supervisor that starts and stops the collectors of the enabled
// streams, until Stop or ctx ends. It is a no-op when already started.
func (s *Service) Start(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.supervise(s.ctx)
	}()
}

// Stop stops every collector and the supervisor and waits for them. A chunk being
// written is abandoned: nothing of it is committed, and the next start reads the
// range again. It is safe to call more than once and before Start.
func (s *Service) Stop() {
	s.lifeMu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.lifeMu.Unlock()
	s.wg.Wait()
}

// Reload makes the supervisor re-read the streams now (after a stream was
// created, changed or deleted).
func (s *Service) Reload() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// supervise reconciles the collectors with the streams, runs the base schedule and
// retention, until ctx ends.
func (s *Service) supervise(ctx context.Context) {
	defer s.stopAll()
	for {
		s.reconcile(ctx)
		s.maintain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-s.cfg.Clock.After(s.cfg.ReconcileInterval):
		}
	}
}

// reconcile starts a collector for every enabled stream (restarting it when the
// stream changed) and stops the others.
func (s *Service) reconcile(ctx context.Context) {
	streams, err := s.cfg.Repo.ListStreams(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("cannot list the PITR streams", logsafe.Error(err))
		}
		return
	}
	want := map[string]*pitr.Stream{}
	for _, st := range streams {
		if st.Enabled {
			want[st.ID] = st
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rw := range s.workers {
		if st, ok := want[id]; !ok || !st.UpdatedAt.Equal(rw.updatedAt) {
			rw.cancel()
			<-rw.done
			delete(s.workers, id)
			if !ok {
				s.observe(func(o Observer) { o.SetPITRCollectorUp(id, false) })
			}
		}
	}
	for id, st := range want {
		if _, ok := s.workers[id]; ok {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := newWorker(s, st)
		rw := &runningWorker{updatedAt: st.UpdatedAt, cancel: cancel, done: make(chan struct{}), w: w}
		s.workers[id] = rw
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer close(rw.done)
			w.run(wctx)
		}()
	}
}

// stopAll stops every collector.
func (s *Service) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, rw := range s.workers {
		rw.cancel()
		<-rw.done
		delete(s.workers, id)
	}
}

// Running reports whether the collector of stream id is running.
func (s *Service) Running(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.workers[id]
	return ok
}

// publish sends e when a publisher is configured.
func (s *Service) publish(ctx context.Context, e events.Event) {
	if s.cfg.Publisher != nil {
		s.cfg.Publisher.Publish(context.WithoutCancel(ctx), e)
	}
}

// observe calls fn with the observer when one is configured.
func (s *Service) observe(fn func(Observer)) {
	if s.cfg.Observer != nil {
		fn(s.cfg.Observer)
	}
}

// newID returns prefix followed by 16 random hex characters.
func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// ChunkKey returns the storage key of the chunk (from, to] of chain on stream st.
func ChunkKey(st *pitr.Stream, chainID string, from, to pitr.Timestamp) string {
	return KeyPrefix + models.SanitizeIDComponent(st.ConnectionID, 0) + "/" + models.SanitizeIDComponent(st.ReplicaSet, 0) +
		"/" + chainID + "/" + from.String() + "-" + to.String() + ".bson.gz" + encryption.FileExtension
}
