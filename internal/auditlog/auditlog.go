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
	// maxCoalesceKeys bounds the in-memory index of coalesced refusals.
	maxCoalesceKeys = 4096
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
	// Logger reports write failures (slog.Default() when nil).
	Logger *slog.Logger
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Service records, lists, verifies, exports and prunes the audit log. It is safe for
// concurrent use; a nil *Service records nothing.
type Service struct {
	cfg Config

	// mu guards refusals.
	mu sync.Mutex
	// refusals indexes the latest stored refusal of every caller and how many
	// identical ones were counted since.
	refusals map[string]*refusal
}

// refusal is the coalescing state of one caller's identical refusals.
type refusal struct {
	since      time.Time
	suppressed int
}

// New returns a Service.
func New(cfg Config) *Service {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{cfg: cfg, refusals: map[string]*refusal{}}
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
// WithClient), chains and stores it and hands it to the forwarder. Identical
// refusals of one caller within CoalesceWindow are counted, not stored: the next
// stored one carries the count. Record never fails the caller: the write is
// detached from ctx's cancellation, bounded by a timeout, and errors are logged.
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
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := s.cfg.Repo.AppendAuditEvent(writeCtx, &e); err != nil {
		// The entry's fields stay out of the log: it is the audit log's job.
		s.cfg.Logger.Error("failed to write audit log entry", logsafe.Error(err))
		return
	}
	s.cfg.Forwarder.Enqueue(e)
}

// coalesce reports whether the refusal e is counted instead of stored; for a stored
// refusal it sets e.Count to include the ones counted since the previous one.
func (s *Service) coalesce(e *Event) bool {
	if e.Outcome != OutcomeDenied && e.Outcome != OutcomeRateLimited {
		return false
	}
	key := strings.Join([]string{e.ActorKind, e.ActorUserID, e.ActorName, e.ActorKeyID, e.ClientIP, e.Action, e.Outcome, fmt.Sprint(e.Status)}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.refusals[key]; ok {
		if !e.Time.Before(r.since) && e.Time.Sub(r.since) < CoalesceWindow {
			r.suppressed++
			return true
		}
		e.Count += r.suppressed
	}
	if len(s.refusals) >= maxCoalesceKeys {
		for k, r := range s.refusals {
			if e.Time.Sub(r.since) >= CoalesceWindow {
				delete(s.refusals, k)
			}
		}
		if len(s.refusals) >= maxCoalesceKeys {
			clear(s.refusals)
		}
	}
	s.refusals[key] = &refusal{since: e.Time}
	return false
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
	targets := make(map[string]string, len(e.Targets))
	for k, v := range e.Targets {
		k = clean(k, maxTargetKeyLength)
		if k == "" || len(targets) >= maxTargets {
			continue
		}
		targets[k] = clean(v, maxTargetValueLength)
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
	// Anchor is where the chain started.
	Anchor Anchor `json:"anchor"`
	// HeadID and HeadHash identify the newest entry checked (the anchor when the log
	// is empty); compare them with a copy kept elsewhere to detect removed tails.
	HeadID   int64  `json:"head_id"`
	HeadHash string `json:"head_hash"`
	// BrokenID is the first entry that does not verify (0 when OK).
	BrokenID int64 `json:"broken_id,omitempty"`
	// Reason says why it does not verify.
	Reason string `json:"reason,omitempty"`
	// VerifiedAt is when the check ran.
	VerifiedAt time.Time `json:"verified_at"`
}

// Verify walks the chain from the anchor and reports the first entry whose ID does
// not follow its predecessor's or whose hash does not match its content chained to
// the predecessor's hash.
func (s *Service) Verify(ctx context.Context) (Verification, error) {
	if s == nil || s.cfg.Repo == nil {
		return Verification{}, ErrNoRepository
	}
	anchor, err := s.cfg.Repo.AuditChainAnchor(ctx)
	if err != nil {
		return Verification{}, err
	}
	v := Verification{Anchor: anchor, HeadID: anchor.LastID, HeadHash: anchor.LastHash, VerifiedAt: s.cfg.Now().UTC()}
	f := Filter{Ascending: true, AfterID: anchor.LastID, Limit: walkBatch}
	for {
		list, err := s.cfg.Repo.ListAuditEvents(ctx, f)
		if err != nil {
			return Verification{}, err
		}
		for _, e := range list {
			if reason := s.check(v.HeadID, v.HeadHash, e); reason != "" {
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
func (s *Service) check(prevID int64, prevHash string, e *Event) string {
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

// Prune removes the entries older than the retention and returns how many.
func (s *Service) Prune(ctx context.Context) (int64, error) {
	if s == nil || s.cfg.Repo == nil {
		return 0, ErrNoRepository
	}
	cutoff := s.cfg.Now().UTC().AddDate(0, 0, -s.retentionDays())
	return s.cfg.Repo.PruneAuditEvents(ctx, cutoff)
}

// RunRetention prunes now and then every interval until ctx ends.
func (s *Service) RunRetention(ctx context.Context, interval time.Duration) {
	if s == nil || s.cfg.Repo == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if n, err := s.Prune(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("failed to prune the audit log", logsafe.Error(err))
		} else if n > 0 {
			s.cfg.Logger.Info("expired audit log entries removed", slog.Int64("removed", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Forwarding returns the forwarder's status (zero without a forwarder).
func (s *Service) Forwarding() ForwardStatus {
	if s == nil {
		return ForwardStatus{}
	}
	return s.cfg.Forwarder.Status()
}
