package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/encryption"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// pitrRunner fakes mongorestore for both passes: it reads all of stdin and answers
// with the output of the pass (an --oplogReplay run prints applied).
type pitrRunner struct {
	mu      sync.Mutex
	calls   [][]string
	stdins  [][]byte
	applied string // the pass-2 output line; empty prints none
	// onRun, when set, is called with each run's arguments (pass 1 creates clones).
	onRun func(args []string)
}

func (r *pitrRunner) run(_ context.Context, _ string, stdin io.Reader, args ...string) (io.Reader, func() error, error) {
	in, err := io.ReadAll(stdin)
	r.mu.Lock()
	r.calls = append(r.calls, args)
	r.stdins = append(r.stdins, in)
	r.mu.Unlock()
	if r.onRun != nil {
		r.onRun(args)
	}
	out := "3 document(s) restored successfully. 0 document(s) failed to restore."
	if slices.Contains(args, "--oplogReplay") {
		out = r.applied
	}
	return strings.NewReader(out), func() error { return err }, nil
}

// pitrAdmin records drops and serves database names.
type pitrAdmin struct {
	mu      sync.Mutex
	names   []string
	dropped []string
}

func (a *pitrAdmin) DatabaseExists(_ context.Context, _, db string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.names, db), nil
}

func (a *pitrAdmin) DropDatabase(_ context.Context, _, db string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropped = append(a.dropped, db)
	return nil
}

func (a *pitrAdmin) list(context.Context, string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.names), nil
}

