package collector

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/redact"
)

// Defaults of a new stream (docs/design/pitr.md, decision 11).
const (
	// DefaultBaseCron takes one base backup a day.
	DefaultBaseCron = "0 2 * * *"
	// DefaultBaseKeepCount and DefaultBaseKeepDays keep 7 bases, or 14 days.
	DefaultBaseKeepCount = 7
	DefaultBaseKeepDays  = 14
	// maxKeep bounds the retention settings.
	maxKeep = 3650
)

// readPreferences are the accepted read preference modes.
var readPreferences = []string{"primary", "primaryPreferred", "secondary", "secondaryPreferred", "nearest"}

// Errors of the stream use cases. Their messages are safe to show to clients.
var (
	// ErrInvalid is returned for an invalid stream request.
	ErrInvalid = errors.New("invalid PITR stream")
	// ErrNotReplicaSet is returned when the connection is not a replica set.
	ErrNotReplicaSet = errors.New("the connection is not a replica set: point-in-time recovery needs the oplog of a replica set")
	// ErrNoOplogAccess is returned when the connection's user may not read the oplog.
	ErrNoOplogAccess = errors.New("the connection's user may not read local.oplog.rs: grant backup, read on local or a custom role with find on it")
	// ErrStillEnabled is returned when deleting an enabled stream.
	ErrStillEnabled = errors.New("disable the PITR stream before deleting it")
	// ErrChunksPending is returned by DeleteStream while the deleted chunks of the
	// stream wait for the end of their grace period.
	ErrChunksPending = errors.New("the stream's oplog chunks are deleted and purged once the delete grace period ends; delete the stream then")
)

// Inspection is what the stream use cases learn about a connection.
type Inspection struct {
	// Window is the connection's oplog window.
	Window pitr.OplogWindow
	// CanReadOplog reports whether its user may read local.oplog.rs.
	CanReadOplog bool
}

// Inspector inspects the replica set of a connection (implemented in internal/app
// with the connection service and mongoconn). It returns pitr.ErrNotReplicaSet for
// a standalone server.
type Inspector func(ctx context.Context, connectionID string) (Inspection, error)

// TargetResolver returns the ID of storage target id ("" is the default target).
type TargetResolver func(ctx context.Context, id string) (string, error)

// StreamRequest creates (all fields) or changes (the fields set) a stream. Omitted
// fields keep their value, or take the defaults on creation.
type StreamRequest struct {
	ConnectionID   *string `json:"connection_id,omitempty"`
	TargetID       *string `json:"target_id,omitempty"`
	Enabled        *bool   `json:"enabled,omitempty"`
	BaseCron       *string `json:"base_cron,omitempty"`
	BaseKeepCount  *int    `json:"base_keep_count,omitempty"`
	BaseKeepDays   *int    `json:"base_keep_days,omitempty"`
	OplogMaxDays   *int    `json:"oplog_max_days,omitempty"`
	ChunkSeconds   *int    `json:"chunk_seconds,omitempty"`
	BaseOnGap      *bool   `json:"base_on_gap,omitempty"`
	ReadPreference *string `json:"read_preference,omitempty"`
	// ChainTestCron schedules chain tests; "" turns them off.
	ChainTestCron *string `json:"chain_test_cron,omitempty"`
	// ChainTestConnectionID is where chain tests restore; "" means the stream's own.
	ChainTestConnectionID *string `json:"chain_test_connection_id,omitempty"`
	// CopyTargets replaces the storage targets the stream's chunks and base
	// backups are copied to (an empty list removes them).
	CopyTargets *[]string `json:"copy_targets,omitempty"`
}

