package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/notify"
)

// TestStepDownAlertIsDelivered checks that an active instance that loses the lease
// delivers ha.stepped_down to its channels itself, before its App closes, and counts
// the step-down.
func TestStepDownAlertIsDelivered(t *testing.T) {
	t.Parallel() // independent: own data directories, HA targets and ports
	ctx := context.Background()
	var (
		mu     sync.Mutex
		bodies []string
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()

	sup := bootstrappedSupervisor(t, haTestConfig(t, t.TempDir(), haTestKey(t)))
	sup.rt.mgr().Step(ctx)
	if sup.rt.mgr().Role() != ha.RoleActive {
		t.Fatal("did not acquire the lease")
	}
	app, err := sup.open(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.notifications.CreateChannel(ctx, &notify.Channel{Name: "ops", Type: notify.ChannelWebhook, Enabled: true,
		Webhook: &notify.WebhookConfig{URL: hook.URL}}); err != nil {
		t.Fatal(err)
	}
	if err = sup.loseLease(app, 1); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], string("ha.stepped_down")) {
		t.Fatalf("webhook received %q; want one ha.stepped_down", bodies)
	}
	if st := sup.rt.metricsState(); st.StepDowns != 1 {
		t.Fatalf("step-downs = %d; want 1", st.StepDowns)
	}
}
