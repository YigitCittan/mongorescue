// Package auditlog is the tamper-evident log of every action: each mutating REST
// request (by a dashboard session or an API key), sign-ins and failed sign-ins,
// sign-outs, the completion of setup, audit log exports, MCP tool calls and actions
// MongoRescue takes on its own (retention). An entry records who acted (actor kind,
// user, API key), the route pattern, the path parameters it targeted, the HTTP
// status and outcome, the client address and a truncated user agent. Request bodies
// are never stored.
//
// The log is append-only and hash-chained: every entry stores
// SHA-256(previous hash || canonical JSON of the entry), see ChainHash, so changing,
// removing or reordering an entry breaks the chain at that entry, which Verify
// reports. Retention removes the oldest entries and keeps the last removed hash as
// the chain's anchor, so verification still starts from a known hash. Entries can be
// forwarded to a webhook as they are written (see Forwarder).
//
// The more detailed activity log of API keys and MCP clients, with redacted
// arguments, stays in internal/audit; MCP tool calls and system actions recorded
// there are mirrored into this log by FromActivity.
//
// The package is the domain core; persistence is the Repository port implemented by
// internal/store. Recording never fails the audited operation: write errors are
// logged, not returned.
package auditlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yigitcittan/mongorescue/internal/logsafe"
)

// Actor kinds.
const (
	// ActorUser is a signed-in dashboard user (session).
	ActorUser = "user"
	// ActorAPIKey is a client authenticated by an API key (REST or MCP).
	ActorAPIKey = "api_key"
	// ActorSystem is MongoRescue acting on its own (for example retention).
	ActorSystem = "system"
	// ActorAnonymous is a caller that is not signed in (a failed sign-in, setup).
	ActorAnonymous = "anonymous"
)

// Outcomes.
const (
	// OutcomeOK is an action that succeeded (HTTP status below 400).
	OutcomeOK = "ok"
	// OutcomeError is an action that failed (any other 4xx or 5xx status).
	OutcomeError = "error"
	// OutcomeDenied is an action refused for missing or wrong credentials or a too
	// small scope (401, 403).
	OutcomeDenied = "denied"
	// OutcomeRateLimited is an action refused by throttling (429).
	OutcomeRateLimited = "rate_limited"
)

// Limits.
const (
	// DefaultListLimit is the page size of List.
	DefaultListLimit = 100
	// MaxListLimit caps the page size of List.
	MaxListLimit = 1000
	// MinRetentionDays is the shortest retention the settings allow.
	MinRetentionDays = 30
	// DefaultRetentionDays is the retention of a fresh installation.
	DefaultRetentionDays = 365
	// CoalesceWindow is how long repeated identical refusals (denied, rate limited)
	// are counted instead of stored one by one.
	CoalesceWindow = 10 * time.Second
	// MaxUserAgentLength caps the stored user agent, in bytes.
	MaxUserAgentLength = 256
	// maxNameLength caps actor names and IDs, in bytes.
	maxNameLength = 128
	// maxActionLength caps the action, in bytes.
	maxActionLength = 256
	// maxTargets caps the number of targets of an entry.
	maxTargets = 16
	// maxTargetKeyLength and maxTargetValueLength cap one target.
	maxTargetKeyLength   = 64
	maxTargetValueLength = 256
	// maxIPLength caps the client address.
	maxIPLength = 64
	// writeTimeout bounds one audit write.
	writeTimeout = 5 * time.Second
	// walkBatch is the page size of Verify and Export.
	walkBatch = 500
	// maxCoalesceKeys bounds the in-memory index of coalesced refusals; reaching it
	// flushes every open window.
	maxCoalesceKeys = 4096
	// MaxSummaryNames is how many distinct attempted names a summary entry keeps.
	MaxSummaryNames = 10
	// maxSummaryNameLength caps one attempted name in a summary entry, in bytes.
	maxSummaryNameLength = 64
	// DefaultWriteQueueSize bounds the entries waiting for the writer; when it is full,
	// Record writes synchronously instead.
	DefaultWriteQueueSize = 4096
	// flushInterval is how often closed coalescing windows are written.
	flushInterval = time.Second
	// DefaultPruneInterval is how often Run applies the retention.
	DefaultPruneInterval = time.Hour
	// retentionSlack is the clock tolerance of the anchor check in Verify.
	retentionSlack = 24 * time.Hour
)

