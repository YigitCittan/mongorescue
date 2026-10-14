package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

const vaultPassword = "vault-pass-9f2"

// fakeCreds is a scripted connections.Credentials.
type fakeCreds struct {
	mu       sync.Mutex
	err      error
	calls    int
	holds    int
	released int
	cause    error
}

func (f *fakeCreds) Configured() bool { return true }

func (f *fakeCreds) Connection(_ context.Context, ref models.VaultRef) (vaultcreds.UserPassword, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return vaultcreds.UserPassword{}, f.err
	}
	return vaultcreds.UserPassword{Username: "v-" + ref.Role + ref.Path, Password: vaultPassword}, nil
}

func (f *fakeCreds) Hold(ctx context.Context, _ models.VaultRef) (context.Context, vaultcreds.UserPassword, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return ctx, vaultcreds.UserPassword{}, func() {}, f.err
	}
	f.holds++
	held, cancel := context.WithCancelCause(ctx)
	if f.cause != nil {
		cancel(f.cause)
	}
	return held, vaultcreds.UserPassword{Username: "held-user", Password: "held:p@ss/word"}, func() {
		f.mu.Lock()
		f.released++
		f.mu.Unlock()
		cancel(context.Canceled)
	}, nil
}

func vaultInput(name, uri string, ref models.VaultRef) connections.Input {
	src := models.CredentialsVault
	return connections.Input{Name: name, URI: uri, CredentialInput: connections.CredentialInput{CredentialSource: &src, Vault: &ref}}
}

func TestVaultConnectionNeedsAConfiguredVaultAndNoUserinfo(t *testing.T) {
	ctx := context.Background()
	plain, _, _ := newService(t)
	ref := models.VaultRef{Engine: models.VaultKV2, Path: "mongo/prod"}
	if _, err := plain.Create(ctx, vaultInput("p", "mongodb://db1:27017", ref)); !errors.Is(err, connections.ErrInvalid) ||
		!strings.Contains(err.Error(), "MONGORESCUE_VAULT_ADDR") {
		t.Fatalf("without Vault = %v", err)
	}
	svc, _, _ := newService(t, connections.WithCredentials(&fakeCreds{}))
	for _, uri := range []string{"mongodb://user:pw@db1:27017", "mongodb://user@db1:27017"} {
		if _, err := svc.Create(ctx, vaultInput("p", uri, ref)); !errors.Is(err, connections.ErrInvalid) {
			t.Errorf("uri with userinfo %q = %v", uri, err)
		}
	}
	if _, err := svc.Create(ctx, vaultInput("p", "mongodb://db1:27017", models.VaultRef{Engine: models.VaultAWS, Role: "x"})); !errors.Is(err, connections.ErrInvalid) {
		t.Errorf("aws engine for a connection = %v", err)
	}
	bad := models.CredentialSource("env")
	if _, err := svc.Create(ctx, connections.Input{Name: "p", URI: "mongodb://db1:27017", CredentialInput: connections.CredentialInput{CredentialSource: &bad}}); !errors.Is(err, connections.ErrInvalid) {
		t.Errorf("unknown source = %v", err)
	}
}

