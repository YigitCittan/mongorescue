//go:build integration

package integration

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
	"github.com/yigitcittan/mongorescue/internal/targets"
	"github.com/yigitcittan/mongorescue/internal/vault"
	"github.com/yigitcittan/mongorescue/internal/vaultcreds"
)

// envVaultMongoHost is the host:port of the test MongoDB as the Vault container
// reaches it (a shared Docker network), for the database secrets engine.
const envVaultMongoHost = "MONGORESCUE_TEST_VAULT_MONGO_HOST"

// vaultStack is a connections and targets service whose credentials come from
// the Vault dev server, with the engines wired as internal/app does.
type vaultStack struct {
	resolver *vaultcreds.Resolver
	store    *store.SQLiteStore
	conns    *connections.Service
	targets  *targets.Service
	backup   *backup.Engine
	restore  *restore.Engine
}

func newVaultStack(t *testing.T, v vaultEnv) *vaultStack {
	t.Helper()
	client, err := vault.New(vault.Config{Addr: v.addr, AllowInsecure: true, Auth: vault.Auth{Method: vault.AuthToken, Token: v.token}})
	if err != nil {
		t.Fatal(err)
	}
	resolver := vaultcreds.New(vaultcreds.Config{Client: client, Logger: discardLogger})
	t.Cleanup(resolver.Close)
	st := storetest.New(t)
	prober := mongoconn.New()
	s := &vaultStack{resolver: resolver, store: st}
	s.conns = connections.NewService(st, prober, connections.WithCredentials(resolver), connections.WithLogger(discardLogger))
	s.targets = targets.NewService(st, storage.TargetFactoryWith(nil, resolver.S3Provider), filepath.Join(t.TempDir(), "data"),
		targets.WithVault(true), targets.WithLogger(discardLogger))
	s.backup = backup.NewEngine(nil, "", backup.WithLogger(discardLogger), backup.WithCollectionLister(collectionLister()),
		backup.WithStorageResolver(s.targets.Storage), backup.WithCredentialHolder(s.conns))
	s.restore = restore.NewEngine(nil, "", restore.WithLogger(discardLogger), restore.WithStorageResolver(s.targets.Storage),
		restore.WithValidationBypassCheck(prober.CanBypassDocumentValidation), restore.WithDatabaseAdmin(prober),
		restore.WithCredentialHolder(s.conns))
	return s
}

// withoutUserinfo returns uri without its user name and password.
func withoutUserinfo(t *testing.T, uri string) (string, string, string) {
	t.Helper()
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	user := u.User.Username()
	pw, _ := u.User.Password()
	u.User = nil
	return u.String(), user, pw
}

// minioProvider returns the MinIO provider of the run or skips.
func minioProvider(t *testing.T) s3Provider {
	t.Helper()
	for _, p := range configuredS3Providers() {
		if p.Name == "minio" {
			return p
		}
	}
	t.Skip("MONGORESCUE_TEST_S3_MINIO_* not set")
	return s3Provider{}
}

// backupAndRestore backs db up from connection conn to target tgt and restores
// it into a safe clone, through the engines and the services.
func (s *vaultStack) backupAndRestore(t *testing.T, env *mongoEnv, conn, tgt, db string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	c, err := s.conns.Resolve(ctx, conn)
	if err != nil {
		t.Fatalf("resolve connection: %v", err)
	}
	opts := models.BackupOptions{Database: db, Gzip: true, ConnectionID: c.ID, MongoURI: c.URI, StorageTargetID: tgt}
	rec, err := s.backup.Run(ctx, opts)
	if err != nil || rec.Status != models.StatusCompleted {
		t.Fatalf("backup = %v (%s)", err, rec.ErrorMessage)
	}
	if c, err = s.conns.Resolve(ctx, conn); err != nil {
		t.Fatalf("resolve connection: %v", err)
	}
	req := models.RestoreRequest{BackupID: rec.ID, TargetConnectionID: c.ID, MongoURI: c.URI}
	rst, err := s.restore.Run(ctx, req, rec)
	if err != nil || rst.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore = %v (%s)", err, rst.ErrorMessage)
	}
	t.Cleanup(func() { env.dropDB(t, rst.TargetDatabase) })
	if got := env.count(t, rst.TargetDatabase, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}
	st, err := s.targets.Storage(ctx, tgt)
	if err == nil {
		_ = st.Delete(ctx, rec.StorageKey)
	}
}

