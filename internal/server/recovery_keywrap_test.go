package server

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/audit"
	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/keywrap"
	"github.com/yigitcittan/mongorescue/internal/keywrap/keywraptest"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/recoverykit"
	"github.com/yigitcittan/mongorescue/internal/settings"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
)

// kitFiles decrypts a downloaded kit and returns its file names and contents.
func kitFiles(t *testing.T, sealed []byte) map[string]string {
	t.Helper()
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Passphrase: kitPassphrase})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := dec.Decrypt(bytes.NewReader(sealed))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	tr := tar.NewReader(plain)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out[hdr.Name] = string(body)
	}
}

// TestRecoveryKitOfAWrappedKey checks that with key wrapping on the status says so,
// a kit carries secret.key.wrapped only, and the plaintext key is handed out only
// when asked for, which the audit entry records.
func TestRecoveryKitOfAWrappedKey(t *testing.T) {
	srv, _, _ := setupTestServer(t)
	st := storetest.New(t)
	authSvc := newTestAuth(t, st, "")
	settingsSvc := newTestSettings(t, st, settings.Defaults().Security)
	targetSvc := targets.NewService(st, nil, t.TempDir())
	dir := t.TempDir()
	spec := keywrap.Spec{Provider: keywrap.ProviderAzureKeyVault, KeyURI: "https://v.vault.azure.net/keys/mongorescue"}
	kms := keywraptest.New("kms")
	loaded, err := keywrap.Load(context.Background(), keywrap.LoadConfig{DataDir: dir, Spec: spec, Wrapper: kms,
		Retry: keywrap.Retry{Attempts: 1}, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	kit, err := recoverykit.New(recoverykit.Config{SecretKey: loaded.Key, Settings: settingsSvc, Targets: targetSvc, WorkFactor: 10,
		KeyWrap: &recoverykit.KeyWrap{Spec: spec, KeyID: kms.KeyID(), CurrentFile: filepath.Join(dir, keywrap.WrappedKeyFileName),
			ReadKey: keywrap.NewCodec(spec, kms).ReadKey}})
	if err != nil {
		t.Fatal(err)
	}
	auditSvc := audit.NewService(st, slog.New(slog.DiscardHandler))
	full := NewServer(bootConfig(), st, srv.backupEngine, srv.restoreEngine, srv.storageDriver, srv.scheduler, nil, nil,
		WithAuth(authSvc), WithSettings(settingsSvc), WithStorageTargets(targetSvc), WithRecoveryKit(kit), WithAudit(auditSvc))
	f := &authFixture{h: full.Handler(), auth: authSvc}
	b := f.browser(t)
	b.setup(f)

	var status recoveryKitStatus
	decodeData(t, b.do("GET", "/api/v1/recovery-kit", nil, nil), &status)
	if !status.KeyWrapped {
		t.Fatal("status key_wrapped = false")
	}

	rec := b.do("POST", "/api/v1/recovery-kit", kitBody(testPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}
	files := kitFiles(t, rec.Body.Bytes())
	if _, ok := files[recoverykit.SecretKeyName]; ok {
		t.Fatal("the default kit carries the plaintext secret.key")
	}
	if _, ok := files[recoverykit.WrappedSecretKeyName]; !ok {
		t.Fatal("the default kit lacks secret.key.wrapped")
	}

	rec = b.do("POST", "/api/v1/recovery-kit", map[string]any{"passphrase": kitPassphrase, "current_password": testPassword,
		"plaintext_secret_key": true}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("plaintext download: %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := kitFiles(t, rec.Body.Bytes())[recoverykit.SecretKeyName]; !ok {
		t.Fatal("the plaintext kit lacks secret.key")
	}
	entries, err := auditSvc.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var plaintext, ok int
	for _, e := range entries {
		if e.Tool != recoveryKitRoute || e.Result != audit.ResultOK {
			continue
		}
		ok++
		if strings.Contains(string(e.Arguments), `"plaintext":"true"`) {
			plaintext++
		}
	}
	if ok != 2 || plaintext != 1 {
		t.Fatalf("audit: %d downloads, %d with the plaintext key; want 2 and 1", ok, plaintext)
	}

	// With the two-person rule, the plaintext key needs a second administrator.
	if _, err = authSvc.CreateUser(context.Background(), auth.SystemPrincipal(), "bob", testPassword, auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if rec = b.do("PUT", "/api/v1/settings", map[string]any{"security": map[string]any{"require_second_approver": true}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("turn the two-person rule on: %d %s", rec.Code, rec.Body)
	}
	plainBody := func(approvalID string) map[string]any {
		return map[string]any{"passphrase": kitPassphrase, "current_password": testPassword, "plaintext_secret_key": true, "approval_id": approvalID}
	}
	rec = b.do("POST", "/api/v1/recovery-kit", plainBody(""), nil)
	var pending struct {
		Data struct {
			ApprovalRequired bool             `json:"approval_required"`
			Approval         *models.Approval `json:"approval"`
		} `json:"data"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &pending); err != nil || rec.Code != http.StatusAccepted || !pending.Data.ApprovalRequired ||
		pending.Data.Approval == nil || pending.Data.Approval.Action != models.ApprovalPlaintextRecoveryKit {
		t.Fatalf("plaintext download with the rule on: %d %s; want 202 with an approval request", rec.Code, rec.Body)
	}
	id := pending.Data.Approval.ID
	if rec = b.do("POST", "/api/v1/recovery-kit", plainBody(id), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("plaintext download before the approval: %d %s; want 403", rec.Code, rec.Body)
	}
	bob := f.browser(t)
	bob.session(bob.login("bob", testPassword))
	if rec = bob.do("POST", "/api/v1/approvals/"+id+"/approve", nil, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"approved"`) {
		t.Fatalf("approval by bob: %d %s", rec.Code, rec.Body)
	}
	rec = b.do("POST", "/api/v1/recovery-kit", plainBody(id), nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("approved plaintext download: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if _, ok := kitFiles(t, rec.Body.Bytes())[recoverykit.SecretKeyName]; !ok {
		t.Fatal("the approved plaintext kit lacks secret.key")
	}
	if rec = b.do("POST", "/api/v1/recovery-kit", plainBody(id), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("second use of the approval: %d %s; want 403", rec.Code, rec.Body)
	}
	// The wrapped kit needs no approval.
	if rec = b.do("POST", "/api/v1/recovery-kit", kitBody(testPassword), nil); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("wrapped download with the rule on: %d %v", rec.Code, rec.Header())
	}
}
