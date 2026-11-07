package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/yigitcittan/mongorescue/internal/redact"
)

// readyPath is the readiness check. Like the health check it needs no credentials,
// is never rate limited and is served by a standby and by an instance that changes
// role. It is not /api/v1/readiness, the recovery-readiness report.
const readyPath = "/api/v1/ready"

// Reasons GET /api/v1/ready gives for answering 503, in the order they are
// checked. They are the stable, machine-readable "code" of the response.
const (
	// ReadyNotActive means this instance is not the active one of an HA cluster:
	// a standby, or an instance between two roles.
	ReadyNotActive = "not_active"
	// ReadyDraining means a graceful shutdown refuses new runs while it waits for
	// the running ones.
	ReadyDraining = "draining"
	// ReadyStoreUnwritable means the metadata database cannot be written.
	ReadyStoreUnwritable = "store_unwritable"
	// ReadyDiskFull means the disk guard refuses new runs: the data directory is
	// below its free space minimum or a write failed because the disk is full.
	ReadyDiskFull = "disk_full"
)

// readyStoreTimeout bounds the write probe of the readiness check, so a wedged
// database fails the probe instead of hanging it.
const readyStoreTimeout = 2 * time.Second

// storeWriteChecker is implemented by stores that can probe whether they accept
// writes (*store.SQLiteStore).
type storeWriteChecker interface {
	// CheckWritable returns an error when the store cannot take a write.
	CheckWritable(ctx context.Context) error
}

// diskChecker is the disk guard the readiness check asks (*diskguard.Guard).
type diskChecker interface {
	// Check returns an error while the data directory is full or low on space.
	Check() error
}

// WithDiskGuard sets the disk guard GET /api/v1/ready consults; without it the
// disk is not part of the readiness check.
func WithDiskGuard(g diskChecker) Option {
	return func(s *Server) { s.diskGuard = g }
}

// readyBody is the data of the answer of GET /api/v1/ready.
type readyBody struct {
	// Ready is true for 200 and false for 503.
	Ready bool `json:"ready"`
	// Reason is one of the Ready* reasons when Ready is false.
	Reason string `json:"reason,omitempty"`
	// Role is the HA role ("active" outside HA mode).
	Role string `json:"role"`
	// Cluster is the HA cluster name, in HA mode.
	Cluster string `json:"cluster,omitempty"`
	// Version is the build version.
	Version string `json:"version,omitempty"`
}

// handleReady reports readiness to take traffic: 200 only when this instance is
// the active one (always, outside HA mode), is not draining, can write its
// metadata database and the disk guard accepts new runs. Otherwise 503 with the
// first failing reason as "code" (and in data.reason) and a Retry-After. It is
// the check a shared Service or load balancer routes by; /api/v1/health stays the
// liveness check.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	body := readyBody{Role: HARoleActive, Version: s.version}
	retry := 10
	if s.haStatus != nil {
		st := s.haStatus()
		body.Role, body.Cluster = st.Role, st.Cluster
		if st.RetryAfterSeconds > 0 {
			retry = st.RetryAfterSeconds
		}
	}
	reason, detail := s.notReady(r.Context(), body.Role)
	if reason == "" {
		body.Ready = true
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, body)
		return
	}
	body.Reason = reason
	writeReady503(w, body, detail, retry)
}

// notReady returns the first reason the instance with the HA role is not ready, and
// a human description of it, or "" when it is ready.
func (s *Server) notReady(ctx context.Context, role string) (reason, detail string) {
	if role != HARoleActive {
		return ReadyNotActive, "this instance is not the active one of its HA cluster (role " + role + ")"
	}
	if s.scheduler != nil && s.scheduler.Paused() {
		return ReadyDraining, "this instance is draining: new runs are refused until it shuts down"
	}
	if c, ok := s.metaStore.(storeWriteChecker); ok {
		pctx, cancel := context.WithTimeout(ctx, readyStoreTimeout)
		err := c.CheckWritable(pctx)
		cancel()
		if err != nil {
			return ReadyStoreUnwritable, "the metadata database is not writable: " + redact.Text(err.Error())
		}
	}
	if s.diskGuard != nil {
		if err := s.diskGuard.Check(); err != nil {
			return ReadyDiskFull, redact.Text(err.Error())
		}
	}
	return "", ""
}

// writeReady503 writes the refusal of the readiness check: the envelope of an
// error with the reason as its code and the body as its data.
func writeReady503(w http.ResponseWriter, body readyBody, detail string, retryAfter int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(apiResponse{Success: false, Data: body, Error: detail, Code: body.Reason})
}
