//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/mongotls"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// Environment of the TLS run (scripts/test-integration-docker.sh starts a MongoDB
// with --tlsMode requireTLS, certificates from internal/integration/gencerts).
const (
	envTLSURI     = "MONGORESCUE_TEST_TLS_URI"      // root user, password over TLS
	envTLSX509URI = "MONGORESCUE_TEST_TLS_X509_URI" // authMechanism=MONGODB-X509
	envTLSDir     = "MONGORESCUE_TEST_TLS_DIR"      // ca.pem, client.crt, client.key, client.key.password
)

// tlsEnv is the TLS deployment under test.
type tlsEnv struct {
	*mongoEnv
	X509URI string
	CA      string
	Client  models.ConnectionTLS // CA, certificate, encrypted key and password
}

// requireTLS skips the test unless the TLS MongoDB is configured, and returns an
// admin client (password authentication over TLS with the test CA).
func requireTLS(t *testing.T) *tlsEnv {
	t.Helper()
	uri, x509URI, dir := os.Getenv(envTLSURI), os.Getenv(envTLSX509URI), os.Getenv(envTLSDir)
	if uri == "" || x509URI == "" || dir == "" {
		t.Skipf("%s, %s and %s not set; skipping TLS integration test", envTLSURI, envTLSX509URI, envTLSDir)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	env := &tlsEnv{X509URI: x509URI, CA: read("ca.pem")}
	env.Client = models.ConnectionTLS{CAPEM: env.CA, ClientCertPEM: read("client.crt"),
		ClientKeyPEM: read("client.key"), ClientKeyPassword: strings.TrimSpace(read("client.key.password"))}
	if err := mongotls.Validate(&env.Client); err != nil {
		t.Fatalf("test material: %v", err)
	}

	cfg, err := mongotls.Config(&models.ConnectionTLS{CAPEM: env.CA})
	if err != nil {
		t.Fatal(err)
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetTLSConfig(cfg).SetTimeout(opTimeout))
	if err != nil {
		t.Fatalf("connect TLS mongo: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping TLS mongo: %v", err)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	env.mongoEnv = &mongoEnv{URI: uri, Password: password, Client: client}
	return env
}

// tlsRoundTrip backs up db over uri with material and restores it into a safe
// clone, both with the real tools, and returns the clone's name.
func tlsRoundTrip(t *testing.T, env *tlsEnv, uri string, material *models.ConnectionTLS, db string) string {
	t.Helper()
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	logger, logs := captureLogger()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	bkp, err := backup.NewEngine(st, "", backup.WithLogger(logger), backup.WithConfigDir(configDir)).
		Run(ctx, models.BackupOptions{Database: db, Gzip: true, MongoURI: uri, MongoTLS: material})
	if err != nil || bkp.Status != models.StatusCompleted {
		t.Fatalf("backup over TLS = %+v, %v", bkp, err)
	}
	rst, err := restore.NewEngine(st, "", restore.WithLogger(logger), restore.WithConfigDir(configDir)).
		Run(ctx, models.RestoreRequest{BackupID: bkp.ID, MongoURI: uri, MongoTLS: material}, bkp)
	if err != nil || rst.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore over TLS = %+v, %v", rst, err)
	}
	if !strings.HasPrefix(rst.TargetDatabase, db+"_rescue_") {
		t.Fatalf("restore target = %q; want a safe clone", rst.TargetDatabase)
	}

	// Nothing the tools were given is left behind, and no secret was logged.
	if entries, _ := os.ReadDir(configDir); len(entries) != 0 {
		t.Fatalf("tools files left behind: %v", entries)
	}
	raw, _ := json.Marshal([]any{bkp, rst})
	secrets := []string{"PRIVATE KEY", material.ClientKeyPassword}
	if env.Password != "" {
		secrets = append(secrets, env.Password)
	}
	for _, s := range secrets {
		if s != "" {
			assertNoSecret(t, s, "records/logs", string(raw), logs.String())
		}
	}
	return rst.TargetDatabase
}

// TestTLSWithCustomCA backs up and restores over requireTLS with a CA the system
// does not trust, given as tls_ca_pem; without it the same backup fails.
func TestTLSWithCustomCA(t *testing.T) {
	env := requireTLS(t)
	db := env.uniqueDB(t, "tlsca")
	env.seed(t, db, "orders", ordersCount)

	material := &models.ConnectionTLS{CAPEM: env.CA}
	clone := tlsRoundTrip(t, env, env.URI, material, db)
	if got := env.count(t, clone, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}

	// The connection test of the driver adapter, with and without the CA.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := mongoconn.New().Ping(mongotls.NewContext(ctx, material), env.URI); err != nil {
		t.Fatalf("ping with the CA: %v", err)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShort()
	if _, err := mongoconn.New().Ping(short, env.URI); err == nil {
		t.Fatal("ping without the CA succeeded; the server certificate must not be trusted")
	}

	// Without the CA, mongodump cannot verify the server either. It keeps retrying
	// the handshake rather than failing fast, so the attempt is bounded.
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()
	rec, err := backup.NewEngine(st, "").Run(ctx2, models.BackupOptions{Database: db, MongoURI: env.URI + "&serverSelectionTimeoutMS=3000"})
	if err == nil && rec.Status == models.StatusCompleted {
		t.Fatal("backup without the CA succeeded")
	}
}

// TestX509ClientCertificate backs up and restores as an x509 user ($external, the
// client certificate subject) with MONGODB-X509, the key encrypted (PKCS#8) and its
// password passed through the tools' --config file. It also runs the connection
// service end to end: save (key sealed and masked), test.
func TestX509ClientCertificate(t *testing.T) {
	env := requireTLS(t)
	db := env.uniqueDB(t, "x509")
	env.seed(t, db, "orders", ordersCount)

	material := env.Client
	clone := tlsRoundTrip(t, env, env.X509URI, &material, db)
	if got := env.count(t, clone, "orders"); got != ordersCount {
		t.Fatalf("restored orders = %d; want %d", got, ordersCount)
	}

	svc := connections.NewService(storetest.New(t), mongoconn.New())
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	in := connections.Input{Name: "x509", URI: env.X509URI, TLSInput: connections.TLSInput{
		CAPEM: &material.CAPEM, ClientCertPEM: &material.ClientCertPEM,
		ClientKeyPEM: &material.ClientKeyPEM, ClientKeyPassword: &material.ClientKeyPassword}}
	c, err := svc.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	assertNoSecret(t, "PRIVATE KEY", "API form", string(raw))
	assertNoSecret(t, material.ClientKeyPassword, "API form", string(raw))
	res, err := svc.Test(ctx, c.ID)
	if err != nil || !res.OK {
		t.Fatalf("connection test = %+v, %v", res, err)
	}
	dbs, err := svc.Databases(ctx, c.ID, false)
	if err != nil {
		t.Fatalf("list databases as the x509 user: %v", err)
	}
	found := false
	for _, d := range dbs {
		found = found || d.Name == db
	}
	if !found {
		t.Fatalf("databases %v lack %s", dbs, db)
	}

	// Without the client certificate the x509 URI cannot authenticate.
	short, cancelShort := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShort()
	if _, err = mongoconn.New().Ping(mongotls.NewContext(short, &models.ConnectionTLS{CAPEM: env.CA}), env.X509URI); err == nil {
		t.Fatal("x509 ping without a client certificate succeeded")
	}
}
