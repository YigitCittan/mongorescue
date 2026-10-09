package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/diskguard"
	"github.com/yigitcittan/mongorescue/internal/operations"
)

// A run refused for space answers 503 with Retry-After; other 503s do not.
func TestLowSpaceAnswersRetryAfter(t *testing.T) {
	s := &Server{logger: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	refused := fmt.Errorf("%w: %w: 50 MiB free in /data, below the minimum of 100 MiB", operations.ErrUnavailable, diskguard.ErrLowSpace)
	s.writeOperationError(rec, refused)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("low space: %d, Retry-After %q; want 503 and 30", rec.Code, rec.Header().Get("Retry-After"))
	}
	rec = httptest.NewRecorder()
	s.writeOperationError(rec, fmt.Errorf("%w: busy", operations.ErrUnavailable))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "" {
		t.Fatalf("other 503: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}