func TestVaultCredentialsAreResolvedAtUseAndNeverStored(t *testing.T) {
	ctx := context.Background()
	creds := &fakeCreds{}
	svc, st, p := newService(t, connections.WithCredentials(creds))
	c, err := svc.Create(ctx, vaultInput("prod", "mongodb://db1.internal:27017/?authSource=admin",
		models.VaultRef{Engine: models.VaultKV2, Path: "mongo/prod"}))
	if err != nil {
		t.Fatal(err)
	}
	if creds.calls != 0 {
		t.Fatal("Vault was read when the connection was saved")
	}
	if !c.FromVault() || c.Vault.Mount != "secret" || c.Vault.UsernameField != "username" {
		t.Fatalf("created = %+v", c)
	}
	resolved, err := svc.Resolve(ctx, c.ID)
	if err != nil || resolved.URI != "mongodb://v-mongo%2Fprod:"+vaultPassword+"@db1.internal:27017/?authSource=admin" {
		t.Fatalf("Resolve = %q, %v", resolved.URI, err)
	}
	res, err := svc.Test(ctx, c.ID)
	if err != nil || !res.OK || !strings.Contains(p.uri(), vaultPassword) {
		t.Fatalf("Test = %+v, %v (prober got %q)", res, err, p.uri())
	}
	if _, err = svc.Databases(ctx, c.ID, false); err != nil || !strings.Contains(p.uri(), vaultPassword) {
		t.Fatalf("Databases = %v", err)
	}
	// What is stored (the test result included) and what is returned holds no
	// credential.
	stored, err := st.GetConnection(ctx, c.ID)
	if err != nil || strings.Contains(stored.URI, vaultPassword) || strings.Contains(stored.URI, "@") || !stored.LastTestOK {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	list, _ := svc.List(ctx)
	got, _ := svc.Get(ctx, c.ID)
	for _, v := range []any{c, list, got, stored, res} {
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), vaultPassword) {
			t.Fatalf("%s holds the password from Vault", raw)
		}
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"credential_source":"vault"`) || !strings.Contains(string(raw), `"path":"mongo/prod"`) {
		t.Fatalf("response lacks the vault reference: %s", raw)
	}
}

func TestVaultFailureFailsUseWithoutFallback(t *testing.T) {
	ctx := context.Background()
	creds := &fakeCreds{}
	svc, st, p := newService(t, connections.WithCredentials(creds))
	c, err := svc.Create(ctx, vaultInput("prod", "mongodb://db1:27017", models.VaultRef{Engine: models.VaultKV2, Path: "mongo"}))
	if err != nil {
		t.Fatal(err)
	}
	creds.err = vaultcreds.ErrUnavailable
	if _, err = svc.Resolve(ctx, c.ID); !errors.Is(err, connections.ErrCredentials) || !errors.Is(err, vaultcreds.ErrUnavailable) {
		t.Fatalf("Resolve while Vault is down = %v", err)
	}
	res, err := svc.Test(ctx, c.ID)
	if err != nil || res.OK || !strings.Contains(res.Error, "vault is unavailable") || p.uri() != "" {
		t.Fatalf("Test while Vault is down = %+v, %v (prober got %q)", res, err, p.uri())
	}
	if stored, _ := st.GetConnection(ctx, c.ID); stored.LastTestOK || stored.LastTestError == "" {
		t.Fatalf("stored test = %+v", stored)
	}
}

func TestHoldLeasesDynamicCredentialsForARun(t *testing.T) {
	ctx := context.Background()
	creds := &fakeCreds{}
	svc, _, _ := newService(t, connections.WithCredentials(creds))
	stored, err := svc.Create(ctx, connections.Input{Name: "static", URI: secretURI})
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Create(ctx, vaultInput("dyn", "mongodb://db1:27017/?authSource=admin", models.VaultRef{Engine: models.VaultDatabase, Role: "backup"}))
	if err != nil {
		t.Fatal(err)
	}
	// A stored connection is not held.
	held, uri, release, err := svc.Hold(ctx, stored.ID, secretURI)
	if err != nil || uri != secretURI || held != ctx || creds.holds != 0 {
		t.Fatalf("Hold of a stored connection = %q, %v", uri, err)
	}
	release()
	// The run's URI (with its read preference) gets the leased credentials.
	runURI := "mongodb://v-backup:old@db1:27017/?authSource=admin&readPreference=secondary"
	held, uri, release, err = svc.Hold(ctx, c.ID, runURI)
	if err != nil || uri != "mongodb://held-user:held%3Ap%40ss%2Fword@db1:27017/?authSource=admin&readPreference=secondary" || creds.holds != 1 {
		t.Fatalf("Hold = %q, %v", uri, err)
	}
	release()
	if creds.released != 1 || held.Err() == nil {
		t.Fatalf("released %d, held ctx %v", creds.released, held.Err())
	}
	creds.err = vaultcreds.ErrRefused
	if _, _, _, err = svc.Hold(ctx, c.ID, runURI); !errors.Is(err, connections.ErrCredentials) {
		t.Fatalf("Hold while Vault refuses = %v", err)
	}
}

func TestUpdateSwitchesCredentialSource(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newService(t, connections.WithCredentials(&fakeCreds{}))
	c, err := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Test(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	// Switching to Vault needs the credentials removed from the URI.
	in := vaultInput("prod", c.URI, models.VaultRef{Engine: models.VaultKV2, Path: "p"})
	if _, err = svc.Update(ctx, c.ID, in); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("switch with the stored password = %v", err)
	}
	in.URI = "mongodb://db1.internal:27017,db2.internal:27017/?replicaSet=rs0&authSource=admin"
	up, err := svc.Update(ctx, c.ID, in)
	if err != nil || !up.FromVault() || up.LastTestAt != nil {
		t.Fatalf("switch to Vault = %+v, %v", up, err)
	}
	// Omitted fields keep the source; "stored" drops the reference.
	if up, err = svc.Update(ctx, c.ID, connections.Input{Name: "prod2", URI: in.URI}); err != nil || !up.FromVault() || up.Vault.Path != "p" {
		t.Fatalf("rename = %+v, %v", up, err)
	}
	src := models.CredentialsStored
	up, err = svc.Update(ctx, c.ID, connections.Input{Name: "prod2", URI: in.URI, CredentialInput: connections.CredentialInput{CredentialSource: &src}})
	if err != nil || up.FromVault() || !up.Vault.IsZero() || up.CredentialSource != "" {
		t.Fatalf("switch back = %+v, %v", up, err)
	}
}

func TestTestURIWithVaultCredentials(t *testing.T) {
	ctx := context.Background()
	svc, _, p := newService(t, connections.WithCredentials(&fakeCreds{}))
	src := models.CredentialsVault
	ref := models.VaultRef{Engine: models.VaultKV2, Path: "form"}
	res, err := svc.TestURIWithCredentials(ctx, "mongodb://db9:27017", "", models.ReadPreference{}, connections.TLSInput{},
		connections.CredentialInput{CredentialSource: &src, Vault: &ref})
	if err != nil || !res.OK || !strings.Contains(p.uri(), vaultPassword) {
		t.Fatalf("form test = %+v, %v", res, err)
	}
	// The stored reference of a connection is never used for other hosts.
	c, err := svc.Create(ctx, vaultInput("prod", "mongodb://db1:27017", models.VaultRef{Engine: models.VaultKV2, Path: "prod"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.TestURIWithTLS(ctx, "mongodb://db1:27017", c.ID, models.ReadPreference{}, connections.TLSInput{}); err != nil ||
		!strings.Contains(p.uri(), "v-prod:") {
		t.Fatalf("re-test of the stored hosts = %v, %q", err, p.uri())
	}
	if _, err = svc.TestURIWithTLS(ctx, "mongodb://elsewhere:27017", c.ID, models.ReadPreference{}, connections.TLSInput{}); err != nil ||
		strings.Contains(p.uri(), vaultPassword) {
		t.Fatalf("test of other hosts = %v, %q", err, p.uri())
	}
}
