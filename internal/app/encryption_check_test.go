package app

import (
	"context"
	"log/slog"
	"maps"
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

	cases := []struct {
		name    string
		env     map[string]string
		markers []string // sources an earlier (buggy) import recorded
		enabled bool     // encryption currently on
		want    bool
	}{
		{"variable still set, switch lost", map[string]string{config.EnvEncryptionEnabled: "true", config.EnvEncryptionRecips: recipient}, buggyImport, false, true},
		{"variables removed, import recorded switch and recipients", nil, buggyImport, false, true},
		{"config.json import recorded", nil, []string{config.LegacyFileName + ":encryption.enabled", config.LegacyFileName + ":encryption.passphrase"}, false, true},
		{"variable says off", map[string]string{config.EnvEncryptionEnabled: "false"}, buggyImport, false, false},
		{"only the switch was recorded", nil, []string{config.EnvEncryptionEnabled}, false, false},
		{"never configured", nil, nil, false, false},
		{"encryption is on", nil, buggyImport, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memSettingsRepo{}
			svc, err := settings.NewService(ctx, repo, settings.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Recipients: &[]string{recipient}}}); err != nil {
				t.Fatal(err)
			}
			if tc.enabled {
				on := true
				if _, err = svc.Update(ctx, settings.Patch{Encryption: &settings.EncryptionPatch{Enabled: &on}}); err != nil {
					t.Fatal(err)
				}
			}
			if err = svc.MarkImported(ctx, tc.markers...); err != nil {
				t.Fatal(err)
			}
			legacy, err := config.LoadLegacy(t.TempDir(), envMap(tc.env))
			if err != nil {
				t.Fatal(err)
			}

			var logs strings.Builder
			logger := slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil))
			pub := &capturePublisher{}
			for range 3 { // every start runs the check; it must stay idempotent
				if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, svc, pub); err != nil {
					t.Fatal(err)
				}
			}
			warned := len(svc.Warnings()) == 1
			if warned != tc.want {
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
			again, err := settings.NewService(ctx, repo, settings.WithLogger(slog.New(slog.DiscardHandler)))
			if err != nil {
				t.Fatal(err)
			}
			if err = checkEncryptionAfterUpgrade(ctx, logger, legacy, again, pub); err != nil {
				t.Fatal(err)
			}
			if (len(again.Warnings()) == 1) != tc.want || pub.count() != wantEvents {
				t.Fatalf("after restart: warnings %v, alerts %d", again.Warnings(), pub.count())
			}
		})
	}
}

// TestAppRaisesEncryptionOffWarningAtStartup runs the check through New: an
// installation upgraded by a release that lost the encryption switch shows the
// warning after the next start, and enabling encryption clears it.
func TestAppRaisesEncryptionOffWarningAtStartup(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	key, err := settings.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
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

	second, err := New(cfg, nil, WithGetenv(noEnv))
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

	third, err := New(cfg, nil, WithGetenv(noEnv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = third.Close() })
	if w := third.settings.Warnings(); len(w) != 0 {
		t.Fatalf("warnings after enabling encryption = %+v", w)
	}
}
