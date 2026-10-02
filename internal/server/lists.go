package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/store"
)

// errBadParam marks a malformed list query parameter (answered with 400).
var errBadParam = errors.New("invalid query parameter")

// listMeta describes the page of a list response that asked for a limit.
type listMeta struct {
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// listPage holds the paging and range parameters shared by the list endpoints.
type listPage struct {
	from, to      time.Time
	sort          store.SortOrder
	limit, offset int
	// limited reports that the request named a limit, so the response carries meta.
	limited bool
}

// parseListPage reads from, to, sort, limit and offset. Empty values count as absent.
func parseListPage(q url.Values) (listPage, error) {
	var p listPage
	var err error
	if p.from, err = timeParam(q, "from"); err != nil {
		return p, err
	}
	if p.to, err = timeParam(q, "to"); err != nil {
		return p, err
	}
	switch v := q.Get("sort"); v {
	case "", string(store.SortNewest), string(store.SortOldest):
		p.sort = store.SortOrder(v)
	default:
		return p, fmt.Errorf("%w: sort must be %q or %q", errBadParam, store.SortNewest, store.SortOldest)
	}
	if v := q.Get("limit"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > operations.MaxListLimit {
			return p, fmt.Errorf("%w: limit must be an integer between 1 and %d", errBadParam, operations.MaxListLimit)
		}
		p.limit, p.limited = n, true
	}
	if v := q.Get("offset"); v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			return p, fmt.Errorf("%w: offset must be a non-negative integer", errBadParam)
		}
		p.offset = n
	}
	if !p.from.IsZero() && !p.to.IsZero() && p.to.Before(p.from) {
		return p, fmt.Errorf("%w: to must not be before from", errBadParam)
	}
	return p, nil
}

// timeParam parses the RFC 3339 time in parameter name, zero when absent.
func timeParam(q url.Values, name string) (time.Time, error) {
	v := q.Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be an RFC 3339 time such as 2026-10-01T00:00:00Z", errBadParam, name)
	}
	return t, nil
}

// idList splits a comma-separated id parameter, dropping empty entries.
func idList(v string) []string {
	var ids []string
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// writeList answers a list request: the items as data, plus meta when p is limited.
func writeList(w http.ResponseWriter, p listPage, total int, items any) {
	resp := struct {
		apiResponse
		Meta *listMeta `json:"meta,omitempty"`
	}{apiResponse: apiResponse{Success: true, Data: items}}
	if p.limited {
		resp.Meta = &listMeta{Total: total, Limit: p.limit, Offset: p.offset}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleListBackups lists backups matching the query parameters, newest first. Without
// limit every match is returned, as before pagination existed.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := parseListPage(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.ops.QueryBackups(r.Context(), operations.BackupFilter{
		IDs:    idList(q.Get("id")),
		Status: models.BackupStatus(q.Get("status")), Database: q.Get("database"),
		ConnectionID: q.Get("connection_id"), JobID: q.Get("job_id"),
		Trigger: models.BackupTrigger(q.Get("trigger")), RetryOf: q.Get("retry_of"), RunID: q.Get("run_id"),
		From: p.from, To: p.to, Search: q.Get("q"), Sort: p.sort, Limit: p.limit, Offset: p.offset,
	})
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeList(w, p, page.Total, page.Items)
}

// handleListRestores lists restores matching the query parameters, newest first.
// Without limit every match is returned.
func (s *Server) handleListRestores(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := parseListPage(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := s.ops.QueryRestores(r.Context(), operations.RestoreFilter{
		Status: models.RestoreStatus(q.Get("status")), BackupID: q.Get("backup_id"), TargetDatabase: q.Get("database"),
		From: p.from, To: p.to, Search: q.Get("q"), Sort: p.sort, Limit: p.limit, Offset: p.offset,
	})
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeList(w, p, page.Total, page.Items)
}

// handleBackupDatabases lists the distinct databases of all backups, for list filters.
func (s *Server) handleBackupDatabases(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.ops.BackupDatabases(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dbs)
}

// handleRestoreDatabases lists the distinct target databases of all restores.
func (s *Server) handleRestoreDatabases(w http.ResponseWriter, r *http.Request) {
	dbs, err := s.ops.RestoreDatabases(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dbs)
}
