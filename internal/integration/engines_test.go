//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// discardLogger keeps engine logs out of the test output.
var discardLogger = slog.New(slog.DiscardHandler)

// collectionLister wires the driver adapter into the backup engine as internal/app does.
func collectionLister() backup.CollectionLister {
	prober := mongoconn.New()
	return func(ctx context.Context, uri, database string) ([]string, error) {
		cols, err := prober.ListCollections(ctx, uri, database)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(cols))
		for _, c := range cols {
			names = append(names, c.Name)
		}
		return names, nil
	}
}

// newBackupEngine returns the production backup engine for st, with a collection lister.
func newBackupEngine(env *mongoEnv, st storage.Storage, opts ...backup.Option) *backup.Engine {
	base := []backup.Option{backup.WithLogger(discardLogger), backup.WithCollectionLister(collectionLister())}
	return backup.NewEngine(st, env.URI, append(base, opts...)...)
}

// newRestoreEngine returns the production restore engine for st.
func newRestoreEngine(env *mongoEnv, st storage.Storage, opts ...restore.Option) *restore.Engine {
	base := []restore.Option{restore.WithLogger(discardLogger), restore.WithValidationBypassCheck(mongoconn.New().CanBypassDocumentValidation)}
	return restore.NewEngine(st, env.URI, append(base, opts...)...)
}

// mustBackup runs a backup that must succeed and deletes its artifact on cleanup.
func mustBackup(t *testing.T, env *mongoEnv, st storage.Storage, opts models.BackupOptions, engineOpts ...backup.Option) *models.BackupRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	opts.MongoURI = env.URI
	rec, err := newBackupEngine(env, st, engineOpts...).Run(ctx, opts)
	if err != nil {
		t.Fatalf("backup of %s: %v", opts.Database, err)
	}
	if rec.Status != models.StatusCompleted || rec.SHA256 == "" || rec.SizeBytes <= 0 {
		t.Fatalf("backup record: status=%s sha=%q size=%d (%s)", rec.Status, rec.SHA256, rec.SizeBytes, rec.ErrorMessage)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = st.Delete(ctx, rec.StorageKey)
	})
	return rec
}

// tryRestore runs a restore and returns its record and error; the target database is
// dropped on cleanup by the uniqueDB prefix of the source.
func tryRestore(t *testing.T, env *mongoEnv, st storage.Storage, req models.RestoreRequest, src *models.BackupRecord, engineOpts ...restore.Option) (*models.RestoreRecord, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if req.BackupID == "" {
		req.BackupID = src.ID
	}
	req.MongoURI = env.URI
	rec, err := newRestoreEngine(env, st, engineOpts...).Run(ctx, req, src)
	if err != nil {
		assertNoSecret(t, env.Password, "restore error", err.Error())
	}
	if rec != nil && !req.InPlace() {
		// Clones are named by the second: drop each one when its (sub)test ends so a
		// later restore within the same second starts from an empty namespace.
		t.Cleanup(func() {
			if env.dbExists(t, rec.TargetDatabase) {
				env.dropDB(t, rec.TargetDatabase)
			}
		})
	}
	return rec, err
}

// mustRestore runs a restore that must succeed.
func mustRestore(t *testing.T, env *mongoEnv, st storage.Storage, req models.RestoreRequest, src *models.BackupRecord, engineOpts ...restore.Option) *models.RestoreRecord {
	t.Helper()
	rec, err := tryRestore(t, env, st, req, src, engineOpts...)
	if err != nil {
		t.Fatalf("restore of %s: %v", src.ID, err)
	}
	if rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore status = %s (%s)", rec.Status, rec.ErrorMessage)
	}
	return rec
}

// keyPair returns a matching age encryptor and decryptor.
func keyPair(t *testing.T) (*encryption.Encryptor, *encryption.Decryptor) {
	t.Helper()
	identity, recipient, err := encryption.GenerateX25519()
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := encryption.NewDecryptor(encryption.DecryptorConfig{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	return enc, dec
}

// dropDB drops db now (tests drop restored clones early to keep disk use low).
func (m *mongoEnv) dropDB(t *testing.T, db string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := m.Client.Database(db).Drop(ctx); err != nil {
		t.Fatalf("drop %s: %v", db, err)
	}
}

// dbExists reports whether db exists on the server.
func (m *mongoEnv) dbExists(t *testing.T, db string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := m.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: db}})
	if err != nil {
		t.Fatalf("list databases: %v", err)
	}
	return len(names) > 0
}

// serverVersion returns the major and minor version of the server.
func (m *mongoEnv) serverVersion(t *testing.T) (major, minor int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var info struct {
		VersionArray []int `bson:"versionArray"`
	}
	if err := m.Client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&info); err != nil {
		t.Fatalf("buildInfo: %v", err)
	}
	if len(info.VersionArray) < 2 {
		t.Fatalf("buildInfo: unexpected versionArray %v", info.VersionArray)
	}
	return info.VersionArray[0], info.VersionArray[1]
}

// copyObject streams the object at from to the key to through transform, which may
// alter each chunk in place (p starts at offset off) or cut the stream short by
// returning fewer bytes. It returns the size written.
func copyObject(t *testing.T, st storage.Storage, from, to string, transform func(off int64, p []byte) int) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	rc, err := st.Retrieve(ctx, from)
	if err != nil {
		t.Fatalf("retrieve %s: %v", from, err)
	}
	defer rc.Close()
	tr := &transformReader{r: rc, fn: transform}
	if _, err := st.Save(ctx, to, tr); err != nil {
		t.Fatalf("save %s: %v", to, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = st.Delete(ctx, to)
	})
	return tr.off
}

type transformReader struct {
	r    io.Reader
	fn   func(off int64, p []byte) int
	off  int64
	done bool
}

func (tr *transformReader) Read(p []byte) (int, error) {
	if tr.done {
		return 0, io.EOF
	}
	n, err := tr.r.Read(p)
	if n > 0 {
		keep := tr.fn(tr.off, p[:n])
		if keep < n {
			tr.done = true
			n = keep
			err = nil
		}
		tr.off += int64(n)
	}
	if tr.done && n == 0 {
		return 0, io.EOF
	}
	return n, err
}

// objectSHA256 returns the SHA-256 of the stored object at key.
func objectSHA256(t *testing.T, st storage.Storage, key string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	rc, err := st.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("retrieve %s: %v", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// assertObjectGone fails if key exists on st.
func assertObjectGone(t *testing.T, st storage.Storage, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if _, err := st.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object %s must not exist after a failed backup (stat error: %v)", key, err)
	}
}

// rawS3Client returns an SDK client for cfg, for assertions the Storage port does
// not expose (incomplete multipart uploads).
func rawS3Client(ctx context.Context, t *testing.T, cfg storage.S3Config) *s3.Client {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
}

// incompleteUploads returns the number of multipart uploads in progress for key.
func incompleteUploads(t *testing.T, cfg storage.S3Config, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	out, err := rawS3Client(ctx, t, cfg).ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String(cfg.Bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		t.Fatalf("list multipart uploads: %v", err)
	}
	n := 0
	for _, u := range out.Uploads {
		if aws.ToString(u.Key) == key {
			n++
		}
	}
	return n
}