// invalidf returns an ErrInvalid error with a message.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// apply copies the fields set in req to st.
func (req StreamRequest) apply(st *pitr.Stream) {
	set := func(dst *int, v *int) {
		if v != nil {
			*dst = *v
		}
	}
	if req.TargetID != nil {
		st.TargetID = strings.TrimSpace(*req.TargetID)
	}
	if req.Enabled != nil {
		st.Enabled = *req.Enabled
	}
	if req.BaseCron != nil {
		st.BaseCron = strings.TrimSpace(*req.BaseCron)
	}
	set(&st.BaseKeepCount, req.BaseKeepCount)
	set(&st.BaseKeepDays, req.BaseKeepDays)
	set(&st.OplogMaxDays, req.OplogMaxDays)
	set(&st.ChunkSeconds, req.ChunkSeconds)
	if req.BaseOnGap != nil {
		st.BaseOnGap = *req.BaseOnGap
	}
	if req.ReadPreference != nil {
		st.ReadPreference = strings.TrimSpace(*req.ReadPreference)
	}
	if req.ChainTestCron != nil {
		st.ChainTestCron = strings.TrimSpace(*req.ChainTestCron)
	}
	if req.ChainTestConnectionID != nil {
		st.ChainTestConnectionID = strings.TrimSpace(*req.ChainTestConnectionID)
	}
	if req.CopyTargets != nil {
		st.CopyTargets = slices.Clone(*req.CopyTargets)
	}
}

// validate checks the settings of st.
func (s *Service) validate(st *pitr.Stream) error {
	switch {
	case st.ChunkSeconds < MinChunkSeconds || st.ChunkSeconds > MaxChunkSeconds:
		return invalidf("chunk_seconds must be between %d and %d", MinChunkSeconds, MaxChunkSeconds)
	case st.BaseKeepCount < 0 || st.BaseKeepCount > maxKeep || st.BaseKeepDays < 0 || st.BaseKeepDays > maxKeep:
		return invalidf("base_keep_count and base_keep_days must be between 0 and %d", maxKeep)
	case st.BaseKeepCount == 0 && st.BaseKeepDays == 0:
		return invalidf("base_keep_count and base_keep_days cannot both be 0")
	case st.OplogMaxDays < 0 || st.OplogMaxDays > maxKeep:
		return invalidf("oplog_max_days must be between 0 (unset) and %d", maxKeep)
	case st.ReadPreference != "" && !slices.Contains(readPreferences, st.ReadPreference):
		return invalidf("read_preference must be one of %s", strings.Join(readPreferences, ", "))
	case st.BaseCron == "":
		return invalidf("base_cron is required")
	}
	if s.cfg.NextRun != nil {
		if _, ok := s.cfg.NextRun(st.BaseCron, s.now()); !ok {
			return invalidf("base_cron %q is not a valid cron schedule", st.BaseCron)
		}
		if _, ok := s.cfg.NextRun(st.ChainTestCron, s.now()); st.ChainTestCron != "" && !ok {
			return invalidf("chain_test_cron %q is not a valid cron schedule", st.ChainTestCron)
		}
	}
	return nil
}

// checkCollectable checks what an enabled stream needs: encryption on, a replica
// set connection whose user may read the oplog. It returns the replica set name.
func (s *Service) checkCollectable(ctx context.Context, connectionID string) (string, error) {
	if s.cfg.Encryptor() == nil {
		return "", fmt.Errorf("%w: turn on backup encryption with age keys first (Settings → Encryption): the oplog keeps deleted data", ErrEncryptionRequired)
	}
	if s.cfg.Inspect == nil {
		return "", errors.New("collector: no connection inspector is configured")
	}
	in, err := s.cfg.Inspect(ctx, connectionID)
	switch {
	case errors.Is(err, pitr.ErrNotReplicaSet):
		return "", ErrNotReplicaSet
	case err != nil:
		return "", fmt.Errorf("%w: cannot inspect the connection: %s", ErrInvalid, redact.Text(err.Error()))
	case !in.CanReadOplog:
		return "", ErrNoOplogAccess
	case in.Window.ReplicaSet == "":
		return "", ErrNotReplicaSet
	}
	return in.Window.ReplicaSet, nil
}

