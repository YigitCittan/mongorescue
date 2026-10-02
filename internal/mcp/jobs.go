package mcp

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// selectionInput is a database selection as assistants send it.
type selectionInput struct {
	Mode           string   `json:"mode" jsonschema:"single, list, all or pattern"`
	Databases      []string `json:"databases,omitempty" jsonschema:"single: the database; list: the databases; all and pattern: databases always backed up in addition"`
	Include        []string `json:"include,omitempty" jsonschema:"pattern: glob patterns of the databases to back up (* any characters, ? one character; case-sensitive)"`
	Exclude        []string `json:"exclude,omitempty" jsonschema:"all and pattern: glob patterns of databases to skip"`
	AutoIncludeNew bool     `json:"auto_include_new,omitempty" jsonschema:"all and pattern: also back up databases created later (default false: they are reported as new instead)"`
}

type previewJobDatabasesInput struct {
	JobID        string          `json:"job_id,omitempty" jsonschema:"ID of the scheduled job (see list_jobs); omit to preview a selection for a new job"`
	ConnectionID string          `json:"connection_id,omitempty" jsonschema:"connection to resolve against (default: the job's; required without job_id)"`
	Selection    *selectionInput `json:"database_selection,omitempty" jsonschema:"selection to preview instead of the job's (required without job_id)"`
}

type listJobRunsInput struct {
	JobID string `json:"job_id" jsonschema:"ID of the scheduled job (see list_jobs)"`
	Limit int    `json:"limit,omitempty" jsonschema:"most runs to return, newest first (default and maximum 200)"`
}

type jobRunList struct {
	JobID string           `json:"job_id"`
	Runs  []*models.JobRun `json:"runs"`
}

// registerJobDatabaseTools registers preview_job_databases and list_job_runs.
func (s *Server) registerJobDatabaseTools() {
	addTool(s, &sdk.Tool{
		Name: ToolPreviewJobDatabases,
		Description: "Resolve a job's database selection against the live server exactly as its next run would: the databases " +
			"it backs up (included), the ones it skips with the reason (excluded: system, excluded, not_matched, not_selected, " +
			"new, not_found) and the databases found since the job last ran (new_since_last_run). Pass database_selection " +
			"(and connection_id) to preview another selection, also for a job that does not exist yet. Nothing is changed.",
		Annotations: readOnly("Preview job databases"),
		InputSchema: schemaFor[previewJobDatabasesInput](func(p map[string]*jsonschema.Schema) { limitIDs(p, "job_id", "connection_id") }),
	}, s.previewJobDatabases)
	addTool(s, &sdk.Tool{
		Name: ToolListJobRuns,
		Description: "List a job's runs, newest first: each run's status over all its databases (running, ok, partial, failed, " +
			"cancelled), the outcome and backup ID of every database and the new databases it did not include.",
		Annotations: readOnly("List job runs"),
		InputSchema: schemaFor[listJobRunsInput](func(p map[string]*jsonschema.Schema) {
			limitIDs(p, "job_id")
			if l := p["limit"]; l != nil {
				l.Minimum, l.Maximum = ptr(1.0), ptr(float64(operations.MaxJobRunList))
			}
		}),
	}, s.listJobRuns)
}

func (s *Server) previewJobDatabases(ctx context.Context, in previewJobDatabasesInput) (*operations.DatabasePreview, string, error) {
	req := operations.DatabasePreviewRequest{ConnectionID: in.ConnectionID}
	if in.Selection != nil {
		req.Selection = &models.DatabaseSelection{
			Mode: models.SelectionMode(in.Selection.Mode), Databases: in.Selection.Databases,
			Include: in.Selection.Include, Exclude: in.Selection.Exclude, AutoIncludeNew: in.Selection.AutoIncludeNew,
		}
	}
	if in.JobID == "" && (req.Selection == nil || in.ConnectionID == "") {
		return nil, "", fmt.Errorf("pass job_id, or connection_id and database_selection")
	}
	p, err := s.cfg.Operations.PreviewJobDatabases(ctx, in.JobID, req)
	if err != nil {
		return nil, "", err
	}
	return p, fmt.Sprintf("The selection backs up %d database(s); %d excluded, %d new since the last run.",
		len(p.Included), len(p.Excluded), len(p.New)), nil
}

func (s *Server) listJobRuns(ctx context.Context, in listJobRunsInput) (jobRunList, string, error) {
	if err := requireID("job_id", in.JobID); err != nil {
		return jobRunList{}, "", err
	}
	list, err := s.cfg.Operations.ListJobRuns(ctx, in.JobID, in.Limit)
	if err != nil {
		return jobRunList{}, "", err
	}
	return jobRunList{JobID: in.JobID, Runs: list}, fmt.Sprintf("%d run(s) of job %s.", len(list), idText(in.JobID)), nil
}
