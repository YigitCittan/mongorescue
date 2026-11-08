//go:build integration && !windows

package integration

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/ha"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// haSwapTimeout is the swap handover timeout of the standby in the test: short, so
// the test does not wait the six hours of a real swap.
const haSwapTimeout = 60 * time.Second

// openFence is the fence of a test that only reads the swap ledger.
type openFence struct{}

func (openFence) Check() error  { return nil }
func (openFence) Epoch() uint64 { return 0 }

// haStreamState is the part of GET /api/v1/pitr/streams/{id} the test reads.
type haStreamState struct {
	Stream struct {
		Enabled bool `json:"enabled"`
	} `json:"stream"`
	Chains []struct {
		ChainID string     `json:"chain_id"`
		EndedAt *time.Time `json:"ended_at"`
	} `json:"chains"`
	Windows []struct {
		Open    bool      `json:"open"`
		EndTime time.Time `json:"end_time"`
	} `json:"windows"`
}

// openChain returns the ID of the open chain ("" without one).
func (s haStreamState) openChain() string {
	for _, c := range s.Chains {
		if c.EndedAt == nil {
			return c.ChainID
		}
	}
	return ""
}

// TestHAInPlacePITRSurvivesATakeover is the acceptance test of issue #171: the
// active instance is killed while it swaps the original databases of an in-place
// point-in-time restore. The standby takes over, finds the swap's marker on the HA
// target and leaves the swap alone (the restore stays in progress, the stream stays
// paused, the clones stay) until the handover timeout, when it marks the restore
// interrupted, removes the marker, and enables the stream again with a new chain.
func TestHAInPlacePITRSurvivesATakeover(t *testing.T) {
	env := requireWholeInstance(t)
	requireReplicaSet(t, env)
	var provider *s3Provider
	for _, p := range configuredS3Providers() {
		if p.Name == "minio" {
			provider = &p
		}
	}
	if provider == nil {
		t.Skip("MinIO is not configured (MONGORESCUE_TEST_S3_MINIO_*)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if provider.CreateBucket {
		ensureBucket(ctx, t, provider.Config)
	}
	db := env.uniqueDB(t, "hapitr")
	// Enough data that the swap (mongodump piped into mongorestore) outlasts the
	// polling that catches its marker.
	env.seedBulk(t, db, 80)
	env.seed(t, db, "orders", 40)
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), opTimeout)
		defer cleanCancel()
		names, err := env.Client.ListDatabaseNames(cleanCtx, bson.D{})
		if err != nil {
			return
		}
		for _, name := range names {
			if strings.HasPrefix(name, db+"_rescue_") {
				_ = env.Client.Database(name).Drop(cleanCtx)
			}
		}
	})

	run := randomHex(t, 6)
	haPrefix, cluster := "it-hapitr-"+run, "it-"+run
	haCfg := provider.Config
	haCfg.Prefix = haPrefix
	haStore, err := storage.NewS3Storage(ctx, haCfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPrefix(t, haStore, "")
	backupStore := provider.Config
	backupStore.Prefix = "it-hapitr-backups-" + run
	backupDriver, err := storage.NewS3Storage(ctx, backupStore)
	if err != nil {
		t.Fatal(err)
	}
	cleanupPrefix(t, backupDriver, "")

	keyBytes := make([]byte, 32)
	if _, err = rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	memberEnv := cleanEnv(
		"MONGORESCUE_SECRET_KEY="+base64.StdEncoding.EncodeToString(keyBytes),
		"MONGORESCUE_HA_MODE=active-passive",
		fmt.Sprintf("MONGORESCUE_HA_TARGET=s3://%s/%s?endpoint=%s&region=%s&path_style=%t",
			haCfg.Bucket, haPrefix, haCfg.Endpoint, haCfg.Region, haCfg.UsePathStyle),
		"MONGORESCUE_HA_CLUSTER="+cluster,
		"MONGORESCUE_HA_LEASE_TTL="+haTestTTL.String(),
		"MONGORESCUE_HA_RENEW_INTERVAL="+haTestRenew.String(),
		"MONGORESCUE_HA_SNAPSHOT_INTERVAL=30s",
		"MONGORESCUE_HA_SWAP_TIMEOUT="+haSwapTimeout.String(),
		"AWS_ACCESS_KEY_ID="+haCfg.AccessKey, "AWS_SECRET_ACCESS_KEY="+haCfg.SecretKey, "AWS_REGION="+haCfg.Region,
		"AWS_CONFIG_FILE="+missing, "AWS_SHARED_CREDENTIALS_FILE="+missing, "AWS_PROFILE=",
	)
	bin := buildBinary(t)
	newMember := func(name string) *haMember {
		return &haMember{t: t, name: name, bin: bin, dataDir: filepath.Join(t.TempDir(), "data"), port: freePort(t), env: memberEnv, logs: &syncBuffer{}}
	}
	a, b := newMember("A"), newMember("B")
	t.Cleanup(func() {
		b.stop()
		a.stop()
		if t.Failed() {
			t.Logf("A logs:\n%s\nB logs:\n%s", a.logs.String(), b.logs.String())
		}
	})
	a.start()
	a.waitRole("active", 60*time.Second)
	b.start()
	b.waitRole("standby", 60*time.Second)

	// Set up A: the first user, the connection, the storage target and the PITR
	// stream with a base backup.
	setupCode := waitSetupCode(t, a.logs, 30*time.Second)
	const password = "integration-password-1"
	apiA := newAPIClient(t, a.base())
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	apiA.data("POST", "/api/v1/setup", map[string]string{"setup_code": setupCode, "username": "admin", "password": password}, http.StatusCreated, &session)
	apiA.csrf = session.CSRFToken
	var conn models.Connection
	apiA.data("POST", "/api/v1/connections", map[string]string{"name": "ha pitr mongo", "uri": env.URI}, http.StatusCreated, &conn)
	var target models.StorageTarget
	apiA.data("POST", "/api/v1/storage-targets", map[string]any{"name": "ha pitr minio", "type": "s3", "s3": map[string]any{
		"endpoint": backupStore.Endpoint, "region": backupStore.Region, "bucket": backupStore.Bucket, "prefix": backupStore.Prefix,
		"access_key_id": backupStore.AccessKey, "secret_access_key": backupStore.SecretKey, "use_path_style": backupStore.UsePathStyle,
	}}, http.StatusCreated, &target)
	// PITR needs backup encryption with age keys (the oplog keeps deleted data).
	var key struct {
		Identity  string `json:"identity"`
		Recipient string `json:"recipient"`
	}
	apiA.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &key)
	apiA.data("PUT", "/api/v1/settings", map[string]any{"encryption": map[string]any{
		"enabled": true, "mode": "x25519", "recipients": []string{key.Recipient}, "identity": key.Identity,
	}}, http.StatusOK, nil)
	var stream struct {
		ID string `json:"id"`
	}
	apiA.data("POST", "/api/v1/pitr/streams", map[string]any{"connection_id": conn.ID, "target_id": target.ID, "chunk_seconds": 15, "base_cron": "@daily"},
		http.StatusCreated, &stream)
	streamState := func(api *apiClient) haStreamState {
		var st haStreamState
		api.data("GET", "/api/v1/pitr/streams/"+stream.ID, nil, http.StatusOK, &st)
		return st
	}
	waitState := func(api *apiClient, what string, within time.Duration, ok func(haStreamState) bool) haStreamState {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			st := streamState(api)
			if ok(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, st)
			}
			time.Sleep(time.Second)
		}
	}
	waitState(apiA, "the stream's first chain", 2*time.Minute, func(st haStreamState) bool { return st.openChain() != "" })
	apiA.data("POST", "/api/v1/pitr/streams/"+stream.ID+"/base", nil, http.StatusAccepted, nil)
	waitState(apiA, "a window with a base", 5*time.Minute, func(st haStreamState) bool { return len(st.Windows) > 0 })

	// The point to restore to, then the disaster after it.
	at := time.Now().UTC().Truncate(time.Second)
	time.Sleep(2 * time.Second)
	if err = env.Client.Database(db).Collection("orders").Drop(ctx); err != nil {
		t.Fatal(err)
	}
	disaster := time.Now().UTC()
	oldChain := waitState(apiA, "oplog chunks covering the disaster", 3*time.Minute, func(st haStreamState) bool {
		return len(st.Windows) > 0 && st.Windows[0].EndTime.After(disaster.Add(time.Second))
	}).openChain()

	var rec models.RestoreRecord
	apiA.data("POST", "/api/v1/restore", models.RestoreRequest{
		PITR:      &models.PITRTarget{StreamID: stream.ID, At: &at, InPlace: true, ConfirmInPlace: db},
		Databases: []string{db},
	}, http.StatusAccepted, &rec)

	// Catch the swap: its marker on the HA target, still swapping. Kill A then.
	board, err := ha.NewSwapBoard(cluster, haStore, keyBytes, openFence{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var marker *models.SwapMarker
	deadline := time.Now().Add(10 * time.Minute)
	for marker == nil && time.Now().Before(deadline) {
		open, openErr := board.Open(ctx)
		if openErr != nil {
			t.Fatal(openErr)
		}
		for _, m := range open {
			if m.RestoreID == rec.ID && !m.Finished() {
				marker = &m
			}
		}
		if marker == nil {
			if cur := restoreOf(apiA, rec.ID); cur != nil && (cur.Status == models.RestoreStatusFailed || cur.Status == models.RestoreStatusCompleted) {
				t.Fatalf("the restore ended (%s: %s) before its swap could be interrupted", cur.Status, cur.ErrorMessage)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if marker == nil {
		t.Fatalf("no swap marker appeared on the HA target\n%s", a.logs.String())
	}
	if marker.Epoch == 0 || marker.StreamID != stream.ID || !marker.StreamWasEnabled || len(marker.Clones) != 1 {
		t.Fatalf("marker = %+v", marker)
	}
	t.Logf("killing A: its swap of epoch %d is under way", marker.Epoch)
	a.kill()

	// B takes over, and leaves the swap alone.
	b.waitRole("active", haTestTTL+haTestRenew+30*time.Second)
	becameActive := time.Now()
	apiB := newAPIClient(t, b.base())
	apiB.data("POST", "/api/v1/auth/login", map[string]string{"username": "admin", "password": password}, http.StatusOK, &session)
	apiB.csrf = session.CSRFToken
	deadlineHold := becameActive.Add(haSwapTimeout / 2)
	for time.Now().Before(deadlineHold) {
		if open, openErr := board.Open(ctx); openErr != nil || len(open) != 1 || open[0].RestoreID != rec.ID {
			t.Fatalf("the marker was touched during the hold: %v, %v", open, openErr)
		}
		st := streamState(apiB)
		if st.Stream.Enabled || st.openChain() != "" {
			t.Fatalf("the stream was enabled or opened a chain while the swap was held: %+v", st)
		}
		// The record may be missing: B restored a metadata snapshot that can predate
		// the restore.
		if cur := restoreOf(apiB, rec.ID); cur != nil && cur.Status != models.RestoreStatusInProgress {
			t.Fatalf("B settled the restore while the swap was held: %s: %s", cur.Status, cur.ErrorMessage)
		}
		names, listErr := env.Client.ListDatabaseNames(ctx, bson.D{})
		if listErr != nil || !containsPrefix(names, db+"_rescue_") {
			t.Fatalf("the clone was dropped while the swap was held: %v, %v", names, listErr)
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(b.logs.String(), "leaves it, its databases and its stream alone") {
		t.Fatalf("B did not log that it leaves the swap alone\n%s", b.logs.String())
	}

	// The process of the swap reported nothing: after the timeout B marks the
	// restore interrupted, removes the marker and enables the stream again, in a
	// new chain.
	waitState(apiB, "the stream to be enabled again with a new chain", haSwapTimeout+3*time.Minute, func(st haStreamState) bool {
		return st.Stream.Enabled && st.openChain() != "" && st.openChain() != oldChain
	})
	resolveBy := time.Now().Add(time.Minute)
	for time.Now().Before(resolveBy) {
		open, openErr := board.Open(ctx)
		if openErr != nil {
			t.Fatal(openErr)
		}
		if len(open) == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if open, _ := board.Open(ctx); len(open) != 0 {
		t.Fatalf("the settled swap's marker is still on the HA target: %+v", open)
	}
	if cur := restoreOf(apiB, rec.ID); cur != nil && (cur.Status != models.RestoreStatusFailed || !strings.Contains(cur.ErrorMessage, "interrupted")) {
		t.Fatalf("the restore after the timeout: %s: %s", cur.Status, cur.ErrorMessage)
	}
	if !containsPrefix(mustDatabaseNames(t, env), db+"_rescue_") {
		t.Fatal("the clone was dropped by the interrupted swap's settlement; it may hold the only copy of the restored state")
	}
}

// restoreOf returns restore id as api lists it, or nil when it has no such record.
func restoreOf(api *apiClient, id string) *models.RestoreRecord {
	code, raw := api.do("GET", "/api/v1/restores?id="+id, nil)
	if code != http.StatusOK {
		return nil
	}
	var body struct {
		Data []models.RestoreRecord `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Data) == 0 {
		return nil
	}
	return &body.Data[0]
}

// containsPrefix reports whether one of names starts with prefix.
func containsPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// mustDatabaseNames lists the databases of the test server.
func mustDatabaseNames(t *testing.T, env *mongoEnv) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	names, err := env.Client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	return names
}
