package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// Delete protection routes: undelete, approvals of the two-person rule and the
// pending changes that lower a protection later (see docs/security.md).
const (
	undeleteRoute       = "POST /api/v1/backups/{id}/undelete"
	listApprovalsRoute  = "GET /api/v1/approvals"
	getApprovalRoute    = "GET /api/v1/approvals/{id}"
	approveRoute        = "POST /api/v1/approvals/{id}/approve"
	rejectRoute         = "POST /api/v1/approvals/{id}/reject"
	listPendingRoute    = "GET /api/v1/pending-changes"
	cancelPendingRoute  = "DELETE /api/v1/pending-changes/{id}"
	maxProtectionBody   = 8 << 10
	approvalRequiredMsg = "a second administrator must approve this action"
)

// registerProtectionRoutes adds the delete protection endpoints.
func (s *Server) registerProtectionRoutes(mux *router) {
	mux.HandleFunc(undeleteRoute, s.handleUndeleteBackup)
	mux.HandleFunc(listApprovalsRoute, s.handleListApprovals)
	mux.HandleFunc(getApprovalRoute, s.handleGetApproval)
	mux.HandleFunc(approveRoute, s.handleApprove)
	mux.HandleFunc(rejectRoute, s.handleReject)
	mux.HandleFunc(listPendingRoute, s.handleListPendingChanges)
	mux.HandleFunc(cancelPendingRoute, s.handleCancelPendingChange)
}

// approvalRequired is the body of a 202 answer to a destructive request that waits
// for a second administrator.
type approvalRequired struct {
	// ApprovalRequired is always true.
	ApprovalRequired bool `json:"approval_required"`
	// Approval is the stored request.
	Approval *models.Approval `json:"approval"`
}

// writeApprovalPending answers 202 Accepted with the approval request err carries, and
// reports whether err was one.
func writeApprovalPending(w http.ResponseWriter, err error) bool {
	var pending *operations.ApprovalPendingError
	if !errors.As(err, &pending) {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(apiResponse{
		Success: true,
		Data:    approvalRequired{ApprovalRequired: true, Approval: pending.Approval},
		Message: approvalRequiredMsg + ": " + pending.Approval.Summary,
	})
	return true
}

// handleUndeleteBackup undoes the deletion of a backup during its grace period.
func (s *Server) handleUndeleteBackup(w http.ResponseWriter, r *http.Request) {
	rec, err := s.ops.UndeleteBackup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleListApprovals lists approval requests, newest first (?status= filters).
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.ListApprovals(r.Context(), models.ApprovalStatus(r.URL.Query().Get("status")))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGetApproval returns one approval request.
func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	a, err := s.ops.GetApproval(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleApprove approves a request and runs its action. Only a signed-in
// administrator other than the requester may approve; API keys never can (the
// operations service applies auth.CheckApprover).
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	a, err := s.ops.Approve(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// rejectRequest is the optional body of POST /api/v1/approvals/{id}/reject.
type rejectRequest struct {
	// Reason explains the rejection.
	Reason string `json:"reason"`
}

// handleReject rejects a request.
func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	var req rejectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxProtectionBody)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request json")
		return
	}
	a, err := s.ops.Reject(r.Context(), r.PathValue("id"), req.Reason)
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleListPendingChanges lists the lowered protections that take effect later.
func (s *Server) handleListPendingChanges(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.PendingChanges(r.Context())
	if err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCancelPendingChange drops a pending change, keeping the protection.
func (s *Server) handleCancelPendingChange(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.ops.CancelPendingChange(r.Context(), id); err != nil {
		s.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cancelled_id": id})
}
