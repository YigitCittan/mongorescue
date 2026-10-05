package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMonitoringDefaultsAndValidation(t *testing.T) {
	svc := newSvc(t, &memRepo{})
	if m := svc.Current().Monitoring; m.HeartbeatURL != "" || m.HeartbeatInterval.Std() != DefaultHeartbeatInterval {
		t.Fatalf("defaults = %+v", m)
	}
	ctx := context.Background()
	for _, p := range []MonitoringPatch{
		{HeartbeatInterval: ptr(Duration(59 * time.Second))},
		{HeartbeatInterval: ptr(Duration(61 * time.Minute))},
		{HeartbeatURL: ptr("ftp://hc.example.com/x")},
		{HeartbeatURL: ptr("https://user:pw@hc.example.com/x")},
		{HeartbeatURL: ptr("https://hc.example.com/x\nX-Evil: 1")},
		{HeartbeatURL: ptr("https://hc.example.com/" + strings.Repeat("a", 2100))},
	} {
		if _, err := svc.Update(ctx, Patch{Monitoring: &p}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Update(%+v) = %v; want ErrInvalid", p, err)
		}
	}
	if _, err := svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr(SecretMask)}}); !errors.Is(err, ErrMaskedSecret) {
		t.Fatalf("masked URL without a stored one = %v", err)
	}
	if _, err := svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatInterval: ptr(Duration(time.Hour))}}); err != nil {
		t.Fatalf("1h interval: %v", err)
	}
}

// TestHeartbeatURLIsMaskedAndKept proves the heartbeat URL never leaves the service
// beyond its origin, is sealed at rest, survives being sent back masked and must be
// entered again for another host.
func TestHeartbeatURLIsMaskedAndKept(t *testing.T) {
	repo := &memRepo{}
	svc := newSvc(t, repo)
	ctx := context.Background()
	const url = "https://hc-ping.com/0b5e-secret-uuid"
	masked, err := svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr(url), HeartbeatInterval: ptr(Duration(2 * time.Minute))}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(masked)
	if strings.Contains(string(raw), "secret-uuid") {
		t.Fatalf("masked settings leak the heartbeat URL: %s", raw)
	}
	if masked.Monitoring.HeartbeatURL != "https://hc-ping.com/"+SecretMask || masked.Monitoring.HeartbeatInterval.Std() != 2*time.Minute {
		t.Fatalf("masked monitoring = %+v", masked.Monitoring)
	}
	if !IsSecret(KeyHeartbeatURL) || IsSecret(KeyHeartbeatInterval) {
		t.Fatal("the heartbeat URL must be sealed at rest, the interval not")
	}

	// Sending the masked value (or the bare mask) back keeps the URL.
	for _, back := range []string{masked.Monitoring.HeartbeatURL, SecretMask} {
		if _, err = svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr(back)}}); err != nil {
			t.Fatalf("send back %q: %v", back, err)
		}
		if got := svc.Current().Monitoring.HeartbeatURL; got != url {
			t.Fatalf("kept URL = %q", got)
		}
	}
	// A masked value for another host is refused: the URL must be entered again.
	if _, err = svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr("https://other.example.com/" + SecretMask)}}); !errors.Is(err, ErrSecretReentry) {
		t.Fatalf("masked URL for another host = %v; want ErrSecretReentry", err)
	}
	if got := svc.Current().Monitoring.HeartbeatURL; got != url {
		t.Fatalf("URL after a refused change = %q", got)
	}
	// A full new URL replaces it, "" turns the heartbeat off.
	if _, err = svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr("https://other.example.com/ping/x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Update(ctx, Patch{Monitoring: &MonitoringPatch{HeartbeatURL: ptr("")}}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Current().Monitoring.HeartbeatURL; got != "" {
		t.Fatalf("URL after clearing = %q", got)
	}
}
