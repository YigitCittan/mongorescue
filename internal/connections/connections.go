// Package connections manages the MongoDB servers MongoRescue backs up from and
// restores into: validation, secret-preserving updates, connectivity tests and
// database/collection discovery.
//
// The package is the domain core; persistence (Repository) and the MongoDB driver
// (Prober) are ports implemented by internal/store and internal/mongoconn.
package connections

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongouri"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Sentinel errors.
var (
	// ErrNotFound is returned when a connection ID does not exist.
	ErrNotFound = errors.New("connections: connection not found")
	// ErrInUse is returned when deleting a connection that jobs still reference.
	ErrInUse = errors.New("connections: connection is used by jobs")
	// ErrInvalid is returned for invalid connection input (name, URI, description).
	ErrInvalid = errors.New("connections: invalid connection")
	// ErrMaskedURI is returned when a URI carries the redaction placeholder and it is not
	// exactly the redacted form of the stored URI.
	ErrMaskedURI = errors.New("connections: uri contains a redacted password; supply the full URI")
	// ErrUnavailable is returned when the server cannot be reached for discovery.
	ErrUnavailable = errors.New("connections: server unavailable")
)

// DefaultTestTimeout bounds connection tests and discovery calls.
const DefaultTestTimeout = 10 * time.Second

// systemDatabases are hidden from database listings unless explicitly requested.
var systemDatabases = []string{"admin", "config", "local"}

// Limits for user-supplied fields.
const (
	maxNameLength        = 100
	maxDescriptionLength = 500
)

// Repository is the persistence port for connections. Implementations store the URI
// encrypted and return copies callers may mutate.
type Repository interface {
	// ListConnections returns all connections sorted by name.
	ListConnections(ctx context.Context) ([]*models.Connection, error)
	// GetConnection returns a connection or ErrNotFound.
	GetConnection(ctx context.Context, id string) (*models.Connection, error)
	// SaveConnection creates or replaces a connection.
	SaveConnection(ctx context.Context, c *models.Connection) error
	// DeleteConnection removes a connection, returning ErrNotFound or, atomically,
	// ErrInUse when a job references it.
	DeleteConnection(ctx context.Context, id string) error
}

// ServerInfo is the outcome of a successful ping.
type ServerInfo struct {
	// Version is the MongoDB server version.
	Version string
}

// Database describes one database of a server.
type Database struct {
	// Name is the database name.
	Name string `json:"name"`
	// SizeBytes is the on-disk size reported by the server.
	SizeBytes int64 `json:"size_bytes"`
	// Empty reports whether the database holds no data.
	Empty bool `json:"empty"`
}

// Collection describes one collection or view.
type Collection struct {
	// Name is the collection name.
	Name string `json:"name"`
	// Type is "collection", "view" or "timeseries".
	Type string `json:"type"`
}

// Prober talks to a MongoDB server. Implementations must honour ctx deadlines and
// must never include the URI's credentials in returned errors.
type Prober interface {
	// Ping connects and returns the server version.
	Ping(ctx context.Context, uri string) (ServerInfo, error)
	// ListDatabases returns all databases of the server.
	ListDatabases(ctx context.Context, uri string) ([]Database, error)
	// ListCollections returns the collections and views of database.
	ListCollections(ctx context.Context, uri, database string) ([]Collection, error)
}

// TestResult is the outcome of a connection test.
type TestResult struct {
	// OK reports whether the server answered.
	OK bool `json:"ok"`
	// ServerVersion is the reported MongoDB version on success.
	ServerVersion string `json:"server_version,omitempty"`
	// LatencyMS is the round-trip time of the test in milliseconds.
	LatencyMS int64 `json:"latency_ms"`
	// Error is the redacted failure reason.
	Error string `json:"error,omitempty"`
}

// Input is the client-editable part of a connection.
type Input struct {
	// Name is the display name (required).
	Name string `json:"name"`
	// URI is the connection string; on update, the exact redacted form of the stored
	// URI keeps the stored credentials.
	URI string `json:"uri"`
	// Description is optional.
	Description string `json:"description"`
}

// Service implements the connection use cases. It is safe for concurrent use.
type Service struct {
	repo        Repository
	prober      Prober
	logger      *slog.Logger
	testTimeout time.Duration
	now         func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.logger = l } }

// WithTestTimeout overrides DefaultTestTimeout.
func WithTestTimeout(d time.Duration) Option { return func(s *Service) { s.testTimeout = d } }