// Errors.
var (
	// ErrNoRepository is returned when the service has no repository.
	ErrNoRepository = errors.New("auditlog: no repository configured")
	// ErrInvalidFilter is returned for a filter that cannot be applied.
	ErrInvalidFilter = errors.New("auditlog: invalid filter")
)

// Event is one entry of the audit log.
type Event struct {
	// ID orders the entries; the repository assigns it (the previous ID + 1).
	ID int64 `json:"id"`
	// Time is when the action was recorded (UTC).
	Time time.Time `json:"time"`
	// ActorKind is ActorUser, ActorAPIKey, ActorSystem or ActorAnonymous.
	ActorKind string `json:"actor_kind"`
	// ActorUserID is the acting user's ID (sessions, and API keys created by a user).
	ActorUserID string `json:"actor_user_id"`
	// ActorName is a snapshot of the acting user's name; for a failed sign-in the
	// name that was tried, for system actions the component.
	ActorName string `json:"actor_name"`
	// ActorKeyID identifies the API key the action was taken with.
	ActorKeyID string `json:"actor_key_id"`
	// ActorKeyName is a snapshot of that key's name.
	ActorKeyName string `json:"actor_key_name"`
	// Action is the route pattern ("POST /api/v1/jobs/{id}/run"), "MCP <tool>" for
	// MCP tool calls or "SYSTEM <action>" for system actions.
	Action string `json:"action"`
	// Targets are the path parameters of the route ({"id": "job_1"}) or the IDs a
	// tool call or system action named.
	Targets map[string]string `json:"targets"`
	// Status is the HTTP status of the response (0 when there is none).
	Status int `json:"status"`
	// Outcome is OutcomeOK, OutcomeError, OutcomeDenied or OutcomeRateLimited.
	Outcome string `json:"outcome"`
	// ClientIP is the caller's address (forwarding headers only from a trusted
	// proxy, see security.trust_proxy_headers).
	ClientIP string `json:"client_ip"`
	// UserAgent is the caller's User-Agent, cut at MaxUserAgentLength bytes.
	UserAgent string `json:"user_agent"`
	// Count is the number of actions the entry stands for: 1, or for a refusal
	// also the identical refusals that were counted instead of stored since the
	// previous entry of the same caller (see CoalesceWindow).
	Count int `json:"count"`
	// Hash is ChainHash of the entry chained to the previous one.
	Hash string `json:"hash"`
}

// Filter selects entries.
type Filter struct {
	// Actor matches the actor's user ID, name, API key ID or API key name exactly.
	Actor string
	// ActorKind matches the actor kind.
	ActorKind string
	// Action matches entries whose action contains it (case-insensitive for ASCII).
	Action string
	// Outcome matches the outcome.
	Outcome string
	// Since keeps entries at or after it; Until those before it.
	Since, Until time.Time
	// BeforeID keeps entries with a smaller ID (newest-first pages); AfterID those
	// with a larger one (oldest-first walks).
	BeforeID, AfterID int64
	// Ascending orders the result oldest first; the default is newest first.
	Ascending bool
	// Limit caps the number of entries returned.
	Limit int
}

// Anchor is where the chain starts: the last entry retention removed, or ID 0 with
// GenesisHash when nothing was removed.
type Anchor struct {
	// LastID is the ID of the last removed entry.
	LastID int64 `json:"last_id"`
	// LastHash is its hash; the first kept entry chains to it.
	LastHash string `json:"last_hash"`
	// LastTime is when that entry was recorded (zero when nothing was removed).
	LastTime time.Time `json:"last_time,omitzero"`
	// PrunedAt is when retention last removed entries (zero when never).
	PrunedAt time.Time `json:"pruned_at,omitzero"`
}

