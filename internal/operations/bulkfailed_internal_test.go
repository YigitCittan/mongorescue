package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestBulkFailedLogsNoRecordFields checks that a failed bulk item logs the error's
// message and the caller's kind and user ID only: a pending approval (which names
// its requester's API key) is an expected outcome and not logged at all, and an
// unexpected error is logged by its message alone, the caller by kind and user ID.
func TestBulkFailedLogsNoRecordFields(t *testing.T) {
	const keyName = "ci-deploy-key-91f2"
	var buf bytes.Buffer
	s := &Service{logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Method: auth.MethodAPIKey, APIKeyID: "key_1", APIKeyName: keyName})
	pending := &ApprovalPendingError{Approval: &models.Approval{ID: "apr_1", Summary: "unpin backup b1",
		RequestedBy: "API key " + keyName, ExpiresAt: time.Now().Add(time.Hour)}}

	res := s.failed(ctx, pending)
	if res.OK || !strings.Contains(res.Error, "apr_1") {
		t.Fatalf("pending approval result = %+v", res)
	}
	if buf.Len() != 0 {
		t.Fatalf("a pending approval was logged: %s", buf.String())
	}

	res = s.failed(ctx, fmt.Errorf("storage exploded: %w", errors.New("disk full")))
	if res.Error != "internal error (see the server log)" {
		t.Fatalf("unexpected error result = %+v", res)
	}
	out := buf.String()
	if !strings.Contains(out, "bulk item failed") || !strings.Contains(out, `"actor_kind"`) {
		t.Fatalf("log = %s", out)
	}
	if strings.Contains(out, keyName) || strings.Contains(out, "key_1") {
		t.Fatalf("log holds the API key: %s", out)
	}
}