// TestVaultKVCredentials backs up and restores with the MongoDB credentials and
// the MinIO access keys read from KV v2 secrets of the Vault dev server, and checks
// that none of them is stored.
func TestVaultKVCredentials(t *testing.T) {
	t.Parallel()
	env := requireMongo(t)
	v := requireVault(t)
	minio := minioProvider(t)
	if minio.CreateBucket {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		ensureBucket(ctx, t, minio.Config)
		cancel()
	}
	s := newVaultStack(t, v)
	ctx := context.Background()
	db := seedSource(t, env, "vaultkv")
	bare, user, pw := withoutUserinfo(t, env.URI)

	path := "mongorescue-it/" + randomHex(t, 4)
	v.api(t, http.MethodPost, "secret/data/"+path+"/mongo", map[string]any{"data": map[string]string{"username": user, "password": pw}}, nil)
	v.api(t, http.MethodPost, "secret/data/"+path+"/minio", map[string]any{"data": map[string]string{
		"access_key": minio.Config.AccessKey, "secret_key": minio.Config.SecretKey}}, nil)

	src := models.CredentialsVault
	ref := models.VaultRef{Engine: models.VaultKV2, Path: path + "/mongo"}
	conn, err := s.conns.Create(ctx, connections.Input{Name: "vault-kv", URI: bare,
		CredentialInput: connections.CredentialInput{CredentialSource: &src, Vault: &ref}})
	if err != nil {
		t.Fatal(err)
	}
	if res, testErr := s.conns.Test(ctx, conn.ID); testErr != nil || !res.OK {
		t.Fatalf("connection test = %+v, %v", res, testErr)
	}
	tgt, err := s.targets.Create(ctx, targets.Input{Name: "vault-minio", Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: minio.Config.Endpoint, Region: minio.Config.Region, Bucket: minio.Config.Bucket, Prefix: "vault-" + randomHex(t, 4),
		UsePathStyle: minio.Config.UsePathStyle, CredentialSource: models.CredentialsVault,
		Vault: models.VaultRef{Engine: models.VaultKV2, Path: path + "/minio"}}})
	if err != nil {
		t.Fatalf("create target (its test reads the keys from Vault): %v", err)
	}
	if res, testErr := s.targets.Test(ctx, tgt.ID); testErr != nil || !res.OK {
		t.Fatalf("target test = %+v, %v", res, testErr)
	}

	s.backupAndRestore(t, env, conn.ID, tgt.ID, db)

	// Nothing read from Vault is stored.
	storedConn, err := s.store.GetConnection(ctx, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	storedTgt, err := s.store.GetStorageTarget(ctx, tgt.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, pw, "stored connection", storedConn.URI, storedConn.LastTestError)
	if storedTgt.S3.AccessKeyID != "" || storedTgt.S3.SecretAccessKey != "" {
		t.Fatalf("stored target keys = %q/%q", storedTgt.S3.AccessKeyID, storedTgt.S3.SecretAccessKey)
	}

	// A secret deleted in Vault fails the next use (once the cache expired) with a
	// clear error, without a fallback.
	v.api(t, http.MethodDelete, "secret/metadata/"+path+"/mongo", nil, nil)
	client, err := vault.New(vault.Config{Addr: v.addr, AllowInsecure: true, Auth: vault.Auth{Method: vault.AuthToken, Token: v.token}})
	if err != nil {
		t.Fatal(err)
	}
	uncached := vaultcreds.New(vaultcreds.Config{Client: client, Logger: discardLogger})
	defer uncached.Close()
	if _, err = connections.NewService(s.store, mongoconn.New(), connections.WithCredentials(uncached)).Resolve(ctx, conn.ID); err == nil ||
		!strings.Contains(err.Error(), "vault refused") {
		t.Fatalf("resolve after the secret was deleted = %v", err)
	}
}