// Repository is the persistence port of the audit log. It inserts and reads; the
// only removal is Prune.
type Repository interface {
	// AppendAuditEvent assigns e the next ID, sets e.Hash to ChainHash of e chained to
	// the newest stored entry (or the anchor) and stores it, in one transaction.
	AppendAuditEvent(ctx context.Context, e *Event) error
	// ListAuditEvents returns the entries matching f (f.Limit of them, at most).
	ListAuditEvents(ctx context.Context, f Filter) ([]*Event, error)
	// AuditChainAnchor returns the anchor.
	AuditChainAnchor(ctx context.Context) (Anchor, error)
	// PruneAuditEvents removes the entries recorded before cutoff (and every older
	// ID) and moves the anchor to the newest removed one, in one transaction. It
	// returns the number of removed entries.
	PruneAuditEvents(ctx context.Context, cutoff time.Time) (int64, error)
}

// Config configures a Service.
type Config struct {
	// Repo stores the entries.
	Repo Repository
	// RetentionDays returns audit.retention_days (DefaultRetentionDays when nil;
	// values below MinRetentionDays are raised to it).
	RetentionDays func() int
	// Forwarder, when set, receives every stored entry.
	Forwarder *Forwarder
	// QueueSize bounds the write queue (DefaultWriteQueueSize when <= 0).
	QueueSize int
	// PruneInterval is how often Run applies the retention (DefaultPruneInterval
	// when <= 0).
	PruneInterval time.Duration
	// OnWriteFailure is called for every entry that could not be stored.
	OnWriteFailure func()
	// OnSyncWrite is called for every entry written on the caller's goroutine
	// because the write queue was full.
	OnSyncWrite func()
	// Logger reports write failures (slog.Default() when nil).
	Logger *slog.Logger
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Service records, lists, verifies, exports and prunes the audit log. While Run is
// running, entries are written by its single writer goroutine through a bounded
// queue, off the request path; when the queue is full, or Run is not running,
// Record writes synchronously instead, so no entry is ever dropped. It is safe for
// concurrent use; a nil *Service records nothing.
type Service struct {
	cfg    Config
	writes chan Event

	// gate guards running: Record enqueues only while Run accepts entries, so
	// nothing is enqueued after Run's final drain.
	gate    sync.RWMutex
	running bool

	// mu guards windows.
	mu sync.Mutex
	// windows are the open coalescing windows of refusals, by caller (see
	// coalesce.go).
	windows map[string]*window
}

// New returns a Service. Start its writer, coalescing flushes and retention with
// Run.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultWriteQueueSize
	}
	if cfg.PruneInterval <= 0 {
		cfg.PruneInterval = DefaultPruneInterval
	}
	return &Service{cfg: cfg, writes: make(chan Event, cfg.QueueSize), windows: map[string]*window{}}
}

// OutcomeFor maps an HTTP status to an outcome.
func OutcomeFor(status int) string {
	switch {
	case status == 401 || status == 403:
		return OutcomeDenied
	case status == 429:
		return OutcomeRateLimited
	case status >= 400:
		return OutcomeError
	default:
		return OutcomeOK
	}
}

// Record normalises e (time, lengths, control characters, the client from ctx, see
// WithClient) and queues it to be chained and stored, then forwarded. Identical
// refusals of one caller within CoalesceWindow are counted into a summary entry
// written when the window closes (see coalesce.go). Record never fails the caller:
// errors are logged and counted (Config.OnWriteFailure).
func (s *Service) Record(ctx context.Context, e Event) {
	if s == nil || s.cfg.Repo == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = s.cfg.Now()
	}
	if c, ok := ClientFrom(ctx); ok {
		if e.ClientIP == "" {
			e.ClientIP = c.IP
		}
		if e.UserAgent == "" {
			e.UserAgent = c.UserAgent
		}
	}
	normalize(&e)
	if s.coalesce(&e) {
		return
	}
	s.submit(e)
}

// submit hands e to the writer, or writes it at once when the writer is not
// running or its queue is full.
func (s *Service) submit(e Event) {
	s.gate.RLock()
	if s.running {
		select {
		case s.writes <- e:
			s.gate.RUnlock()
			return
		default:
		}
		s.gate.RUnlock()
		if s.cfg.OnSyncWrite != nil {
			s.cfg.OnSyncWrite()
		}
		s.write(e)
		return
	}
	s.gate.RUnlock()
	s.write(e)
}