// oplogInsert returns an insert entry at second sec into ns with a marker field.
func oplogInsert(t *testing.T, sec uint32, ns string, id int) []byte {
	t.Helper()
	b, err := bson.Marshal(bson.D{
		{Key: "op", Value: "i"}, {Key: "ns", Value: ns},
		{Key: "o", Value: bson.D{{Key: "_id", Value: int32(id)}, {Key: "marker", Value: fmt.Sprintf("marker-%d", id)}}},
		{Key: "ts", Value: bson.Timestamp{T: sec, I: 1}}, {Key: "t", Value: int64(1)}, {Key: "v", Value: int64(2)},
		{Key: "wall", Value: time.Unix(int64(sec), 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pitrFixture is a base and two chunks in mock storage, encrypted with one key.
type pitrFixture struct {
	store  *storage.MockStorage
	dec    *encryption.Decryptor
	base   *models.BackupRecord
	chunks []*pitr.Chunk
}

func newPITRFixture(t *testing.T) *pitrFixture {
	t.Helper()
	ctx := context.Background()
	identity, recipient := newKeyPair(t)
	enc, err := encryption.NewX25519Encryptor([]string{recipient})
	if err != nil {
		t.Fatal(err)
	}
	f := &pitrFixture{store: storage.NewMockStorage(), dec: mustDecryptor(t, encryption.DecryptorConfig{Identity: identity})}
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, _ = w.Write(b)
		_ = w.Close()
		return buf.Bytes()
	}
	save := func(key string, plain []byte) string {
		sealed := sealPayload(t, enc, gz(plain))
		if _, err := f.store.Save(ctx, key, bytes.NewReader(sealed)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(sealed)
		return hex.EncodeToString(sum[:])
	}
	baseKey := "_mongorescue/base/c1/rs0/2026/10/b1.archive.gz.age"
	f.base = &models.BackupRecord{ID: "b1", Scope: models.ScopeInstance, StorageKey: baseKey, Encrypted: true,
		EncryptionMode: "x25519", ServerVersion: "8.0.4", Status: models.StatusCompleted}
	f.base.SHA256 = save(baseKey, []byte("base archive bytes"))

	chunk := func(from, to uint32, entries ...[]byte) *pitr.Chunk {
		key := fmt.Sprintf("_mongorescue/oplog/c1/rs0/ch1/%d-%d.bson.gz.age", from, to)
		c := &pitr.Chunk{ID: fmt.Sprintf("k%d", to), ChainID: "ch1", StorageKey: key,
			From: pitr.Timestamp{T: from, I: 1}, To: pitr.Timestamp{T: to, I: 1}, Encrypted: true, EncryptionMode: "x25519"}
		c.SHA256 = save(key, bytes.Join(entries, nil))
		return c
	}
	f.chunks = []*pitr.Chunk{
		chunk(100, 110, oplogInsert(t, 101, "shop.orders", 1), oplogInsert(t, 105, "other.items", 2)),
		chunk(110, 120, oplogInsert(t, 112, "shop.orders", 3), oplogInsert(t, 118, "shop.orders", 4)),
	}
	return f
}

// run returns the planned run restoring up to second 115 (limit 116:0).
func (f *pitrFixture) run() PITRRun {
	plan := &pitr.RestorePlan{StreamID: "s1", ChainID: "ch1", Chunks: f.chunks, ChunkCount: len(f.chunks),
		Limit: pitr.Timestamp{T: 116}, TargetTime: time.Unix(115, 0).UTC(),
		Base: pitr.Base{ID: f.base.ID}}
	return PITRRun{Plan: plan, Base: f.base}
}

func pitrRequest(dbs ...string) models.RestoreRequest {
	at := time.Unix(115, 0).UTC()
	return models.RestoreRequest{MongoURI: "mongodb://u:secret@db:27017", PITR: &models.PITRTarget{StreamID: "s1", At: &at}, Databases: dbs}
}

func (f *pitrFixture) engine(r *pitrRunner, admin *pitrAdmin) *Engine {
	return NewEngine(f.store, "", WithRunner(r.run), WithDecryptor(f.dec), WithDatabaseAdmin(admin), WithDatabaseLister(admin.list))
}

func hasArg(args []string, want string) bool { return slices.Contains(args, want) }

func TestPITRRestoreWholeInstance(t *testing.T) {
	f := newPITRFixture(t)
	r := &pitrRunner{applied: "2026-10-05T12:00:00.000+0000	applied 3 oplog entries"}
	admin := &pitrAdmin{names: []string{"shop", "other"}}
	e := f.engine(r, admin)
	req := pitrRequest()
	rec, err := e.PreparePITR(req, f.run())
	if err != nil {
		t.Fatal(err)
	}
	suffix := rec.PITR.CloneSuffix
	if !strings.HasPrefix(suffix, "_rescue_") || rec.TargetDatabase != "*"+suffix || rec.BackupID != "b1" {
		t.Fatalf("record = %+v", rec)
	}
	rec, err = e.ExecutePITR(context.Background(), req, f.run(), rec)
	if err != nil {
		t.Fatalf("ExecutePITR: %v (%s)", err, rec.ErrorMessage)
	}
	if rec.Status != models.RestoreStatusCompleted || rec.PITR.OpsReplayed != 3 || rec.PITR.OpsApplied == nil || *rec.PITR.OpsApplied != 3 {
		t.Fatalf("record = %+v, pitr = %+v", rec, rec.PITR)
	}
	if len(r.calls) != 2 {
		t.Fatalf("%d mongorestore runs, want 2", len(r.calls))
	}
	pass1, pass2 := r.calls[0], r.calls[1]
	for _, want := range []string{"--archive", "--gzip", "--nsExclude=admin.*", "--nsExclude=config.*", "--nsExclude=local.*",
		"--nsFrom=$db$.$coll$", "--nsTo=$db$" + suffix + ".$coll$"} {
		if !hasArg(pass1, want) {
			t.Errorf("pass 1 args %v lack %s", pass1, want)
		}
	}
	if hasArg(pass1, "--oplogReplay") {
		t.Errorf("pass 1 replays the oplog: %v", pass1)
	}
	if !hasArg(pass2, "--oplogReplay") || !hasArg(pass2, "--oplogLimit=116:0") || !hasArg(pass2, "--archive") {
		t.Errorf("pass 2 args = %v", pass2)
	}
	for _, args := range r.calls {
		if strings.Contains(strings.Join(args, " "), "secret") {
			t.Fatalf("credentials in argv: %v", args)
		}
	}
	archive := string(r.stdins[1])
	for _, want := range []string{"shop" + suffix + ".orders", "other" + suffix + ".items", "marker-1", "marker-2", "marker-3"} {
		if !strings.Contains(archive, want) {
			t.Errorf("synthetic archive lacks %q", want)
		}
	}
	if strings.Contains(archive, "marker-4") || strings.Contains(archive, `shop.orders`) {
		t.Error("synthetic archive holds an entry past the limit or an unrenamed namespace")
	}
	if string(r.stdins[0]) == "" {
		t.Error("pass 1 got no base archive")
	}
	if len(admin.dropped) != 0 {
		t.Errorf("dropped %v after a successful restore", admin.dropped)
	}
}

func TestPITRRestoreSelectedDatabase(t *testing.T) {
	f := newPITRFixture(t)
	r := &pitrRunner{applied: "applied 2 oplog entries"}
	e := f.engine(r, &pitrAdmin{})
	req := pitrRequest("shop")
	rec, err := e.PreparePITR(req, f.run())
	if err != nil {
		t.Fatal(err)
	}
	clone := "shop" + rec.PITR.CloneSuffix
	if rec.SourceDatabase != "shop" || rec.TargetDatabase != clone {
		t.Fatalf("source %s target %s", rec.SourceDatabase, rec.TargetDatabase)
	}
	if rec, err = e.ExecutePITR(context.Background(), req, f.run(), rec); err != nil {
		t.Fatalf("ExecutePITR: %v (%s)", err, rec.ErrorMessage)
	}
	pass1 := r.calls[0]
	for _, want := range []string{"--nsInclude=shop.*", "--nsFrom=shop.*", "--nsTo=" + clone + ".*"} {
		if !hasArg(pass1, want) {
			t.Errorf("pass 1 args %v lack %s", pass1, want)
		}
	}
	if hasArg(pass1, "--nsExclude=admin.*") {
		t.Errorf("pass 1 of one database excludes system databases: %v", pass1)
	}
	archive := string(r.stdins[1])
	if strings.Contains(archive, "marker-2") || !strings.Contains(archive, "marker-3") {
		t.Error("the synthetic archive does not hold exactly the selected database")
	}
	if rec.PITR.OpsReplayed != 2 {
		t.Fatalf("ops replayed = %d, want 2", rec.PITR.OpsReplayed)
	}
}

func TestPITRRestoreOpCount(t *testing.T) {
	t.Run("mismatch fails and keeps the clones", func(t *testing.T) {
		f := newPITRFixture(t)
		r := &pitrRunner{applied: "applied 2 oplog entries"}
		admin := &pitrAdmin{}
		e := f.engine(r, admin)
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		rec, err := e.ExecutePITR(context.Background(), req, f.run(), rec)
		if !errors.Is(err, ErrOpCountMismatch) || rec.Status != models.RestoreStatusFailed {
			t.Fatalf("err = %v, status %s", err, rec.Status)
		}
		if len(admin.dropped) != 0 || !strings.Contains(rec.ErrorMessage, "kept for inspection") {
			t.Fatalf("dropped %v, message %q", admin.dropped, rec.ErrorMessage)
		}
	})
	t.Run("missing count warns", func(t *testing.T) {
		f := newPITRFixture(t)
		e := f.engine(&pitrRunner{}, &pitrAdmin{})
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		rec, err := e.ExecutePITR(context.Background(), req, f.run(), rec)
		if err != nil || rec.Status != models.RestoreStatusCompleted || !strings.Contains(rec.Warning, "cross-checked") {
			t.Fatalf("err = %v, status %s, warning %q", err, rec.Status, rec.Warning)
		}
	})
}

func TestPITRRestoreRefusals(t *testing.T) {
	t.Run("tampered chunk fails and drops the clones", func(t *testing.T) {
		f := newPITRFixture(t)
		f.chunks[1].SHA256 = strings.Repeat("0", 64)
		r := &pitrRunner{applied: "applied 3 oplog entries"}
		admin := &pitrAdmin{}
		e := f.engine(r, admin)
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		admin.names = []string{"shop"}
		r.onRun = func([]string) {
			// Pass 1 creates the clones.
			admin.mu.Lock()
			defer admin.mu.Unlock()
			if len(admin.names) == 1 {
				admin.names = append(admin.names, "shop"+rec.PITR.CloneSuffix, "other"+rec.PITR.CloneSuffix)
			}
		}
		rec, err := e.ExecutePITR(context.Background(), req, f.run(), rec)
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch", err)
		}
		slices.Sort(admin.dropped)
		if want := []string{"other" + rec.PITR.CloneSuffix, "shop" + rec.PITR.CloneSuffix}; !slices.Equal(admin.dropped, want) {
			t.Fatalf("dropped %v, want %v", admin.dropped, want)
		}
	})
	t.Run("existing clone", func(t *testing.T) {
		f := newPITRFixture(t)
		r := &pitrRunner{}
		admin := &pitrAdmin{}
		e := f.engine(r, admin)
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		admin.names = []string{"shop" + rec.PITR.CloneSuffix}
		if _, err := e.ExecutePITR(context.Background(), req, f.run(), rec); !errors.Is(err, ErrCloneExists) {
			t.Fatalf("err = %v, want ErrCloneExists", err)
		}
		if len(r.calls) != 0 || len(admin.dropped) != 0 {
			t.Fatalf("ran %d times, dropped %v", len(r.calls), admin.dropped)
		}
	})
	t.Run("no key", func(t *testing.T) {
		f := newPITRFixture(t)
		r := &pitrRunner{}
		e := NewEngine(f.store, "", WithRunner(r.run))
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		if _, err := e.ExecutePITR(context.Background(), req, f.run(), rec); !errors.Is(err, encryption.ErrEncryptionKeyRequired) {
			t.Fatalf("err = %v, want ErrEncryptionKeyRequired", err)
		}
		if len(r.calls) != 0 {
			t.Fatal("mongorestore ran without a key")
		}
	})
	t.Run("no server version", func(t *testing.T) {
		f := newPITRFixture(t)
		f.base.ServerVersion = ""
		e := f.engine(&pitrRunner{}, &pitrAdmin{})
		req := pitrRequest()
		rec, _ := e.PreparePITR(req, f.run())
		if _, err := e.ExecutePITR(context.Background(), req, f.run(), rec); !errors.Is(err, ErrNoServerVersion) {
			t.Fatalf("err = %v, want ErrNoServerVersion", err)
		}
	})
	t.Run("prepare", func(t *testing.T) {
		f := newPITRFixture(t)
		e := f.engine(&pitrRunner{}, &pitrAdmin{})
		inPlace := pitrRequest()
		no := false
		inPlace.SafeClone = &no
		inPlace.ConfirmInPlace = true
		if _, err := e.PreparePITR(inPlace, f.run()); !errors.Is(err, models.ErrPITRInPlace) {
			t.Fatalf("in place: err = %v, want ErrPITRInPlace", err)
		}
		long := pitrRequest(strings.Repeat("d", 50))
		if _, err := e.PreparePITR(long, f.run()); err == nil {
			t.Fatal("a clone name past 63 bytes was accepted")
		}
		if _, err := e.PreparePITR(pitrRequest("admin"), f.run()); err == nil {
			t.Fatal("admin was accepted")
		}
		if _, err := e.PreparePITR(pitrRequest(), PITRRun{}); err == nil {
			t.Fatal("a run without a plan was accepted")
		}
	})
}

func TestAppliedOps(t *testing.T) {
	if n, ok := appliedOps("x\napplied 0 oplog entries\n...applied 12 oplog entries\n"); !ok || n != 12 {
		t.Fatalf("appliedOps = %d, %v", n, ok)
	}
	if _, ok := appliedOps("done"); ok {
		t.Fatal("a count without the line")
	}
}
