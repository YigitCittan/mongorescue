package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/notify"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
)

// Tables of records the store decodes, as reported in CorruptRecord.Table.
const (
	tableBackups  = "backups"
	tableRestores = "restores"
	tableRules    = "notification_rules"
	tableUsers    = "users"
	tableAPIKeys  = "api_keys"
)

// CorruptRecord identifies a stored row that cannot be read: its JSON does not decode
// into the record type, or one of its encrypted fields cannot be opened. Lists skip
// such rows instead of failing; the row itself is never changed or deleted.
type CorruptRecord struct {
	// Table is the database table of the row, for example "jobs".
	Table string `json:"table"`
	// ID is the row's id column.
	ID string `json:"id"`
	// Error summarises why the row cannot be read. It never contains stored data.
	Error string `json:"error"`
}

// corruptKey identifies a row in the corrupt record log.
type corruptKey struct{ table, id string }

// corruptLog remembers the rows that lists skipped, so they can be reported until
// they are repaired or deleted. It is safe for concurrent use.
type corruptLog struct {
	mu   sync.Mutex
	rows map[corruptKey]CorruptRecord
}

// reportCorrupt records that row id of table cannot be read and logs it once (and
// again when the reason changes). The log names the table and the ID, never the data.
func (s *SQLiteStore) reportCorrupt(table, id string, err error) {
	rec := CorruptRecord{Table: table, ID: id, Error: corruptSummary(err)}
	key := corruptKey{table, id}
	s.corrupt.mu.Lock()
	if s.corrupt.rows == nil {
		s.corrupt.rows = make(map[corruptKey]CorruptRecord)
	}
	prev, seen := s.corrupt.rows[key]
	s.corrupt.rows[key] = rec
	s.corrupt.mu.Unlock()
	if !seen || prev.Error != rec.Error {
		s.logger.Warn("skipping a stored record that cannot be read; the row is left unchanged in the database",
			slog.String("table", table), slog.String("id", id), slog.String("reason", rec.Error))
	}
}

// reportCorruptField records that an encrypted field of row id cannot be opened
// although the row itself is still used without it (table names the field, such as
// tableJobHeartbeats), and logs it once (and again when the reason changes).
func (s *SQLiteStore) reportCorruptField(table, id string, err error) {
	rec := CorruptRecord{Table: table, ID: id, Error: corruptSummary(err)}
	key := corruptKey{table, id}
	s.corrupt.mu.Lock()
	if s.corrupt.rows == nil {
		s.corrupt.rows = make(map[corruptKey]CorruptRecord)
	}
	prev, seen := s.corrupt.rows[key]
	s.corrupt.rows[key] = rec
	s.corrupt.mu.Unlock()
	if !seen || prev.Error != rec.Error {
		s.logger.Warn("an encrypted field of a stored record cannot be opened; the record is used without it and left unchanged in the database",
			slog.String("field", table), slog.String("id", id), slog.String("reason", rec.Error))
	}
}

// clearCorrupt forgets row id of table after it was read successfully.
func (s *SQLiteStore) clearCorrupt(table, id string) {
	s.corrupt.mu.Lock()
	defer s.corrupt.mu.Unlock()
	if len(s.corrupt.rows) == 0 {
		return
	}
	key := corruptKey{table, id}
	if _, ok := s.corrupt.rows[key]; ok {
		delete(s.corrupt.rows, key)
		s.logger.Info("a stored record that could not be read is readable again",
			slog.String("table", table), slog.String("id", id))
	}
}