// write chains and stores e and forwards it, logging and counting failures.
func (s *Service) write(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := s.cfg.Repo.AppendAuditEvent(ctx, &e); err != nil {
		// The entry's fields stay out of the log: it is the audit log's job.
		s.cfg.Logger.Error("failed to write audit log entry", logsafe.Error(err))
		if s.cfg.OnWriteFailure != nil {
			s.cfg.OnWriteFailure()
		}
		return
	}
	s.cfg.Forwarder.Enqueue(e)
}

// QueueDepth returns the number of entries waiting for the writer.
func (s *Service) QueueDepth() int {
	if s == nil {
		return 0
	}
	return len(s.writes)
}

// Run writes queued entries, writes the summaries of closed coalescing windows
// every second and applies the retention at start and every PruneInterval, until
// ctx ends. It then writes the summaries of every open window and drains the queue
// before returning, so no entry recorded before the end is lost.
func (s *Service) Run(ctx context.Context) {
	if s == nil || s.cfg.Repo == nil {
		return
	}
	s.gate.Lock()
	s.running = true
	s.gate.Unlock()

	var wg sync.WaitGroup
	wg.Go(func() { s.maintain(ctx) })
	for done := false; !done; {
		select {
		case e := <-s.writes:
			s.write(e)
		case <-ctx.Done():
			done = true
		}
	}
	wg.Wait()
	s.flushRefusals(time.Time{}, true)

	s.gate.Lock()
	s.running = false
	s.gate.Unlock()
	for {
		select {
		case e := <-s.writes:
			s.write(e)
		default:
			return
		}
	}
}

// maintain flushes closed coalescing windows and applies the retention until ctx
// ends.
func (s *Service) maintain(ctx context.Context) {
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()
	prune := time.NewTicker(s.cfg.PruneInterval)
	defer prune.Stop()
	s.pruneLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			s.flushRefusals(s.cfg.Now(), false)
		case <-prune.C:
			s.pruneLogged(ctx)
		}
	}
}

// pruneLogged applies the retention, logging the outcome.
func (s *Service) pruneLogged(ctx context.Context) {
	if n, err := s.Prune(ctx); err != nil && ctx.Err() == nil {
		s.cfg.Logger.Warn("failed to prune the audit log", logsafe.Error(err))
	} else if n > 0 {
		s.cfg.Logger.Info("expired audit log entries removed", slog.Int64("removed", n))
	}
}

// normalize makes e storable: UTC time with its monotonic reading dropped, bounded
// lengths, valid UTF-8 without control characters, a known actor kind and outcome,
// and a count of at least 1.
func normalize(e *Event) {
	e.Time = e.Time.UTC().Round(0)
	e.ActorKind = clean(e.ActorKind, maxNameLength)
	switch e.ActorKind {
	case ActorUser, ActorAPIKey, ActorSystem, ActorAnonymous:
	default:
		e.ActorKind = ActorAnonymous
	}
	e.ActorUserID = clean(e.ActorUserID, maxNameLength)
	e.ActorName = clean(e.ActorName, maxNameLength)
	e.ActorKeyID = clean(e.ActorKeyID, maxNameLength)
	e.ActorKeyName = clean(e.ActorKeyName, maxNameLength)
	e.Action = clean(e.Action, maxActionLength)
	e.ClientIP = clean(e.ClientIP, maxIPLength)
	e.UserAgent = clean(e.UserAgent, MaxUserAgentLength)
	if e.Outcome == "" {
		e.Outcome = OutcomeFor(e.Status)
	}
	e.Outcome = clean(e.Outcome, maxNameLength)
	keys := make([]string, 0, len(e.Targets))
	for k := range e.Targets {
		keys = append(keys, k)
	}
	sort.Strings(keys) // which targets survive the cap must not depend on map order
	targets := make(map[string]string, len(e.Targets))
	for _, k := range keys {
		ck := clean(k, maxTargetKeyLength)
		if ck == "" || len(targets) >= maxTargets {
			continue
		}
		targets[ck] = clean(e.Targets[k], maxTargetValueLength)
	}
	e.Targets = targets
	e.Count = max(e.Count, 1)
	e.ID, e.Hash = 0, ""
}

