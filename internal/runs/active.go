package runs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongotools"
	"github.com/yigitcittan/mongorescue/internal/runlog"
)

// Errors of the run Registry.
var (
	// ErrCancelled is the cause of a run's context once the run was cancelled through
	// the Registry; the cause itself is a *Cancellation wrapping it.
	ErrCancelled = errors.New("cancelled")

	// ErrNotRunning is returned when a run to cancel is not active in this process.
	ErrNotRunning = errors.New("runs: run is not running")

	// ErrFinishing is returned when a run to cancel is past the point of no return
	// (see Run.Finishing): its tool finished and it is only recording the outcome.
	ErrFinishing = errors.New("runs: run is already finishing")
)

// SystemActor names the application itself as the canceller of a run (shutdown,
// the desktop app's force quit).
const SystemActor = "system"

// ActorKind classifies who cancelled a run. Logs record the kind and the user ID,
// never the display name of an API key.
type ActorKind string

// Actor kinds.
const (
	// ActorUser is a signed-in user (a session).
	ActorUser ActorKind = "user"
	// ActorAPIKey is an API key (REST or MCP).
	ActorAPIKey ActorKind = "api_key"
	// ActorSystem is the application itself.
	ActorSystem ActorKind = "system"
)

// Cancellation describes who stopped a run, and why. It is the cause of the run's
// context and wraps ErrCancelled.
type Cancellation struct {
	// By names the canceller for the record: a username, an API key or SystemActor.
	By string
	// Kind classifies the canceller (ActorSystem when empty); with UserID it is what
	// logs record.
	Kind ActorKind
	// UserID is the ID of the user who cancelled the run, or of the user who created
	// the API key, when known.
	UserID string
	// Reason, when set, explains the cancellation ("MongoRescue was force-quit").
	Reason string
	// At is when the cancellation was requested.
	At time.Time
}

// Error implements error: "cancelled by alice" or "cancelled: <reason>" (the reason
// alone when it already starts with "cancelled").
func (c *Cancellation) Error() string {
	switch {
	case strings.HasPrefix(c.Reason, "cancelled"):
		return c.Reason
	case c.Reason != "":
		return "cancelled: " + c.Reason
	case c.By != "":
		return "cancelled by " + c.By
	default:
		return "cancelled"
	}
}

// Unwrap returns ErrCancelled.
func (c *Cancellation) Unwrap() error { return ErrCancelled }

// LogAttrs returns the log attributes of the canceller: its kind and user ID.
func (c *Cancellation) LogAttrs() []any {
	kind := c.Kind
	if kind == "" {
		kind = ActorSystem
	}
	return []any{logsafe.Attr("cancelled_by_kind", string(kind)), logsafe.Attr("cancelled_by_user_id", c.UserID)}
}

// CancellationOf returns the Cancellation that ended ctx, or nil when ctx is live or
// ended for another reason (a timeout, a stall, a plain shutdown).
//
// A cancellation requested before the run bound its context counts even when the
// parent context ended first, before Bind could apply it (a force quit cancels the
// runs and then shuts the application down): the run then reports it, because Cancel
// only records cancellations that precede the end of the run's context.
func CancellationOf(ctx context.Context) *Cancellation {
	if ctx.Err() == nil {
		return nil
	}
	var c *Cancellation
	if errors.As(context.Cause(ctx), &c) {
		return c
	}
	return FromContext(ctx).Cancellation()
}

// Meta describes a run when it is registered.
type Meta struct {
	// Kind is models.RunBackup or models.RunRestore.
	Kind models.RunKind
	// ID is the backup or restore record ID.
	ID string
	// JobID is the job of a backup, if any.
	JobID string
	// Database is the backed-up database or the restore target.
	Database string
	// Group is the job run the backup belongs to (models.BackupRecord.RunID), if any.
	// Cancelling one run of a group cancels the whole group: stopping a job run
	// stops its running database and every database still waiting.
	Group string
}

// Registry tracks the backup and restore runs active in this process: it cancels
// them on request, owns their log files and serves their live progress. It is safe
// for concurrent use; a nil *Registry tracks nothing.
type Registry struct {
	logs   *runlog.Dir
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	active map[string]*Run
}

// RegistryOption customises a Registry.
type RegistryOption func(*Registry)

// WithLogs makes every registered run write its log to d.
func WithLogs(d *runlog.Dir) RegistryOption {
	return func(r *Registry) { r.logs = d }
}

