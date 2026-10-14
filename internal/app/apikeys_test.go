package app

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// TestTurningOnAMaximumKeyLifetimeGivesKeysADeadline proves the wiring of the API
// key expiry: the settings are the policy of the auth service, changing them
// kicks the monitor, and its check gives existing keys a deadline and sets the
// gauges.
func TestTurningOnAMaximumKeyLifetimeGivesKeysADeadline(t *testing.T) {
	application, err := New(testConfig(t), nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ctx := context.Background()
	res, err := application.auth.Setup(ctx, "192.0.2.1", application.auth.SetupCode(), "admin", "a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := application.auth.AuthenticateSession(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = application.auth.CreateAPIKey(ctx, admin, "old", auth.ScopeRead); err != nil {
		t.Fatal(err)
	}

	days := 3
	if _, err = application.settings.Update(ctx, settings.Patch{Security: &settings.SecurityPatch{APIKeyMaxLifetimeDays: &days}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-application.keyExpiry.kick:
	default:
		t.Fatal("changing the maximum lifetime did not kick the expiry monitor")
	}
	if _, _, err = application.auth.CreateAPIKey(ctx, admin, "new", auth.ScopeRead); err == nil {
		t.Fatal("a key without an expiry was created under a maximum lifetime")
	}
	before := time.Now()
	application.keyExpiry.check(ctx)
	keys, err := application.auth.ListAPIKeys(ctx, admin)
	if err != nil || len(keys) != 1 || keys[0].ExpiresAt == nil {
		t.Fatalf("keys = %+v, %v; want the old key with a deadline", keys, err)
	}
	if got := keys[0].ExpiresAt.Sub(before); got < 3*24*time.Hour-time.Minute || got > 3*24*time.Hour+time.Minute {
		t.Errorf("deadline in %v; want 3 days", got)
	}
	rec := httptest.NewRecorder()
	application.metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if body := rec.Body.String(); !strings.Contains(body, "mongorescue_api_keys_expiring 1") || !strings.Contains(body, "mongorescue_api_keys_expired 0") {
		t.Errorf("metrics do not report the expiring key:\n%s", body)
	}
}