// clean returns s as valid UTF-8 without control characters (and without U+2028 and
// U+2029, which JSON encoders escape differently), cut at n bytes on a rune
// boundary.
func clean(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Page is one page of List.
type Page struct {
	// Events are the entries, newest first.
	Events []*Event `json:"events"`
	// NextBeforeID is the before_id of the next page; 0 when this is the last one.
	NextBeforeID int64 `json:"next_before_id,omitempty"`
}

// List returns one page of entries matching f, newest first (DefaultListLimit for a
// Limit <= 0, at most MaxListLimit).
func (s *Service) List(ctx context.Context, f Filter) (Page, error) {
	if s == nil || s.cfg.Repo == nil {
		return Page{}, ErrNoRepository
	}
	if err := checkFilter(f); err != nil {
		return Page{}, err
	}
	switch {
	case f.Limit <= 0:
		f.Limit = DefaultListLimit
	case f.Limit > MaxListLimit:
		f.Limit = MaxListLimit
	}
	f.Ascending, f.AfterID = false, 0
	f.Limit++ // one more tells whether there is a next page
	list, err := s.cfg.Repo.ListAuditEvents(ctx, f)
	if err != nil {
		return Page{}, err
	}
	page := Page{Events: list}
	if len(list) == f.Limit {
		page.Events = list[:f.Limit-1]
		page.NextBeforeID = page.Events[len(page.Events)-1].ID
	}
	if page.Events == nil {
		page.Events = []*Event{}
	}
	return page, nil
}

// checkFilter validates the values of f that come from clients.
func checkFilter(f Filter) error {
	switch {
	case f.BeforeID < 0 || f.AfterID < 0:
		return fmt.Errorf("%w: IDs must be positive", ErrInvalidFilter)
	case !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since):
		return fmt.Errorf("%w: until must be after since", ErrInvalidFilter)
	case len(f.Actor) > 256 || len(f.Action) > 256 || len(f.ActorKind) > 64 || len(f.Outcome) > 64:
		return fmt.Errorf("%w: a filter value is too long", ErrInvalidFilter)
	}
	return nil
}

// Export calls fn for every entry matching f, oldest first, reading the log in
// batches so that no read holds the database for long. Pagination fields of f are
// ignored. It stops at the first error of fn.
func (s *Service) Export(ctx context.Context, f Filter, fn func(*Event) error) error {
	if s == nil || s.cfg.Repo == nil {
		return ErrNoRepository
	}
	if err := checkFilter(f); err != nil {
		return err
	}
	f.Ascending, f.BeforeID, f.AfterID, f.Limit = true, 0, 0, walkBatch
	for {
		list, err := s.cfg.Repo.ListAuditEvents(ctx, f)
		if err != nil {
			return err
		}
		for _, e := range list {
			if err := fn(e); err != nil {
				return err
			}
		}
		if len(list) < walkBatch {
			return nil
		}
		f.AfterID = list[len(list)-1].ID
	}
}

// Verification is the result of Verify.
type Verification struct {
	// OK reports that every entry chains to its predecessor.
	OK bool `json:"ok"`
	// Checked is the number of entries checked.
	Checked int64 `json:"checked"`
	// Anchor is where the chain started, with when retention last pruned.
	Anchor Anchor `json:"anchor"`
	// HeadID and HeadHash identify the newest entry checked (the anchor when the log
	// is empty); compare them with a copy kept elsewhere to detect removed tails.
	HeadID   int64  `json:"head_id"`
	HeadHash string `json:"head_hash"`
	// BrokenID is the first entry that does not verify (0 when OK).
	BrokenID int64 `json:"broken_id,omitempty"`
	// Reason says why it does not verify.
	Reason string `json:"reason,omitempty"`
	// Warning reports a chain that verifies but whose anchor moved further than the
	// retention explains: the last removed entry is newer than now minus the
	// retention (with a day of tolerance), so entries may have been removed early.
	Warning string `json:"warning,omitempty"`
	// RetentionDays is the retention the anchor was checked against.
	RetentionDays int `json:"retention_days"`
	// VerifiedAt is when the check ran.
	VerifiedAt time.Time `json:"verified_at"`
}