// WithRegistryLogger sets the logger for log file errors.
func WithRegistryLogger(l *slog.Logger) RegistryOption {
	return func(r *Registry) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewRegistry returns an empty Registry.
func NewRegistry(opts ...RegistryOption) *Registry {
	r := &Registry{logger: slog.Default(), now: time.Now, active: make(map[string]*Run)}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Logs returns the log directory, or nil.
func (r *Registry) Logs() *runlog.Dir {
	if r == nil {
		return nil
	}
	return r.logs
}

// Register adds a run before it starts and opens its log. Bind the returned Run to
// the run's context when it starts and End it when it finishes (also when it never
// starts). A nil Registry returns a nil Run, whose methods do nothing.
func (r *Registry) Register(meta Meta) (*Run, error) {
	if r == nil {
		return nil, nil
	}
	run := &Run{meta: meta, reg: r, queuedAt: r.now().UTC(), phase: models.PhaseQueued, done: make(chan struct{})}
	run.updatedAt = run.queuedAt
	r.mu.Lock()
	if _, dup := r.active[meta.ID]; dup {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBusy, meta.ID)
	}
	r.active[meta.ID] = run
	r.mu.Unlock()
	if r.logs != nil {
		w, err := r.logs.Create(meta.ID)
		if err != nil {
			r.logger.Warn("cannot write the log of this run", logsafe.Attr("run_id", meta.ID), logsafe.Error(err))
		} else {
			run.log = w
		}
	}
	return run, nil
}

// Get returns the active run id, or nil.
func (r *Registry) Get(id string) *Run {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active[id]
}

// Cancel cancels the active run id. It returns ErrNotRunning when no such run is
// active. Cancelling a run twice keeps the first Cancellation. A run of a group
// (Meta.Group) cancels the other active runs of its group too, whatever its own
// outcome: a job run stops as a whole.
func (r *Registry) Cancel(id string, c Cancellation) error {
	run := r.Get(id)
	if run == nil {
		return fmt.Errorf("%w: %s", ErrNotRunning, id)
	}
	err := run.Cancel(c)
	if run.meta.Group != "" {
		r.cancelWhere(c, func(m Meta) bool { return m.Group == run.meta.Group && m.ID != id })
	}
	return err
}

// CancelGroup cancels every active run of group (a job run) and returns the IDs of
// the runs it cancelled.
func (r *Registry) CancelGroup(group string, c Cancellation) []string {
	if group == "" {
		return nil
	}
	return r.cancelWhere(c, func(m Meta) bool { return m.Group == group })
}

// CancelJob cancels every active backup of job jobID (every database of its current
// run) and returns the IDs of the runs it cancelled.
func (r *Registry) CancelJob(jobID string, c Cancellation) []string {
	if jobID == "" {
		return nil
	}
	return r.cancelWhere(c, func(m Meta) bool { return m.Kind == models.RunBackup && m.JobID == jobID })
}

// JobRuns returns the active backup runs of job jobID.
func (r *Registry) JobRuns(jobID string) []*Run {
	if r == nil || jobID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Run
	for _, run := range r.active {
		if run.meta.Kind == models.RunBackup && run.meta.JobID == jobID {
			out = append(out, run)
		}
	}
	return out
}

// cancelWhere cancels the active runs whose Meta match and returns the IDs of the
// runs it cancelled, sorted.
func (r *Registry) cancelWhere(c Cancellation, match func(Meta) bool) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	var list []*Run
	for _, run := range r.active {
		if match(run.meta) {
			list = append(list, run)
		}
	}
	r.mu.Unlock()
	var ids []string
	for _, run := range list {
		if newly, err := run.cancelRun(c); err == nil && newly {
			ids = append(ids, run.meta.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// CancelAll cancels every active run with c and returns the IDs of the runs it
// cancelled: runs already cancelled before, finishing or ended are not included.
func (r *Registry) CancelAll(c Cancellation) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	list := make([]*Run, 0, len(r.active))
	for _, run := range r.active {
		list = append(list, run)
	}
	r.mu.Unlock()
	var ids []string
	for _, run := range list {
		if newly, err := run.cancelRun(c); err == nil && newly {
			ids = append(ids, run.meta.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// Count returns the number of active runs of kind.
func (r *Registry) Count(kind models.RunKind) int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, run := range r.active {
		if run.meta.Kind == kind {
			n++
		}
	}
	return n
}

// Snapshots returns the progress of every active run, oldest first.
func (r *Registry) Snapshots() []models.RunProgress {
	if r == nil {
		return []models.RunProgress{}
	}
	r.mu.Lock()
	list := make([]*Run, 0, len(r.active))
	for _, run := range r.active {
		list = append(list, run)
	}
	r.mu.Unlock()
	out := make([]models.RunProgress, 0, len(list))
	for _, run := range list {
		out = append(out, run.Snapshot())
	}
	slices.SortFunc(out, func(a, b models.RunProgress) int {
		if c := a.StartedAt.Compare(b.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Progress returns the progress of the active run id, or nil.
func (r *Registry) Progress(id string) *models.RunProgress {
	run := r.Get(id)
	if run == nil {
		return nil
	}
	p := run.Snapshot()
	return &p
}

// OpenLog opens the log of run id: the live log of an active run, else the file. It
// returns runlog.ErrNotFound when the run has no log.
func (r *Registry) OpenLog(id string) (*runlog.Reader, error) {
	if r == nil || r.logs == nil {
		return nil, runlog.ErrNotFound
	}
	if run := r.Get(id); run != nil && run.log != nil {
		if rd, err := run.log.Reader(); err == nil {
			return rd, nil
		}
		// The run ended meanwhile: its file is complete.
	}
	return r.logs.Open(id)
}

// RemoveLog deletes the log of run id unless the run is active.
func (r *Registry) RemoveLog(id string) error {
	if r == nil || r.logs == nil || r.Get(id) != nil {
		return nil
	}
	return r.logs.Remove(id)
}

// PruneLogs deletes the logs of finished runs older than keep (see runlog.Dir.Prune).
func (r *Registry) PruneLogs(keep time.Duration) (int, error) {
	if r == nil || r.logs == nil {
		return 0, nil
	}
	return r.logs.Prune(keep, func(id string) bool { return r.Get(id) != nil })
}

// runKey is the context key of the Run a context belongs to.
type runKey struct{}

// FromContext returns the Run bound to ctx (see Run.Bind), or nil. The methods of a
// nil Run do nothing, so engines call them unconditionally.
func FromContext(ctx context.Context) *Run {
	run, _ := ctx.Value(runKey{}).(*Run)
	return run
}

// collState is the progress of one collection.
type collState struct {
	done, total float64
	bytes       bool
	finished    bool
	docs        int64
}

// Run is one active backup or restore. A nil *Run is valid and ignores every call.
type Run struct {
	meta     Meta
	reg      *Registry
	log      *runlog.Writer
	queuedAt time.Time
	done     chan struct{} // closed by End

	bytes atomic.Int64

	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelCauseFunc
	pending       *Cancellation
	cancellation  *Cancellation
	finishing     bool
	ended         bool
	startedAt     time.Time
	transferStart time.Time
	updatedAt     time.Time
	phase         string
	phases        models.RunPhases
	totalBytes    int64
	colls         map[string]*collState
	current       string
}

// ID returns the record ID of the run.
func (run *Run) ID() string {
	if run == nil {
		return ""
	}
	return run.meta.ID
}

// Bind returns a context derived from ctx that the Registry can cancel and that
// carries the run (see FromContext). A cancellation requested before Bind takes
// effect at once.
func (run *Run) Bind(ctx context.Context) context.Context {
	if run == nil {
		return ctx
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	runCtx = context.WithValue(runCtx, runKey{}, run)
	run.mu.Lock()
	run.cancel, run.ctx = cancel, runCtx
	now := run.reg.now().UTC()
	run.startedAt, run.transferStart, run.updatedAt = now, now, now
	pending := run.pending
	run.mu.Unlock()
	if pending != nil {
		cancel(pending)
	}
	return runCtx
}

// End unregisters the run and completes its log. It is idempotent.
func (run *Run) End() {
	if run == nil {
		return
	}
	run.mu.Lock()
	if run.ended {
		run.mu.Unlock()
		return
	}
	run.ended = true
	cancel := run.cancel
	run.mu.Unlock()
	if run.log != nil {
		if err := run.log.Close(); err != nil {
			run.reg.logger.Warn("failed to complete the log of this run", logsafe.Attr("run_id", run.meta.ID), slog.Any("error", err))
		}
	}
	run.reg.mu.Lock()
	if run.reg.active[run.meta.ID] == run {
		delete(run.reg.active, run.meta.ID)
	}
	run.reg.mu.Unlock()
	close(run.done)
	if cancel != nil {
		// Release the context's resources; the run is over.
		cancel(context.Canceled)
	}
}

// Done returns a channel closed once End ran: the run's outcome is recorded (the
// operations service and the scheduler persist the final record before End). A nil
// Run returns a closed channel.
func (run *Run) Done() <-chan struct{} {
	if run == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return run.done
}

// Cancel stops the run with c as its context's cause. It returns ErrNotRunning once
// the run ended and ErrFinishing once it is past the point of no return (see
// Finishing); a second cancellation keeps the first.
func (run *Run) Cancel(c Cancellation) error {
	_, err := run.cancelRun(c)
	return err
}

// cancelRun implements Cancel and reports whether this call cancelled the run.
func (run *Run) cancelRun(c Cancellation) (bool, error) {
	if run == nil {
		return false, ErrNotRunning
	}
	if c.At.IsZero() {
		c.At = run.reg.now().UTC()
	}
	run.mu.Lock()
	if run.ended {
		run.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrNotRunning, run.meta.ID)
	}
	if run.cancellation != nil {
		run.mu.Unlock()
		return false, nil
	}
	if run.finishing {
		run.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrFinishing, run.meta.ID)
	}
	if run.ctx != nil && run.ctx.Err() != nil {
		// The run already stopped for another reason (a timeout, the shutdown).
		run.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ErrNotRunning, run.meta.ID)
	}
	cause := &c
	run.cancellation = cause
	run.phase = models.PhaseCancelling
	run.updatedAt = c.At
	cancel := run.cancel
	if cancel == nil {
		run.pending = cause
	}
	run.mu.Unlock()
	run.Printf("cancellation requested (%s)", cause.Error())
	if cancel != nil {
		cancel(cause)
	}
	return true, nil
}

// Finishing marks the point of no return: the run's tool finished successfully and
// the run only records its outcome, so later cancellations fail with ErrFinishing
// (a cancellation requested before still counts). The phase becomes
// models.PhaseFinishing.
func (run *Run) Finishing() {
	if run == nil {
		return
	}
	run.mu.Lock()
	run.finishing = true
	if run.cancellation == nil {
		run.phase = models.PhaseFinishing
	}
	run.updatedAt = run.reg.now().UTC()
	run.mu.Unlock()
}

// Cancellation returns the run's Cancellation, or nil.
func (run *Run) Cancellation() *Cancellation {
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.cancellation
}

// Printf adds one of MongoRescue's own lines to the run's log.
func (run *Run) Printf(format string, args ...any) {
	if run == nil || run.log == nil {
		return
	}
	run.log.Printf(format, args...)
}

// Phase records the current phase and the phase timestamps reached so far.
func (run *Run) Phase(name string, phases models.RunPhases) {
	if run == nil {
		return
	}
	run.mu.Lock()
	if run.cancellation == nil {
		run.phase = name
	}
	run.phases = phases
	run.updatedAt = run.reg.now().UTC()
	run.mu.Unlock()
	run.Printf("phase: %s", name)
}

// StartTransfer resets the byte counter for a new streaming phase that reads or
// writes total bytes (0 when unknown).
func (run *Run) StartTransfer(total int64) {
	if run == nil {
		return
	}
	run.mu.Lock()
	run.bytes.Store(0)
	run.totalBytes = total
	run.transferStart = run.reg.now().UTC()
	run.mu.Unlock()
}

// AddBytes counts n bytes streamed in the current phase.
func (run *Run) AddBytes(n int) {
	if run == nil || n <= 0 {
		return
	}
	run.bytes.Add(int64(n))
}

// CountingReader wraps r so the bytes read through it are counted (see AddBytes).
func (run *Run) CountingReader(r io.Reader) io.Reader {
	if run == nil {
		return r
	}
	return &countingReader{r: r, run: run}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r   io.Reader
	run *Run
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.run.AddBytes(n)
	return n, err
}

// ToolOutput returns a writer for one stream of tool output (mongodump's or
// mongorestore's stderr): every line goes to the run's log and its progress is
// parsed. Close it at the end of the stream to flush a final partial line. Each
// stream needs its own writer; a writer is not safe for concurrent use.
func (run *Run) ToolOutput() io.WriteCloser {
	if run == nil {
		return nopWriteCloser{io.Discard}
	}
	return &toolOutput{run: run}
}

// nopWriteCloser adds a no-op Close to a writer.
type nopWriteCloser struct{ io.Writer }

// Close implements io.Closer.
func (nopWriteCloser) Close() error { return nil }

// toolLineMax bounds a tool line held while waiting for its newline.
const toolLineMax = 64 << 10

// toolOutput splits tool output into lines for the log and the progress parser.
type toolOutput struct {
	run     *Run
	pending []byte
	cut     bool
}

// Write implements io.Writer; it never fails.
func (t *toolOutput) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.buffer(p)
			break
		}
		t.buffer(p[:i])
		t.flush()
		p = p[i+1:]
	}
	return n, nil
}

// buffer appends part of a line, bounded by toolLineMax.
func (t *toolOutput) buffer(p []byte) {
	if t.cut {
		return
	}
	if room := toolLineMax - len(t.pending); len(p) > room {
		p, t.cut = p[:room], true
	}
	t.pending = append(t.pending, p...)
}

// flush hands the pending line to the log and the parser.
func (t *toolOutput) flush() {
	line := string(t.pending)
	t.pending, t.cut = t.pending[:0], false
	t.run.toolLine(line)
}

// Close flushes a final partial line.
func (t *toolOutput) Close() error {
	if len(t.pending) > 0 {
		t.flush()
	}
	return nil
}

// toolLine logs and parses one line of tool output.
func (run *Run) toolLine(line string) {
	if run.log != nil {
		_, _ = run.log.Write([]byte(line + "\n"))
	}
	p, ok := mongotools.ParseProgress(line)
	if !ok {
		return
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.colls == nil {
		run.colls = make(map[string]*collState)
	}
	c := run.colls[p.Namespace]
	if c == nil {
		c = &collState{}
		run.colls[p.Namespace] = c
	}
	switch p.Kind {
	case mongotools.ProgressStarted:
		run.current = p.Namespace
	case mongotools.ProgressBar:
		c.done, c.total, c.bytes = p.Done, p.Total, p.Bytes
		if !c.finished {
			run.current = p.Namespace
		}
	case mongotools.ProgressDone:
		c.finished, c.docs = true, p.Documents
		if !c.bytes {
			c.done = float64(p.Documents)
			c.total = max(c.total, c.done)
		}
		if run.current == p.Namespace {
			run.current = ""
		}
	}
	run.updatedAt = run.reg.now().UTC()
}

// Snapshot returns the run's progress.
func (run *Run) Snapshot() models.RunProgress {
	if run == nil {
		return models.RunProgress{}
	}
	now := run.reg.now().UTC()
	run.mu.Lock()
	defer run.mu.Unlock()
	p := models.RunProgress{
		ID: run.meta.ID, Kind: run.meta.Kind, JobID: run.meta.JobID, RunID: run.meta.Group, Database: run.meta.Database,
		Phase: run.phase, Bytes: run.bytes.Load(), TotalBytes: run.totalBytes,
		CurrentCollection: run.current, StartedAt: run.startedAt, UpdatedAt: run.updatedAt,
		Cancelling: run.cancellation != nil, Phases: run.phases,
	}
	if p.StartedAt.IsZero() {
		p.StartedAt = run.queuedAt
	}
	if p.Phases.Queued == nil {
		p.Phases.Queued = models.Stamp(run.queuedAt)
	}
	var docDone, docTotal, byteDone, byteTotal float64
	for _, c := range run.colls {
		p.CollectionsTotal++
		if c.finished {
			p.CollectionsDone++
		}
		if c.bytes {
			byteDone += c.done
			byteTotal += c.total
			p.Documents += c.docs
			continue
		}
		docDone += c.done
		docTotal += c.total
		p.Documents += int64(c.done)
	}
	switch {
	case run.totalBytes > 0:
		p.Percent = percent(float64(p.Bytes), float64(run.totalBytes))
	case docTotal > 0:
		p.Percent = percent(docDone, docTotal)
	case byteTotal > 0:
		p.Percent = percent(byteDone, byteTotal)
	}
	if !run.transferStart.IsZero() {
		if secs := now.Sub(run.transferStart).Seconds(); secs >= 1 {
			p.BytesPerSecond = float64(p.Bytes) / secs
		}
	}
	return p
}

// percent returns done/total as a percentage, capped at 100.
func percent(done, total float64) *float64 {
	v := min(done/total*100, 100)
	v = float64(int64(v*10)) / 10
	return &v
}