// WithClock overrides the time source (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// NewService returns a Service. prober may be nil, in which case tests and discovery
// report ErrUnavailable.
func NewService(repo Repository, prober Prober, opts ...Option) *Service {
	s := &Service{repo: repo, prober: prober, logger: slog.Default(), testTimeout: DefaultTestTimeout, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// List returns all connections with redacted URIs.
func (s *Service) List(ctx context.Context) ([]*models.Connection, error) {
	list, err := s.repo.ListConnections(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*models.Connection, 0, len(list))
	for _, c := range list {
		out = append(out, c.Redacted())
	}
	return out, nil
}

// Get returns one connection with a redacted URI.
func (s *Service) Get(ctx context.Context, id string) (*models.Connection, error) {
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.Redacted(), nil
}

// Resolve returns the connection with its full URI, for the backup and restore engines.
// The result must never be serialised to clients or logged.
func (s *Service) Resolve(ctx context.Context, id string) (*models.Connection, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: connection_id is required", ErrInvalid)
	}
	return s.repo.GetConnection(ctx, id)
}

// Create validates in and stores a new connection. It returns the redacted result.
func (s *Service) Create(ctx context.Context, in Input) (*models.Connection, error) {
	if err := validateInput(&in); err != nil {
		return nil, err
	}
	if strings.Contains(in.URI, redact.Mask) {
		return nil, ErrMaskedURI
	}
	if err := mongouri.Validate(in.URI); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	c := &models.Connection{ID: id, Name: in.Name, URI: in.URI, Description: in.Description, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.SaveConnection(ctx, c); err != nil {
		return nil, err
	}
	s.logger.Info("connection created", slog.String("connection_id", c.ID), slog.String("uri", redact.URI(c.URI)))
	return c.Redacted(), nil
}

// Update replaces the editable fields of connection id. A URI equal to the redacted
// form of the stored URI keeps the stored credentials; any other masked URI is
// rejected with ErrMaskedURI. Changing the URI clears the last test result.
func (s *Service) Update(ctx context.Context, id string, in Input) (*models.Connection, error) {
	existing, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = validateInput(&in); err != nil {
		return nil, err
	}
	uri, err := KeepSecret(in.URI, existing.URI)
	if err != nil {
		return nil, err
	}
	if err := mongouri.Validate(uri); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	updated := *existing
	if uri != existing.URI {
		updated.LastTestAt, updated.LastTestOK, updated.LastTestError, updated.ServerVersion = nil, false, "", ""
	}
	updated.Name, updated.URI, updated.Description = in.Name, uri, in.Description
	updated.UpdatedAt = s.now().UTC()
	if err := s.repo.SaveConnection(ctx, &updated); err != nil {
		return nil, err
	}
	return updated.Redacted(), nil
}

// KeepSecret resolves an incoming URI against the stored one: the exact redacted form
// of stored stands for stored; any other value containing the mask is rejected.
func KeepSecret(incoming, stored string) (string, error) {
	if !strings.Contains(incoming, redact.Mask) {
		return incoming, nil
	}
	if stored == "" || redact.URI(stored) != incoming {
		return "", ErrMaskedURI
	}
	return stored, nil
}

// Delete removes a connection; it returns ErrInUse while jobs reference it.
func (s *Service) Delete(ctx context.Context, id string) error {
	return s.repo.DeleteConnection(ctx, id)
}

// Test pings the stored connection and records the outcome on it.
func (s *Service) Test(ctx context.Context, id string) (TestResult, error) {
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return TestResult{}, err
	}
	res := s.probe(ctx, c.URI)

	now := s.now().UTC()
	c.LastTestAt, c.LastTestOK, c.LastTestError = &now, res.OK, res.Error
	if res.OK {
		c.ServerVersion = res.ServerVersion
	}
	// Record the result even if the client went away meanwhile.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.repo.SaveConnection(saveCtx, c); err != nil && !errors.Is(err, ErrNotFound) {
		s.logger.Warn("failed to record connection test result", slog.String("connection_id", id), slog.Any("error", err))
	}
	return res, nil
}

