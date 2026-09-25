//go:build integration

// Package integration contains end-to-end tests that exercise MongoRescue against real
// infrastructure: a MongoDB server, the official mongodump/mongorestore binaries, and
// any configured S3-compatible object stores. They are compiled only with the
// "integration" build tag and every dependency is optional: tests skip unless the
// corresponding environment variables are set.
//
// Environment:
//
//	MONGORESCUE_TEST_MONGO_URI                  MongoDB URI with credentials (required for Mongo tests)
//	MONGORESCUE_TEST_S3_<PROVIDER>_BUCKET       enables a provider; PROVIDER is one of
//	                                            MINIO, LOCALSTACK, AWS, R2, B2, SPACES, WASABI
//	MONGORESCUE_TEST_S3_<PROVIDER>_ENDPOINT     custom endpoint (empty for AWS)
//	MONGORESCUE_TEST_S3_<PROVIDER>_REGION       region (default us-east-1; "auto" for R2)
//	MONGORESCUE_TEST_S3_<PROVIDER>_ACCESS_KEY   access key id
//	MONGORESCUE_TEST_S3_<PROVIDER>_SECRET_KEY   secret access key
//	MONGORESCUE_TEST_S3_<PROVIDER>_PATH_STYLE   "true" for path-style addressing (MinIO, LocalStack)
//	MONGORESCUE_TEST_S3_<PROVIDER>_CREATE_BUCKET "true" to create the bucket if missing (emulators)
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

const (
	envMongoURI = "MONGORESCUE_TEST_MONGO_URI"
	envS3Prefix = "MONGORESCUE_TEST_S3_"

	// opTimeout bounds individual driver and storage operations in tests.
	opTimeout = 2 * time.Minute
)

// s3ProviderNames lists every S3-compatible provider the suite knows how to target.
var s3ProviderNames = []string{"MINIO", "LOCALSTACK", "AWS", "R2", "B2", "SPACES", "WASABI"}

// mongoEnv describes the MongoDB deployment under test.
type mongoEnv struct {
	URI      string
	Password string
	Client   *mongo.Client
}

// requireMongo skips the test unless MONGORESCUE_TEST_MONGO_URI is set, verifies the
// database tools are installed, and returns a connected client (disconnected on cleanup).
func requireMongo(t *testing.T) *mongoEnv {
	t.Helper()

	uri := os.Getenv(envMongoURI)
	if uri == "" {
		t.Skipf("%s not set; skipping MongoDB integration test", envMongoURI)
	}
	for _, bin := range []string{"mongodump", "mongorestore"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Fatalf("%s is set but %s is not on PATH: %v", envMongoURI, bin, err)
		}
	}

	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %s: %v", envMongoURI, err)
	}
	password, _ := parsed.User.Password()
	if password == "" {
		t.Logf("%s has no password; credential leak assertions are vacuous", envMongoURI)
	}

	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetTimeout(opTimeout))
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatalf("ping mongo: %v", err)
	}

	return &mongoEnv{URI: uri, Password: password, Client: client}
}

// randomHex returns n random bytes hex-encoded.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// uniqueDB returns a short, unique database name for the test and registers a cleanup
// that drops it together with every database derived from it (rescue clones, targets).
// Names stay well under MongoDB's 63-byte limit even after "_rescue_<timestamp>".
func (m *mongoEnv) uniqueDB(t *testing.T, tag string) string {
	t.Helper()
	if len(tag) > 8 {
		tag = tag[:8]
	}
	name := fmt.Sprintf("it_%s_%s", tag, randomHex(t, 4))

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		names, err := m.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^" + name}}}})
		if err != nil {
			t.Logf("cleanup: list databases: %v", err)
			return
		}
		for _, n := range names {
			if err := m.Client.Database(n).Drop(ctx); err != nil {
				t.Logf("cleanup: drop %s: %v", n, err)
			}
		}
	})
	return name
}

