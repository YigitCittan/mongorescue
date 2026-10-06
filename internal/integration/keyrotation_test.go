//go:build integration

package integration

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/app"
	"github.com/yigitcittan/mongorescue/internal/config"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/reencrypt"
)

const rotationPassword = "a long rotation admin password"

// rotationApp starts the application on cfg and returns a signed-in client.
func rotationApp(t *testing.T, cfg *config.Config, logger *slog.Logger, setup bool) (*app.App, *apiClient) {
	t.Helper()
	a, err := app.New(cfg, logger, app.WithGetenv(func(string) string { return "" }))
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if err = a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	api := newAPIClient(t, srv.URL)
	if setup {
		api.data("POST", "/api/v1/setup", map[string]string{"setup_code": a.SetupCode(), "username": "admin", "password": rotationPassword}, http.StatusCreated, nil)
	} else {
		api.data("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": rotationPassword}, http.StatusOK, nil)
	}
	api.csrf = api.me()
	return a, api
}

// runJob runs job id now and waits for its backup to complete.
func runJob(t *testing.T, api *apiClient, id string) *models.BackupRecord {
	t.Helper()
	var run models.BackupRecord
	api.data("POST", "/api/v1/jobs/"+id+"/run", nil, http.StatusAccepted, &run)
	for deadline := time.Now().Add(opTimeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var list []*models.BackupRecord
		api.data("GET", "/api/v1/backups?database="+run.Database, nil, http.StatusOK, &list)
		for _, b := range list {
			if b.ID != run.ID {
				continue
			}
			switch b.Status {
			case models.StatusCompleted:
				return b
			case models.StatusFailed, models.StatusCancelled:
				t.Fatalf("backup %s: %s %s", b.ID, b.Status, b.ErrorMessage)
			}
		}
	}
	t.Fatalf("backup %s never completed", run.ID)
	return nil
}

// restoreBackup restores id into a safe clone and returns the restored database.
func restoreBackup(t *testing.T, api *apiClient, id string) string {
	t.Helper()
	var accepted models.RestoreRecord
	api.data("POST", "/api/v1/restore", models.RestoreRequest{BackupID: id}, http.StatusAccepted, &accepted)
	for deadline := time.Now().Add(opTimeout); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var list []*models.RestoreRecord
		api.data("GET", "/api/v1/restores", nil, http.StatusOK, &list)
		for _, r := range list {
			if r.ID != accepted.ID || r.Status == models.RestoreStatusInProgress {
				continue
			}
			if r.Status != models.RestoreStatusCompleted {
				t.Fatalf("restore of %s: %s %s", id, r.Status, r.ErrorMessage)
			}
			return r.TargetDatabase
		}
	}
	t.Fatalf("restore of %s never finished", id)
	return ""
}

// TestKeyRotationEndToEnd backs up, rotates secret.key and the backup encryption key
// (re-encrypting the existing backup), restarts the application with the rotated
// key, then backs up and restores the old and the new backup.
func TestKeyRotationEndToEnd(t *testing.T) {
	env := requireMongo(t)
	db := seedSource(t, env, "keyrot")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	logger, logs := captureLogger()

	first, api := rotationApp(t, cfg, logger, true)
	var key struct{ Identity, Recipient string }
	api.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &key)
	api.data("PUT", "/api/v1/settings", map[string]any{"encryption": map[string]any{
		"enabled": true, "recipients": []string{key.Recipient}, "identity": key.Identity,
	}}, http.StatusOK, nil)
	var conn models.Connection
	api.data("POST", "/api/v1/connections", map[string]string{"name": "keyrot", "uri": env.URI}, http.StatusCreated, &conn)
	var job models.Job
	api.data("POST", "/api/v1/jobs", models.Job{Name: "keyrot", Database: db, CronExpression: "@daily", Enabled: true,
		Gzip: true, ConnectionID: conn.ID}, http.StatusCreated, &job)
	before := runJob(t, api, job.ID)

	// secret.key: everyone signs in again afterwards.
	var rotated struct {
		NewFingerprint string `json:"new_fingerprint"`
		SignInAgain    bool   `json:"sign_in_again"`
	}
	api.data("POST", "/api/v1/security/rotate-secret-key", map[string]string{"current_password": rotationPassword}, http.StatusOK, &rotated)
	if !rotated.SignInAgain || rotated.NewFingerprint == "" {
		t.Fatalf("rotation = %+v", rotated)
	}
	if code, _ := api.do("GET", "/api/v1/jobs", nil); code != http.StatusUnauthorized {
		t.Fatalf("session after the rotation: %d; want 401", code)
	}
	api.data("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": rotationPassword}, http.StatusOK, nil)
	api.csrf = api.me()

	// The backup encryption key, re-encrypting the existing backup.
	api.data("POST", "/api/v1/encryption/rotate", map[string]bool{"reencrypt": true}, http.StatusOK, nil)
	var job2 reencrypt.State
	for deadline := time.Now().Add(opTimeout); time.Now().Before(deadline); time.Sleep(300 * time.Millisecond) {
		api.data("GET", "/api/v1/encryption/reencryption", nil, http.StatusOK, &job2)
		if job2.Status != reencrypt.StatusRunning {
			break
		}
	}
	if job2.Status != reencrypt.StatusCompleted || job2.Done != 1 {
		t.Fatalf("re-encryption = %+v", job2)
	}
	first.Stop()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart with the rotated secret.key.
	second, api := rotationApp(t, cfg, logger, false)
	t.Cleanup(func() { second.Stop(); _ = second.Close() })
	after := runJob(t, api, job.ID)
	for _, b := range []*models.BackupRecord{before, after} {
		restored := restoreBackup(t, api, b.ID)
		if !strings.HasPrefix(restored, db+"_rescue_") {
			t.Fatalf("restored into %q", restored)
		}
		if got := env.count(t, restored, "orders"); got != ordersCount {
			t.Fatalf("restored orders of %s = %d; want %d", b.ID, got, ordersCount)
		}
	}
	assertNoSecret(t, env.Password, "logs", logs.String())
}
