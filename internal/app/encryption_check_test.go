package app

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/events"
	"github.com/yigitcittan/mongorescue/internal/settings"
)

// memSettingsRepo is an in-memory settings.Repository.
type memSettingsRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *memSettingsRepo) LoadSettings(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.values), nil
}

func (m *memSettingsRepo) SaveSettings(_ context.Context, v map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]string{}
	}
	maps.Copy(m.values, v)
	return nil
}

// capturePublisher records published events.
type capturePublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (c *capturePublisher) Publish(_ context.Context, e events.Event) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return true
}

func (c *capturePublisher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// deliver hands the captured events at index from and later to h, as the bus does.
func (c *capturePublisher) deliver(ctx context.Context, from int, h events.Handler) {
	c.mu.Lock()
	pending := append([]events.Event(nil), c.events[from:]...)
	c.mu.Unlock()
	for _, e := range pending {
		h(ctx, e)
	}
}

func newCheckService(t *testing.T, repo *memSettingsRepo) *settings.Service {
	t.Helper()
	svc, err := settings.NewService(context.Background(), repo, settings.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestEncryptionOffAfterUpgradeCheck covers the startup check that catches
// installations whose encryption switch was lost by the import of v0.7.1 and
// earlier.
func TestEncryptionOffAfterUpgradeCheck(t *testing.T) {
	ctx := context.Background()
	_, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	buggyImport := []string{config.EnvEncryptionEnabled, config.EnvEncryptionRecips}
	fileImport := []string{config.LegacyFileName + ":encryption.enabled", config.LegacyFileName + ":encryption.passphrase"}

	cases := []struct {
		name    string
		env     map[string]string
		file    string   // config.json content, if any
		markers []string // sources an earlier (buggy) import recorded
		enabled bool     // encryption currently on
		want    bool
	}{
		{"variable still says on, switch lost", map[string]string{config.EnvEncryptionEnabled: "true", config.EnvEncryptionRecips: recipient}, "", buggyImport, false, true},
		{"config.json still says on, switch lost", nil, `{"encryption": {"enabled": true, "passphrase": "kept"}}`, fileImport, false, true},
		// The markers were also written for ENCRYPTION_ENABLED=false: without the
		// switch value, they must not raise an alarm.
		{"variables removed, markers only", nil, "", buggyImport, false, false},
		{"config.json removed, markers only", nil, "", fileImport, false, false},
		{"variable says off, passphrase kept", map[string]string{config.EnvEncryptionEnabled: "false", config.EnvEncryptionPass: "a kept passphrase"},
			"", []string{config.EnvEncryptionEnabled, config.EnvEncryptionPass}, false, false},
		{"config.json says off, passphrase kept", nil, `{"encryption": {"enabled": false, "passphrase": "kept"}}`, fileImport, false, false},
		{"variable says on, no key imported", map[string]string{config.EnvEncryptionEnabled: "true"}, "", []string{config.EnvEncryptionEnabled}, false, false},
		{"never configured", nil, "", nil, false, false},
		{"encryption is on", map[string]string{config.EnvEncryptionEnabled: "true", config.EnvEncryptionRecips: recipient}, "", buggyImport, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memSettingsRepo{}
			svc := newCheckService(t, repo)
			if _, err := svc.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Recipients: &[]string{recipient}}}); err != nil {
				t.Fatal(err)
			}
			if tc.enabled {
				on := true
				if _, err := svc.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Enabled: &on}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.MarkImported(ctx, tc.markers...); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, config.LegacyFileName), []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			legacy, err := config.LoadLegacy(dir, envMap(tc.env))
			if err != nil {
				t.Fatal(err)
			}

			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil))
			pub := &capturePublisher{}
			watch := watchEncryptionOffAlert(logger, svc)
			for range 3 { // every start runs the check; it must stay idempotent
				before := pub.count()
				if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, svc, pub); err != nil {
					t.Fatal(err)
				}
				pub.deliver(ctx, before, watch) // the bus runs after the check
			}
			if warned := len(svc.Warnings()) == 1; warned != tc.want {
				t.Fatalf("warning = %v; want %v", warned, tc.want)
			}
			if svc.Current().Encryption.Enabled != tc.enabled {
				t.Fatal("the check must never change the encryption switch")
			}
			wantEvents, wantLogs := 0, 0
			if tc.want {
				wantEvents, wantLogs = 1, 3
			}
			if pub.count() != wantEvents {
				t.Fatalf("published %d alerts; want %d", pub.count(), wantEvents)
			}
			if pub.count() == 1 && pub.events[0].Type != events.EncryptionOffAfterUpgrade {
				t.Fatalf("event = %+v", pub.events[0])
			}
			if n := strings.Count(logs.String(), "level=WARN msg=\"Encryption was enabled in your previous configuration"); n != wantLogs {
				t.Fatalf("logged the warning %d times; want %d:\n%s", n, wantLogs, logs.String())
			}

			// A restart keeps an active warning without a second alert.
			again := newCheckService(t, repo)
			if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, again, pub); err != nil {
				t.Fatal(err)
			}
			if (len(again.Warnings()) == 1) != tc.want || pub.count() != wantEvents {
				t.Fatalf("after restart: warnings %v, alerts %d", again.Warnings(), pub.count())
			}
		})
	}
}