// Verify walks the chain from the anchor and reports the first entry whose ID does
// not follow its predecessor's or whose hash does not match its content chained to
// the predecessor's hash. It also checks the anchor against the retention (see
// Verification.Warning).
func (s *Service) Verify(ctx context.Context) (Verification, error) {
	if s == nil || s.cfg.Repo == nil {
		return Verification{}, ErrNoRepository
	}
	anchor, err := s.cfg.Repo.AuditChainAnchor(ctx)
	if err != nil {
		return Verification{}, err
	}
	now := s.cfg.Now().UTC()
	days := s.retentionDays()
	v := Verification{Anchor: anchor, HeadID: anchor.LastID, HeadHash: anchor.LastHash, RetentionDays: days, VerifiedAt: now}
	if limit := now.AddDate(0, 0, -days).Add(retentionSlack); anchor.LastID > 0 && anchor.LastTime.After(limit) {
		v.Warning = fmt.Sprintf("the last removed entry (#%d) was recorded at %s, less than the retention of %d days ago: entries were removed earlier than the retention explains, or the retention was raised since",
			anchor.LastID, FormatTime(anchor.LastTime), days)
	}
	f := Filter{Ascending: true, AfterID: anchor.LastID, Limit: walkBatch}
	for {
		list, err := s.cfg.Repo.ListAuditEvents(ctx, f)
		if err != nil {
			return Verification{}, err
		}
		for _, e := range list {
			if reason := check(v.HeadID, v.HeadHash, e); reason != "" {
				v.BrokenID, v.Reason = e.ID, reason
				return v, nil
			}
			v.Checked++
			v.HeadID, v.HeadHash = e.ID, e.Hash
		}
		if len(list) < walkBatch {
			v.OK = true
			return v, nil
		}
		f.AfterID = list[len(list)-1].ID
	}
}

// check returns why e does not follow the entry (prevID, prevHash), or "".
func check(prevID int64, prevHash string, e *Event) string {
	if e.ID != prevID+1 {
		return fmt.Sprintf("entry %d follows entry %d: entries %d to %d are missing", e.ID, prevID, prevID+1, e.ID-1)
	}
	want, err := ChainHash(prevHash, e)
	if err != nil {
		return "the entry cannot be encoded"
	}
	if want != e.Hash {
		return "the hash does not match the entry's content and its predecessor's hash: the entry, or the one before it, was changed"
	}
	return ""
}

// retentionDays returns the effective retention.
func (s *Service) retentionDays() int {
	days := DefaultRetentionDays
	if s.cfg.RetentionDays != nil {
		days = s.cfg.RetentionDays()
	}
	return max(days, MinRetentionDays)
}

// PruneAction is the action of the system entry retention writes after pruning.
const PruneAction = "SYSTEM audit.prune"

// Prune removes the entries older than the retention and returns how many. When it
// removed any, it records a system entry with the count and the new anchor.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.cfg.Repo == nil {
		return 0, ErrNoRepository
	}
	cutoff := s.cfg.Now().UTC().AddDate(0, 0, -s.retentionDays())
	n, err := s.cfg.Repo.PruneAuditEvents(ctx, cutoff)
	if err != nil || n == 0 {
		return n, err
	}
	targets := map[string]string{"removed": strconv.FormatInt(n, 10)}
	if anchor, aerr := s.cfg.Repo.AuditChainAnchor(ctx); aerr == nil {
		targets["anchor_id"] = strconv.FormatInt(anchor.LastID, 10)
		targets["anchor_hash"] = anchor.LastHash
	}
	s.Record(ctx, Event{ActorKind: ActorSystem, ActorName: "retention", Action: PruneAction, Targets: targets, Outcome: OutcomeOK})
	return n, nil
}

// Forwarding returns the forwarder's status (zero without a forwarder).
func (s *Service) Forwarding() ForwardStatus {
	if s == nil {
		return ForwardStatus{}
	}
	return s.cfg.Forwarder.Status()
}