// seed inserts n documents into db.coll.
func (m *mongoEnv) seed(t *testing.T, db, coll string, n int) {
	t.Helper()
	docs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		docs = append(docs, bson.D{
			{Key: "seq", Value: i},
			{Key: "sku", Value: fmt.Sprintf("SKU-%05d", i)},
			{Key: "amount", Value: float64(i) * 1.25},
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := m.Client.Database(db).Collection(coll).InsertMany(ctx, docs); err != nil {
		t.Fatalf("seed %s.%s: %v", db, coll, err)
	}
}

// count returns the number of documents in db.coll.
func (m *mongoEnv) count(t *testing.T, db, coll string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	n, err := m.Client.Database(db).Collection(coll).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatalf("count %s.%s: %v", db, coll, err)
	}
	return n
}

// assertNoSecret fails the test if secret appears in any of the given values.
func assertNoSecret(t *testing.T, secret, what string, values ...string) {
	t.Helper()
	if secret == "" {
		return
	}
	for _, v := range values {
		if strings.Contains(v, secret) {
			t.Fatalf("%s leaked the MongoDB password", what)
		}
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer used to capture structured logs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger returns a debug-level logger writing into a buffer for leak assertions.
func captureLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// s3Provider is a configured S3-compatible provider under test.
type s3Provider struct {
	Name         string
	Config       storage.S3Config
	CreateBucket bool
}

// configuredS3Providers returns every provider whose bucket and credentials are set.
func configuredS3Providers() []s3Provider {
	var out []s3Provider
	for _, name := range s3ProviderNames {
		get := func(field string) string { return strings.TrimSpace(os.Getenv(envS3Prefix + name + "_" + field)) }
		bucket, access, secret := get("BUCKET"), get("ACCESS_KEY"), get("SECRET_KEY")
		if bucket == "" || access == "" || secret == "" {
			continue
		}
		region := get("REGION")
		if region == "" {
			region = "us-east-1"
		}
		out = append(out, s3Provider{
			Name: strings.ToLower(name),
			Config: storage.S3Config{
				Endpoint:     get("ENDPOINT"),
				Bucket:       bucket,
				Region:       region,
				AccessKey:    access,
				SecretKey:    secret,
				UsePathStyle: strings.EqualFold(get("PATH_STYLE"), "true"),
			},
			CreateBucket: strings.EqualFold(get("CREATE_BUCKET"), "true"),
		})
	}
	return out
}

// newS3Storage builds the production S3 driver for p, creating the bucket first when
// the provider is an emulator configured with CREATE_BUCKET=true.
func newS3Storage(t *testing.T, p s3Provider) storage.Storage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	if p.CreateBucket {
		ensureBucket(ctx, t, p.Config)
	}

	drv, err := storage.NewS3Storage(ctx, p.Config)
	if err != nil {
		t.Fatalf("%s: new s3 storage: %v", p.Name, err)
	}
	return drv
}

// ensureBucket creates cfg.Bucket if it does not exist yet (test-only helper).
func ensureBucket(ctx context.Context, t *testing.T, cfg storage.S3Config) {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})

	if _, err = client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err == nil {
		return
	}

	input := &s3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)}
	if cfg.Endpoint == "" && cfg.Region != "us-east-1" {
		input.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(cfg.Region),
		}
	}
	_, err = client.CreateBucket(ctx, input)
	var owned *s3types.BucketAlreadyOwnedByYou
	var exists *s3types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		t.Fatalf("create bucket %s: %v", cfg.Bucket, err)
	}
}

// storageTarget is a named storage backend the suites run against.
type storageTarget struct {
	Name    string
	Storage storage.Storage
}

// storageTargets returns local disk storage plus every configured S3 provider.
func storageTargets(t *testing.T) []storageTarget {
	t.Helper()
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	targets := []storageTarget{{Name: "local", Storage: local}}
	for _, p := range configuredS3Providers() {
		targets = append(targets, storageTarget{Name: p.Name, Storage: newS3Storage(t, p)})
	}
	t.Logf("storage targets: %s", targetNames(targets))
	return targets
}

func targetNames(targets []storageTarget) string {
	names := make([]string, 0, len(targets))
	for _, tg := range targets {
		names = append(names, tg.Name)
	}
	return strings.Join(names, ", ")
}

// cleanupPrefix deletes every object under prefix when the test finishes.
func cleanupPrefix(t *testing.T, st storage.Storage, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		objs, err := st.List(ctx, prefix)
		if err != nil {
			t.Logf("cleanup: list %s: %v", prefix, err)
			return
		}
		for _, o := range objs {
			if err := st.Delete(ctx, o.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				t.Logf("cleanup: delete %s: %v", o.Key, err)
			}
		}
	})
}
