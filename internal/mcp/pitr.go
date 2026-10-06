package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/pitr/collector"
)

// pitrStatusInput selects the streams of pitr_status.
type pitrStatusInput struct {
	StreamID string `json:"stream_id,omitempty" jsonschema:"one PITR stream (default: every stream)"`
}

// pitrStatusOutput lists PITR streams with their windows.
type pitrStatusOutput struct {
	Streams []*collector.StreamStatus `json:"streams"`
}

// pitrRestoreInput is a point-in-time restore request.
type pitrRestoreInput struct {
	StreamID           string   `json:"stream_id" jsonschema:"PITR stream ID, or the ID of its connection (see pitr_status)"`
	At                 string   `json:"at" jsonschema:"RFC 3339 time to restore to, inside a window of pitr_status (every write up to and including that second is restored)"`
	Databases          []string `json:"databases,omitempty" jsonschema:"only restore these databases (default: every database except admin, config and local)"`
	TargetConnectionID string   `json:"target_connection_id,omitempty" jsonschema:"another connection to restore into (default: the stream's connection)"`
	Force              bool     `json:"force,omitempty" jsonschema:"WARNING: starts the restore even though a preflight check failed; set it only after reading the failed checks"`
}

// registerPITRTools adds pitr_status and pitr_restore.
func (s *Server) registerPITRTools() {
	addTool(s, &sdk.Tool{
		Name: ToolPITRStatus,
		Description: "Show the PITR (point-in-time recovery) streams: the collector's state, lag and the windows of times that can be " +
			"restored (a gap splits them), with the base backups. Experimental.",
		Annotations: readOnly("PITR status"),
		InputSchema: schemaFor[pitrStatusInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "stream_id") }),
	}, s.pitrStatus)
	addTool(s, &sdk.Tool{
		Name: ToolPITRRestore,
		Description: "Restore a replica set, or some of its databases, to a point in time: every database is restored into a NEW " +
			"database named <db>_rescue_<timestamp>_<id> from a base backup, and the oplog is replayed up to the time. Existing data is " +
			"never overwritten; admin, config and local are never restored. Admin API keys only; experimental. The time must lie in " +
			"a window of pitr_status. A preflight runs first (chain, privileges, disk space, tools version): its result is in the output, " +
			"and a failed check refuses the restore unless force is set. Poll get_restore until completed or failed.",
		Annotations: additive("Point-in-time restore to safe clones"),
		InputSchema: schemaFor[pitrRestoreInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "stream_id", "target_connection_id")
			p["at"].MinLength, p["at"].MaxLength = ptr(1), ptr(64)
			if d := p["databases"]; d != nil {
				d.MaxItems = ptr(operations.MaxBackupDatabases)
				d.Items = databaseEntrySchema()
			}
		}),
	}, s.pitrRestore)
}

func (s *Server) pitrStatus(ctx context.Context, in pitrStatusInput) (pitrStatusOutput, string, error) {
	out := pitrStatusOutput{Streams: []*collector.StreamStatus{}}
	if s.cfg.PITR == nil {
		return out, "No PITR streams.", nil
	}
	if in.StreamID != "" {
		st, err := s.cfg.PITR.Status(ctx, in.StreamID)
		if errors.Is(err, pitr.ErrNotFound) {
			return out, "", fmt.Errorf("%w: PITR stream not found", operations.ErrNotFound)
		}
		if err != nil {
			return out, "", err
		}
		out.Streams = append(out.Streams, st)
	} else {
		list, err := s.cfg.PITR.ListStatuses(ctx)
		if err != nil {
			return out, "", err
		}
		out.Streams = append(out.Streams, list...)
	}
	parts := make([]string, 0, len(out.Streams))
	for _, st := range out.Streams {
		desc := fmt.Sprintf("stream %s: %d window(s)", idText(st.Stream.ID), len(st.Windows))
		if n := len(st.Windows); n > 0 {
			w := st.Windows[n-1]
			desc += fmt.Sprintf(", newest %s to %s", w.StartTime.Format(time.RFC3339), w.EndTime.Format(time.RFC3339))
		}
		parts = append(parts, desc)
	}
	if len(parts) == 0 {
		return out, "No PITR streams.", nil
	}
	return out, fmt.Sprintf("%d PITR stream(s) (experimental): %s.", len(parts), strings.Join(parts, "; ")), nil
}

func (s *Server) pitrRestore(ctx context.Context, in pitrRestoreInput) (restoreStarted, string, error) {
	if err := requireID("stream_id", in.StreamID); err != nil {
		return restoreStarted{}, "", err
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(in.At))
	if err != nil {
		return restoreStarted{}, "", fmt.Errorf("%w: at must be an RFC 3339 time such as 2026-10-05T14:30:00Z", errInvalidInput)
	}
	// Always safe clones: the request has no way to name an existing database.
	rec, err := s.cfg.Operations.StartRestore(ctx, models.RestoreRequest{
		PITR:               &models.PITRTarget{StreamID: in.StreamID, At: &at},
		Databases:          in.Databases,
		TargetConnectionID: in.TargetConnectionID,
		Force:              in.Force,
	})
	if err != nil {
		return restoreStarted{}, "", err
	}
	next := fmt.Sprintf("poll get_restore with id %q until status is completed or failed", rec.ID)
	return restoreStarted{Restore: rec, Preflight: rec.Preflight, NextStep: next},
		fmt.Sprintf("Point-in-time restore %s to %s from base %s started into new databases ending in %s%s; %s.",
			idText(rec.ID), rec.PITR.TargetTime.Format(time.RFC3339), idText(rec.PITR.BaseID), quoted(rec.PITR.CloneSuffix),
			preflightSummary(rec.Preflight), next), nil
}