// TestURI pings an unsaved URI (from the connection form). When id is non-empty and
// uri is the redacted form of that connection's stored URI, the stored URI is used, so
// an edit form can be re-tested without re-entering the password.
func (s *Service) TestURI(ctx context.Context, uri, id string) (TestResult, error) {
	if id != "" && strings.Contains(uri, redact.Mask) {
		existing, err := s.repo.GetConnection(ctx, id)
		if err != nil {
			return TestResult{}, err
		}
		if uri, err = KeepSecret(uri, existing.URI); err != nil {
			return TestResult{}, err
		}
	}
	if strings.Contains(uri, redact.Mask) {
		return TestResult{}, ErrMaskedURI
	}
	if err := mongouri.Validate(uri); err != nil {
		return TestResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return s.probe(ctx, uri), nil
}

// Databases lists the databases of connection id, hiding admin, config and local
// unless includeSystem is set. Results are sorted by name.
func (s *Service) Databases(ctx context.Context, id string, includeSystem bool) ([]Database, error) {
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.prober == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	dbs, err := s.prober.ListDatabases(ctx, c.URI)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, scrub(err, c.URI))
	}
	out := make([]Database, 0, len(dbs))
	for _, d := range dbs {
		if includeSystem || !slices.Contains(systemDatabases, d.Name) {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b Database) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Collections lists the collections of database on connection id, sorted by name.
func (s *Service) Collections(ctx context.Context, id, database string) ([]Collection, error) {
	if strings.TrimSpace(database) == "" {
		return nil, fmt.Errorf("%w: database is required", ErrInvalid)
	}
	c, err := s.repo.GetConnection(ctx, id)
	if err != nil {
		return nil, err
	}
	if s.prober == nil {
		return nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	cols, err := s.prober.ListCollections(ctx, c.URI, database)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, scrub(err, c.URI))
	}
	slices.SortFunc(cols, func(a, b Collection) int { return strings.Compare(a.Name, b.Name) })
	return cols, nil
}

// EnsureDefault creates a connection named "default" for uri when uri is non-empty
// and no connection exists yet. It reports whether one was created.
func (s *Service) EnsureDefault(ctx context.Context, uri string) (bool, error) {
	if uri == "" {
		return false, nil
	}
	list, err := s.repo.ListConnections(ctx)
	if err != nil {
		return false, err
	}
	if len(list) > 0 {
		return false, nil
	}
	if _, err := s.Create(ctx, Input{Name: "default", URI: uri, Description: "Created from MONGORESCUE_MONGO_URI"}); err != nil {
		return false, err
	}
	return true, nil
}

// probe pings uri within the test timeout.
func (s *Service) probe(ctx context.Context, uri string) TestResult {
	if s.prober == nil {
		return TestResult{Error: ErrUnavailable.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, s.testTimeout)
	defer cancel()
	start := s.now()
	info, err := s.prober.Ping(ctx, uri)
	res := TestResult{LatencyMS: s.now().Sub(start).Milliseconds()}
	if err != nil {
		res.Error = scrub(err, uri)
		return res
	}
	res.OK, res.ServerVersion = true, info.Version
	return res
}

// scrub renders err without the URI or any credential it might contain: the URI is
// replaced by its redacted form, its password (raw and percent-decoded) by the mask,
// and remaining credential patterns by redact.Text.
func scrub(err error, uri string) string {
	msg := err.Error()
	if uri != "" {
		msg = strings.ReplaceAll(msg, uri, redact.URI(uri))
		for _, pw := range passwordsOf(uri) {
			msg = strings.ReplaceAll(msg, pw, redact.Mask)
		}
	}
	return redact.Text(msg)
}

// passwordsOf returns the userinfo password of uri, raw and percent-decoded.
func passwordsOf(uri string) []string {
	authStart, atIdx, ok := redact.SplitUserinfo(uri)
	if !ok || atIdx < 0 {
		return nil
	}
	_, pw, found := strings.Cut(uri[authStart:atIdx], ":")
	if !found || pw == "" {
		return nil
	}
	out := []string{pw}
	if decoded, err := url.PathUnescape(pw); err == nil && decoded != pw {
		out = append(out, decoded)
	}
	return out
}

// validateInput normalises and checks the editable fields.
func validateInput(in *Input) error {
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.URI = strings.TrimSpace(in.URI)
	switch {
	case in.Name == "":
		return fmt.Errorf("%w: name is required", ErrInvalid)
	case len(in.Name) > maxNameLength:
		return fmt.Errorf("%w: name must be at most %d characters", ErrInvalid, maxNameLength)
	case strings.ContainsFunc(in.Name, isControl):
		return fmt.Errorf("%w: name must not contain control characters", ErrInvalid)
	case len(in.Description) > maxDescriptionLength:
		return fmt.Errorf("%w: description must be at most %d characters", ErrInvalid, maxDescriptionLength)
	case in.URI == "":
		return fmt.Errorf("%w: uri is required", ErrInvalid)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// NewID returns a new random connection ID ("conn_" + 16 hex characters).
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("connections: generate id: %w", err)
	}
	return "conn_" + hex.EncodeToString(b), nil
}

// HostList returns the host list of uri (e.g. "db1:27017,db2:27017"), used to name
// connections migrated from legacy job URIs. It never includes credentials.
func HostList(uri string) string {
	rest := uri
	for _, scheme := range []string{mongouri.SchemeSRV, mongouri.SchemeStandard} {
		if strings.HasPrefix(rest, scheme) {
			rest = rest[len(scheme):]
			break
		}
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if rest == "" {
		return "mongodb"
	}
	return rest
}