// resolveTarget resolves the stream's storage target and copy targets: at most
// models.MaxCopyTargets distinct targets other than the primary.
func (s *Service) resolveTarget(ctx context.Context, st *pitr.Stream) error {
	if s.cfg.ResolveTarget != nil {
		id, err := s.cfg.ResolveTarget(ctx, st.TargetID)
		if err != nil {
			return fmt.Errorf("%w: storage target %q: %s", ErrInvalid, st.TargetID, redact.Text(err.Error()))
		}
		st.TargetID = id
	}
	ids, err := models.NormalizeCopyTargets(st.CopyTargets, st.TargetID)
	if err != nil {
		return fmt.Errorf("%w: copy_targets: %w", ErrInvalid, err)
	}
	st.CopyTargets = nil
	for _, id := range ids {
		if s.cfg.ResolveTarget != nil {
			resolved, resolveErr := s.cfg.ResolveTarget(ctx, id)
			if resolveErr != nil {
				return fmt.Errorf("%w: copy target %q: %s", ErrInvalid, id, redact.Text(resolveErr.Error()))
			}
			id = resolved
		}
		if id == st.TargetID || slices.Contains(st.CopyTargets, id) {
			return invalidf("copy_targets must name distinct storage targets other than target_id")
		}
		st.CopyTargets = append(st.CopyTargets, id)
	}
	return nil
}

// CreateStream creates the PITR stream of a replica set connection. An enabled
// stream (the default) needs backup encryption and a replica set whose user may
// read the oplog. Expected failures: ErrInvalid, ErrNotReplicaSet,
// ErrNoOplogAccess, ErrEncryptionRequired and pitr.ErrConnectionTaken.
func (s *Service) CreateStream(ctx context.Context, req StreamRequest) (*pitr.Stream, error) {
	if req.ConnectionID == nil || strings.TrimSpace(*req.ConnectionID) == "" {
		return nil, invalidf("connection_id is required")
	}
	st := &pitr.Stream{ID: newID("pst_"), ConnectionID: strings.TrimSpace(*req.ConnectionID), Enabled: true,
		BaseCron: DefaultBaseCron, BaseKeepCount: DefaultBaseKeepCount, BaseKeepDays: DefaultBaseKeepDays,
		ChunkSeconds: DefaultChunkSeconds, BaseOnGap: true}
	req.apply(st)
	if err := s.validate(st); err != nil {
		return nil, err
	}
	if err := s.resolveTarget(ctx, st); err != nil {
		return nil, err
	}
	// The replica set is recorded even for a disabled stream: it names the keys.
	rs, err := s.checkCollectable(ctx, st.ConnectionID)
	if err != nil {
		return nil, err
	}
	st.ReplicaSet = rs
	if err := s.cfg.Repo.CreateStream(ctx, st); err != nil {
		return nil, err
	}
	s.Reload()
	return s.cfg.Repo.GetStream(ctx, st.ID)
}

// UpdateStream changes stream id. The connection cannot change; enabling it
// checks encryption and the oplog access again. Expected failures: those of
// CreateStream and pitr.ErrNotFound.
func (s *Service) UpdateStream(ctx context.Context, id string, req StreamRequest) (*pitr.Stream, error) {
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return nil, err
	}
	if req.ConnectionID != nil && strings.TrimSpace(*req.ConnectionID) != st.ConnectionID {
		return nil, invalidf("connection_id cannot change: create a stream for the other connection")
	}
	req.apply(st)
	if err := s.validate(st); err != nil {
		return nil, err
	}
	if req.TargetID != nil || req.CopyTargets != nil {
		if err := s.resolveTarget(ctx, st); err != nil {
			return nil, err
		}
	}
	if st.Enabled {
		if _, err := s.checkCollectable(ctx, st.ConnectionID); err != nil {
			return nil, err
		}
	}
	if err := s.cfg.Repo.UpdateStream(ctx, st); err != nil {
		return nil, err
	}
	s.Reload()
	return s.cfg.Repo.GetStream(ctx, id)
}