// TestVaultDatabaseSecretsEngine backs up and restores with a dynamic MongoDB user
// of the database secrets engine: the lease is held for each run and revoked
// after it, which drops the user.
func TestVaultDatabaseSecretsEngine(t *testing.T) {
	t.Parallel()
	env := requireMongo(t)
	v := requireVault(t)
	host := os.Getenv(envVaultMongoHost)
	if host == "" {
		t.Skip(envVaultMongoHost + " not set (the Vault container cannot reach MongoDB)")
	}
	minio := minioProvider(t)
	if minio.CreateBucket {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		ensureBucket(ctx, t, minio.Config)
		cancel()
	}
	_, rootUser, rootPW := withoutUserinfo(t, env.URI)
	mount := "db-" + randomHex(t, 4)
	v.api(t, http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "database"}, nil)
	t.Cleanup(func() { v.api(t, http.MethodDelete, "sys/mounts/"+mount, nil, nil) })
	v.api(t, http.MethodPost, mount+"/config/mongo", map[string]any{
		"plugin_name":    "mongodb-database-plugin",
		"allowed_roles":  "backup",
		"connection_url": "mongodb://{{username}}:{{password}}@" + host + "/admin?directConnection=true",
		"username":       rootUser, "password": rootPW,
	}, nil)
	v.api(t, http.MethodPost, mount+"/roles/backup", map[string]any{
		"db_name":             "mongo",
		"creation_statements": `{"db": "admin", "roles": [{"role": "root"}]}`,
		"default_ttl":         "2m", "max_ttl": "10m",
	}, nil)

	s := newVaultStack(t, v)
	ctx := context.Background()
	db := seedSource(t, env, "vaultdb")
	bare, _, _ := withoutUserinfo(t, env.URI)
	if !strings.Contains(bare, "authSource=") {
		bare += map[bool]string{true: "&", false: "?"}[strings.Contains(bare, "?")] + "authSource=admin"
	}
	src := models.CredentialsVault
	ref := models.VaultRef{Engine: models.VaultDatabase, Mount: mount, Role: "backup"}
	conn, err := s.conns.Create(ctx, connections.Input{Name: "vault-db", URI: bare,
		CredentialInput: connections.CredentialInput{CredentialSource: &src, Vault: &ref}})
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := s.targets.Create(ctx, targets.Input{Name: "minio-stored", Type: models.StorageS3, S3: &models.S3Target{
		Endpoint: minio.Config.Endpoint, Region: minio.Config.Region, Bucket: minio.Config.Bucket, Prefix: "vaultdb-" + randomHex(t, 4),
		UsePathStyle: minio.Config.UsePathStyle, AccessKeyID: minio.Config.AccessKey, SecretAccessKey: minio.Config.SecretKey}})
	if err != nil {
		t.Fatal(err)
	}

	// A run holds a lease: the user exists while it runs and is dropped when the
	// run releases it (the lease is revoked).
	held, uri, release, err := s.conns.Hold(ctx, conn.ID, bare)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(uri)
	dynUser := u.User.Username()
	if !strings.HasPrefix(dynUser, "v-") || !vaultUserExists(t, env, dynUser) {
		t.Fatalf("dynamic user %q does not exist", dynUser)
	}
	release()
	if held.Err() == nil {
		t.Fatal("the released lease is still held")
	}
	waitUserGone(t, env, dynUser)

	// A backup and a restore each run on a lease of their own.
	s.backupAndRestore(t, env, conn.ID, tgt.ID, db)

	// Closing the resolver revokes whatever is still open.
	s.resolver.Close()
	var out struct {
		Users []bson.M `bson:"users"`
	}
	if err = env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: 1}}).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, usr := range out.Users {
		if name, _ := usr["user"].(string); strings.HasPrefix(name, "v-") && strings.Contains(name, "backup") {
			waitUserGone(t, env, name)
		}
	}
}

// waitUserGone waits up to 30s until user no longer exists.
func waitUserGone(t *testing.T, env *mongoEnv, user string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for vaultUserExists(t, env, user) {
		if time.Now().After(deadline) {
			t.Fatalf("the user %s of a revoked lease still exists", user)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// vaultUserExists reports whether user exists in the admin database.
func vaultUserExists(t *testing.T, env *mongoEnv, user string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out struct {
		Users []bson.M `bson:"users"`
	}
	if err := env.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "usersInfo", Value: user}}).Decode(&out); err != nil {
		t.Fatalf("usersInfo: %v", err)
	}
	return len(out.Users) > 0
}