// TestEncryptionOffAlertIsResentUntilDelivered stops the process after the check but
// before the event bus delivered the alert: the next start sends it again, and once
// it was delivered no start sends it any more.
func TestEncryptionOffAlertIsResentUntilDelivered(t *testing.T) {
	ctx := context.Background()
	_, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	repo := &memSettingsRepo{}
	svc := newCheckService(t, repo)
	if _, err = svc.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Recipients: &[]string{recipient}}}); err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkImported(ctx, config.EnvEncryptionEnabled, config.EnvEncryptionRecips); err != nil {
		t.Fatal(err)
	}
	legacy, err := config.LoadLegacy(t.TempDir(), envMap(map[string]string{config.EnvEncryptionEnabled: "true", config.EnvEncryptionRecips: recipient}))
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	pub := &capturePublisher{}

	// First start: published, then the process dies before the bus ran.
	if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, svc, pub); err != nil {
		t.Fatal(err)
	}
	// Second start: still pending, published again and this time delivered.
	second := newCheckService(t, repo)
	if !second.EncryptionOffAlertPending() {
		t.Fatal("an alert that never reached the bus must stay pending")
	}
	if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, second, pub); err != nil {
		t.Fatal(err)
	}
	if pub.count() != 2 {
		t.Fatalf("alerts = %d; want the undelivered one sent again", pub.count())
	}
	pub.deliver(ctx, 1, watchEncryptionOffAlert(logger, second))
	pub.deliver(ctx, 1, watchEncryptionOffAlert(logger, second)) // idempotent

	// Third start: delivered before, so no new alert, but the warning stays.
	third := newCheckService(t, repo)
	if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, third, pub); err != nil {
		t.Fatal(err)
	}
	if pub.count() != 2 || third.EncryptionOffAlertPending() || len(third.Warnings()) != 1 {
		t.Fatalf("after delivery: alerts %d, pending %v, warnings %v", pub.count(), third.EncryptionOffAlertPending(), third.Warnings())
	}
}

// TestAppRaisesEncryptionOffWarningAtStartup runs the check through New: an
// installation upgraded by a release that lost the encryption switch, still
// configured with MONGORESCUE_ENCRYPTION_ENABLED=true, shows the warning, and
// enabling encryption clears it.
func TestAppRaisesEncryptionOffWarningAtStartup(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	key, err := settings.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	env := envMap(map[string]string{config.EnvEncryptionEnabled: "true", config.EnvEncryptionRecips: key.Recipient})
	first, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	// What the buggy import left behind: keys imported, switch off, sources recorded.
	if _, err = first.settings.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Recipients: &[]string{key.Recipient}}}); err != nil {
		t.Fatal(err)
	}
	if err = first.settings.MarkImported(ctx, config.EnvEncryptionEnabled, config.EnvEncryptionRecips); err != nil {
		t.Fatal(err)
	}
	if len(first.settings.Warnings()) != 0 {
		t.Fatal("no warning before the check ran")
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg, nil, WithGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	if w := second.settings.Warnings(); len(w) != 1 || w[0].ID != settings.WarningEncryptionOff {
		t.Fatalf("warnings = %+v; want the encryption warning", w)
	}
	if second.settings.Current().Encryption.Enabled {
		t.Fatal("encryption must not be enabled automatically")
	}
	on := true
	if _, err = second.settings.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Enabled: &on}}); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}

	third, err := New(cfg, nil, WithGetenv(env))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	if w := third.settings.Warnings(); len(w) != 0 {
		t.Fatalf("warnings after enabling encryption = %+v", w)
	}
}
