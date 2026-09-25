package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yigitcittan/mongorescue/internal/operations"
)

// Resource URIs.
const (
	// URIScheme is the scheme of every MongoRescue resource.
	URIScheme = "mongorescue"
	// StatusURI is the operational overview (the get_status tool as a resource).
	StatusURI = URIScheme + "://status"
	// BackupURITemplate addresses one backup record.
	BackupURITemplate = URIScheme + "://backups/{id}"
	// JobURITemplate addresses one scheduled job.
	JobURITemplate = URIScheme + "://jobs/{id}"
	// RestoreURITemplate addresses one restore record.
	RestoreURITemplate = URIScheme + "://restores/{id}"
)

// jsonMIME is the MIME type of every resource.
const jsonMIME = "application/json"

func (s *Server) registerResources() {
	s.sdk.AddResource(&sdk.Resource{
		URI:         StatusURI,
		Name:        "status",
		Title:       "MongoRescue status",
		Description: "Health, version, counts, the last successful backup of every job and recent failures.",
		MIMEType:    jsonMIME,
	}, s.readStatus)
	for _, t := range []struct {
		uri, name, title, desc string
		get                    func(ctx context.Context, id string) (any, error)
	}{
		{BackupURITemplate, "backup", "Backup record", "One backup record by ID (see list_backups).",
			func(ctx context.Context, id string) (any, error) { return s.cfg.Operations.GetBackup(ctx, id) }},
		{JobURITemplate, "job", "Scheduled job", "One scheduled backup job by ID (see list_jobs).",
			func(ctx context.Context, id string) (any, error) { return s.cfg.Operations.GetJob(ctx, id) }},
		{RestoreURITemplate, "restore", "Restore record", "One restore record by ID (see list_restores).",
			func(ctx context.Context, id string) (any, error) { return s.cfg.Operations.GetRestore(ctx, id) }},
	} {
		kind := strings.TrimSuffix(strings.TrimPrefix(t.uri, URIScheme+"://"), "/{id}")
		get := t.get
		s.sdk.AddResourceTemplate(&sdk.ResourceTemplate{
			URITemplate: t.uri, Name: t.name, Title: t.title, Description: t.desc, MIMEType: jsonMIME,
		}, func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			uri := req.Params.URI
			id, err := resourceID(uri, kind)
			if err != nil {
				return nil, sdk.ResourceNotFoundError(uri)
			}
			v, err := get(ctx, id)
			if errors.Is(err, operations.ErrNotFound) {
				return nil, sdk.ResourceNotFoundError(uri)
			}
			if err != nil {
				return nil, s.toolError("resource "+kind, err)
			}
			return jsonResource(uri, v)
		})
	}
}

func (s *Server) readStatus(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	st, err := s.cfg.Operations.Status(ctx)
	if err != nil {
		return nil, s.toolError("resource status", err)
	}
	return jsonResource(req.Params.URI, st)
}

// resourceID extracts the ID from mongorescue://<kind>/<id>.
func resourceID(uri, kind string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != URIScheme || u.Host != kind {
		return "", fmt.Errorf("%w: not a %s resource", errInvalidInput, kind)
	}
	id := strings.TrimPrefix(u.Path, "/")
	if err := requireID("id", id); err != nil || strings.Contains(id, "/") {
		return "", fmt.Errorf("%w: bad %s id", errInvalidInput, kind)
	}
	return id, nil
}

// jsonResource renders v as the JSON contents of uri.
func jsonResource(uri string, v any) (*sdk.ReadResourceResult, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode resource: %w", err)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: jsonMIME, Text: string(raw)}}}, nil
}