// CorruptRecords returns the rows that lists skipped because they cannot be read,
// sorted by table and ID. Every row is checked again first: rows that were repaired
// or deleted meanwhile are dropped from the report. Rows are never changed.
func (s *SQLiteStore) CorruptRecords(ctx context.Context) ([]CorruptRecord, error) {
	defer s.lockKey()()
	s.corrupt.mu.Lock()
	pending := make([]CorruptRecord, 0, len(s.corrupt.rows))
	for _, rec := range s.corrupt.rows {
		pending = append(pending, rec)
	}
	s.corrupt.mu.Unlock()

	out := make([]CorruptRecord, 0, len(pending))
	for _, rec := range pending {
		check, ok := recordCheckers[rec.Table]
		if !ok {
			out = append(out, rec)
			continue
		}
		err := check(ctx, s, rec.ID)
		switch {
		case errors.Is(err, errRowGone):
			s.forgetCorrupt(rec.Table, rec.ID)
		case err == nil:
			s.clearCorrupt(rec.Table, rec.ID)
		case errors.Is(err, ErrCorruptRecord):
			s.reportCorrupt(rec.Table, rec.ID, err)
			out = append(out, CorruptRecord{Table: rec.Table, ID: rec.ID, Error: corruptSummary(err)})
		default:
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b CorruptRecord) int {
		return cmp.Or(cmp.Compare(a.Table, b.Table), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// forgetCorrupt drops a deleted row from the corrupt record log.
func (s *SQLiteStore) forgetCorrupt(table, id string) {
	s.corrupt.mu.Lock()
	defer s.corrupt.mu.Unlock()
	delete(s.corrupt.rows, corruptKey{table, id})
}

// scanCorruptRecords reads every record once, so rows that cannot be read are logged
// and reported from startup on, not only once a list happens to reach them. It only
// logs failures: a damaged row must never keep the store from opening.
func (s *SQLiteStore) scanCorruptRecords(ctx context.Context) {
	scans := []struct {
		table string
		list  func(context.Context) error
	}{
		{tableJobs, func(ctx context.Context) error { _, err := s.ListJobs(ctx); return err }},
		{tableConnections, func(ctx context.Context) error { _, err := s.ListConnections(ctx); return err }},
		{tableStorageTargets, func(ctx context.Context) error { _, err := s.ListStorageTargets(ctx); return err }},
		{tableChannels, func(ctx context.Context) error { _, err := s.ListChannels(ctx); return err }},
		{tableRules, func(ctx context.Context) error { _, err := s.ListRules(ctx); return err }},
		{tableUsers, func(ctx context.Context) error { _, err := s.ListUsers(ctx); return err }},
		{tableAPIKeys, func(ctx context.Context) error { _, err := s.ListAPIKeys(ctx); return err }},
		{tableBackups, func(ctx context.Context) error { _, err := s.ListBackupRecords(ctx, ""); return err }},
		{tableRestores, func(ctx context.Context) error { _, err := s.ListRestoreRecords(ctx); return err }},
	}
	for _, sc := range scans {
		if err := sc.list(ctx); err != nil && !errors.Is(err, ErrNoSecretBox) {
			s.logger.Warn("could not check stored records for damage",
				slog.String("table", sc.table), slog.Any("error", err))
		}
	}
}

// errRowGone is returned by a record checker when the row no longer exists.
var errRowGone = errors.New("store: row no longer exists")

// recordChecker reads row id the way the lists do and returns nil when it can be
// read, errRowGone when it was deleted, or an error wrapping ErrCorruptRecord.
type recordChecker func(ctx context.Context, s *SQLiteStore, id string) error

// recordCheckers maps every table reported in CorruptRecord to its checker.
var recordCheckers = map[string]recordChecker{
	tableJobs: checkJSON(tableJobs, func(s *SQLiteStore, j *models.Job) error {
		return s.openJob(j)
	}),
	// The job itself stays readable; this is its heartbeat URL alone.
	tableJobHeartbeats: checkJSON(tableJobs, func(s *SQLiteStore, j *models.Job) error {
		return s.openJobHeartbeat(j)
	}),
	tableBackups:  checkJSON[models.BackupRecord](tableBackups, nil),
	tableRestores: checkJSON[models.RestoreRecord](tableRestores, nil),
	tableJobRuns:  checkJSON[models.JobRun](tableJobRuns, nil),
	tableRules:    checkJSON[notify.Rule](tableRules, nil),
	tableConnections: checkJSON(tableConnections, func(s *SQLiteStore, c *storedConnection) error {
		return s.openStoredConnection(c)
	}),
	tableStorageTargets: checkJSON(tableStorageTargets, func(s *SQLiteStore, t *models.StorageTarget) error {
		return s.openStorageTarget(t)
	}),
	tableChannels: checkJSON(tableChannels, func(s *SQLiteStore, ch *notify.Channel) error {
		_, err := s.openChannel(ch)
		return err
	}),
	tableUsers: func(ctx context.Context, s *SQLiteStore, id string) error {
		var u auth.User
		err := scanUserInto(s.db.QueryRowContext(ctx, "SELECT "+userColumns+" FROM users WHERE id = ?", id), &u)
		return rowCheckResult(err)
	},
	tableAPIKeys: func(ctx context.Context, s *SQLiteStore, id string) error {
		var k auth.APIKey
		err := scanAPIKeyInto(s.db.QueryRowContext(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE id = ?", id), &k)
		return rowCheckResult(err)
	},
}

// rowCheckResult maps a scan error of a column-based row to a checker result.
func rowCheckResult(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errRowGone
	}
	return err
}

// errDecodeRecord marks a JSON decoding failure of a stored record.
var errDecodeRecord = errors.New("store: decode record")

// columnScanError is a column value of a row that does not convert to its Go type.
// Its message is not shown: database/sql quotes the offending value.
type columnScanError struct{ err error }

func (e *columnScanError) Error() string { return "scan row: " + e.err.Error() }
func (e *columnScanError) Unwrap() error { return e.err }

// checkJSON returns the checker of a JSON record table; open, when not nil, also
// opens the record's encrypted fields.
func checkJSON[T any](table string, open func(*SQLiteStore, *T) error) recordChecker {
	return func(ctx context.Context, s *SQLiteStore, id string) error {
		v, err := getRecord[T](ctx, s.db, errRowGone, "SELECT data FROM "+table+" WHERE id = ?", id) //nolint:gosec // G202: table is a constant.
		if err != nil || open == nil {
			return err
		}
		if err := open(s, v); err != nil {
			if errors.Is(err, ErrNoSecretBox) {
				return nil
			}
			return unreadable(err)
		}
		return nil
	}
}

// unreadable marks err, raised while reading one stored row, as ErrCorruptRecord. The
// returned error's message is the corruptSummary of err, never err's own text: JSON,
// time and column conversion errors quote the stored value. err stays in the chain
// for errors.Is and errors.As.
func unreadable(err error) error {
	var rec *recordError
	if errors.As(err, &rec) {
		return err
	}
	return &recordError{summary: corruptSummary(err), err: err}
}

// recordError is an error reading one stored row; see unreadable.
type recordError struct {
	summary string
	err     error
}

func (e *recordError) Error() string   { return ErrCorruptRecord.Error() + ": " + e.summary }
func (e *recordError) Unwrap() []error { return []error{ErrCorruptRecord, e.err} }

// corruptSummary describes why a row cannot be read without quoting stored data:
// JSON and time parsing errors echo the offending value, so only their shape is kept.
func corruptSummary(err error) string {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		timeErr   *time.ParseError
		scanErr   *columnScanError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return fmt.Sprintf("malformed JSON at byte %d", syntaxErr.Offset)
	case errors.As(err, &typeErr):
		kind, _, _ := strings.Cut(typeErr.Value, " ")
		field := typeErr.Field
		if field == "" {
			field = "(record)"
		}
		return fmt.Sprintf("field %s: a JSON %s does not fit type %s", field, kind, typeErr.Type)
	case errors.As(err, &timeErr):
		return "a timestamp field is not a valid time"
	case errors.As(err, &scanErr):
		return "a column has a value of the wrong type"
	case errors.Is(err, ErrUnsealedSecret):
		return "an encrypted field is stored in plaintext"
	case errors.Is(err, secretbox.ErrDecrypt), errors.Is(err, secretbox.ErrMalformed):
		return "an encrypted field cannot be decrypted"
	case errors.Is(err, errDecodeRecord):
		return "the stored JSON does not match the record format"
	default:
		return "the record cannot be read"
	}
}