// DeleteStream deletes a disabled stream. Its open chain ends (stopped) and every
// chunk is deleted with the delete grace period; until the purge removed their
// objects it returns ErrChunksPending, and the stream is deleted by a later call.
// Its base backups stay, as backups. Expected failures: pitr.ErrNotFound,
// ErrStillEnabled and ErrChunksPending.
func (s *Service) DeleteStream(ctx context.Context, id string) error {
	st, err := s.cfg.Repo.GetStream(ctx, id)
	if err != nil {
		return err
	}
	if st.Enabled {
		return ErrStillEnabled
	}
	err = s.cfg.Repo.DeleteStream(ctx, id)
	if !errors.Is(err, pitr.ErrInUse) {
		if err == nil {
			s.observe(func(o Observer) { o.ForgetPITRStream(id) })
		}
		return err
	}
	unlock, err := lockStream(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	now := s.now()
	chains, err := s.cfg.Repo.ListChains(ctx, id)
	if err != nil {
		return err
	}
	var ids []string
	for _, c := range chains {
		if c.Open() {
			if err := s.cfg.Repo.EndChain(ctx, id, c.ChainID, s.chainEnd(ctx, id), pitr.EndStopped, now); err != nil && !errors.Is(err, pitr.ErrChainEnded) {
				return err
			}
		}
		chunks, err := s.cfg.Repo.ListChunks(ctx, pitr.ChunkQuery{StreamID: id, ChainID: c.ChainID})
		if err != nil {
			return err
		}
		for _, ch := range chunks {
			if ch.DeletedAt == nil && ch.Status != pitr.ChunkPruned {
				ids = append(ids, ch.ID)
			}
		}
	}
	if _, err := s.cfg.Repo.DeleteChunks(ctx, ids, now, now.Add(s.deleteGrace())); err != nil {
		return err
	}
	return ErrChunksPending
}

// chainEnd returns the collector's position of stream id (zero without a state).
func (s *Service) chainEnd(ctx context.Context, id string) pitr.Timestamp {
	st, err := s.cfg.Repo.LoadState(ctx, id)
	if err != nil {
		return pitr.Timestamp{}
	}
	return st.Last.TS
}

// deleteGrace returns the delete grace period in force.
func (s *Service) deleteGrace() time.Duration {
	if s.cfg.DeleteGrace == nil {
		return models.GraceDuration(models.DefaultDeleteGraceDays)
	}
	return s.cfg.DeleteGrace()
}

// GetStream returns stream id or pitr.ErrNotFound.
func (s *Service) GetStream(ctx context.Context, id string) (*pitr.Stream, error) {
	return s.cfg.Repo.GetStream(ctx, id)
}

// ListStreams returns every stream.
func (s *Service) ListStreams(ctx context.Context) ([]*pitr.Stream, error) {
	return s.cfg.Repo.ListStreams(ctx)
}

// ChunkPage is one page of a stream's chunks (metadata only, never their
// contents).
type ChunkPage struct {
	Chunks []*pitr.Chunk `json:"chunks"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// MaxChunkPage caps a page of chunks.
const MaxChunkPage = 500

// ListChunks returns a page of the chunks of stream id, newest first.
func (s *Service) ListChunks(ctx context.Context, id string, limit, offset int) (*ChunkPage, error) {
	if _, err := s.cfg.Repo.GetStream(ctx, id); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxChunkPage {
		limit = 100
	}
	offset = max(offset, 0)
	chunks, total, err := s.cfg.Repo.ListStreamChunks(ctx, id, limit, offset)
	if err != nil {
		return nil, err
	}
	return &ChunkPage{Chunks: chunks, Total: total, Limit: limit, Offset: offset}, nil
}
