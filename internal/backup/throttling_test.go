package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/runs"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// configRunner records the URI of mongodump's config file.
type configRunner struct {
	mu     sync.Mutex
	config string
	args   []string
}

func (c *configRunner) run(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.args = args
	for _, a := range args {
		if p, ok := strings.CutPrefix(a, "--config="); ok {
			data, _ := os.ReadFile(p)
			c.config = string(data)
		}
	}
	return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
}

func TestBackupPassesTheReadPreferenceInTheConfigFile(t *testing.T) {
	r := &configRunner{}
	var probed, manifestURI string
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run),
		WithMemberProbe(func(_ context.Context, uri string) (models.SourceMember, error) {
			probed = uri
			return models.SourceMember{Host: "db2:27017", State: models.MemberSecondary, SetName: "rs0"}, nil
		}),
		WithManifestCapturer(func(_ context.Context, uri, _ string) (*models.Manifest, error) {
			manifestURI = uri
			return &models.Manifest{}, nil
		}))
	rp := models.ReadPreference{Mode: models.ReadSecondary, Tags: []map[string]string{{"dc": "east"}}}
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop",
		MongoURI: "mongodb://u:secret@db1,db2/?replicaSet=rs0&readPreference=primary", ReadPreference: rp, NumParallelCollections: 2})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.args, " "), "readPreference") || strings.Contains(strings.Join(r.args, " "), "secret") {
		t.Fatalf("read preference or credentials on the command line: %v", r.args)
	}
	const want = "replicaSet=rs0&readPreference=secondary&readPreferenceTags=dc:east"
	if !strings.Contains(r.config, want) || strings.Contains(r.config, "readPreference=primary") {
		t.Fatalf("config file %q, want %q", r.config, want)
	}
	for name, uri := range map[string]string{"member probe": probed, "manifest": manifestURI} {
		if !strings.Contains(uri, want) {
			t.Errorf("%s used %q without the read preference", name, uri)
		}
	}
	if rec.SourceMember == nil || rec.SourceMember.Host != "db2:27017" || rec.ReadPreference != "secondary (dc=east)" {
		t.Fatalf("record member %+v, read preference %q", rec.SourceMember, rec.ReadPreference)
	}
	if got := (&argsRunner{args: r.args}).values("--numParallelCollections"); len(got) != 1 || got[0] != "2" {
		t.Fatalf("--numParallelCollections = %v", got)
	}
}

func TestBackupWithoutReadPreferenceKeepsTheURI(t *testing.T) {
	r := &configRunner{}
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run))
	const uri = "mongodb://u:secret@db1/?readPreference=secondaryPreferred"
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: uri})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.config, uri) || rec.ReadPreference != "" || rec.SourceMember != nil {
		t.Fatalf("config %q, record %q %+v", r.config, rec.ReadPreference, rec.SourceMember)
	}
	if len((&argsRunner{args: r.args}).values("--numParallelCollections")) != 0 {
		t.Fatalf("mongodump's default parallelism changed: %v", r.args)
	}
}

func TestBackupSecondaryNeedsASecondary(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		member models.SourceMember
		err    error
		fail   bool
	}{
		{"secondary on a primary fails", models.ReadSecondary, models.SourceMember{Host: "db1:27017", State: models.MemberPrimary, SetName: "rs0"}, nil, true},
		{"secondary on a standalone fails", models.ReadSecondary, models.SourceMember{State: models.MemberStandalone}, nil, true},
		{"secondary without an eligible member fails", models.ReadSecondary, models.SourceMember{}, errors.New("server selection error: no secondary"), true},
		{"secondary through mongos passes", models.ReadSecondary, models.SourceMember{State: models.MemberMongos}, nil, false},
		{"secondaryPreferred on a primary passes", models.ReadSecondaryPreferred, models.SourceMember{Host: "db1:27017", State: models.MemberPrimary}, nil, false},
		{"secondaryPreferred survives a failed probe", models.ReadSecondaryPreferred, models.SourceMember{}, errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &configRunner{}
			engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run),
				WithMemberProbe(func(context.Context, string) (models.SourceMember, error) { return tc.member, tc.err }))
			rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://db1",
				ReadPreference: models.ReadPreference{Mode: tc.mode}})
			if !tc.fail {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrNoEligibleMember) || rec.Status != models.StatusFailed {
				t.Fatalf("err %v, status %s", err, rec.Status)
			}
			if !strings.Contains(rec.ErrorMessage, "read preference secondary") {
				t.Fatalf("unclear error %q", rec.ErrorMessage)
			}
			if r.args != nil {
				t.Fatal("mongodump started")
			}
		})
	}
}

// fakeSlots records AcquireSlot calls and lets the caller wait.
type fakeSlots struct {
	key   string
	limit int
	wait  bool
}

func (f *fakeSlots) AcquireSlot(_ context.Context, key string, limit int, waiting func()) (func(), error) {
	f.key, f.limit = key, limit
	if f.wait && waiting != nil {
		waiting()
	}
	return func() {}, nil
}

func TestBackupWaitsForASlotOfItsConnection(t *testing.T) {
	slots := &fakeSlots{wait: true}
	reg := runs.NewRegistry()
	tracked, err := reg.Register(runs.Meta{Kind: models.RunBackup, ID: "bkp_1", Database: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	defer tracked.End()
	var phases []string
	r := &configRunner{}
	engine := NewEngine(storage.NewMockStorage(), "", WithConnectionSlots(slots), WithRunner(func(ctx context.Context, name string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		if p := reg.Progress("bkp_1"); p != nil {
			phases = append(phases, p.Phase)
		}
		return r.run(ctx, name, args...)
	}))
	opts := models.BackupOptions{Database: "shop", MongoURI: "mongodb://db1", ConnectionID: "conn_a", MaxConcurrentBackups: 2}
	rec, err := engine.Prepare(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Execute(tracked.Bind(context.Background()), opts, rec); err != nil {
		t.Fatal(err)
	}
	if slots.key != runs.ConnectionKey("conn_a") || slots.limit != 2 {
		t.Fatalf("slot %q limit %d", slots.key, slots.limit)
	}
	if len(phases) != 1 || phases[0] != models.PhaseDumping {
		t.Fatalf("phases at mongodump start %v", phases)
	}

	// No limit takes no slot.
	slots.key = ""
	opts.MaxConcurrentBackups = 0
	if _, err = engine.Run(context.Background(), opts); err != nil || slots.key != "" {
		t.Fatalf("unlimited backup took a slot: %q, %v", slots.key, err)
	}
}

// The per-connection limit of the runs manager queues the second backup until the
// first one ends.
func TestBackupsOfOneConnectionQueue(t *testing.T) {
	m := runs.NewManager(nil)
	defer func() { _ = m.Shutdown(context.Background()) }()
	release := make(chan struct{})
	started := make(chan string, 2)
	runner := func(_ context.Context, _ string, args ...string) (io.ReadCloser, io.Reader, func() error, error) {
		db := (&argsRunner{args: args}).values("--db")[0]
		started <- db
		if db == "first" {
			<-release
		}
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(runner), WithConnectionSlots(m))
	var wg sync.WaitGroup
	for _, db := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := engine.Run(context.Background(), models.BackupOptions{Database: db, MongoURI: "mongodb://db1",
				ConnectionID: "conn_a", MaxConcurrentBackups: 1}); err != nil {
				t.Error(err)
			}
		}()
		if db == "first" {
			if got := <-started; got != "first" {
				t.Fatalf("started %s", got)
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.SlotWaiters(runs.ConnectionKey("conn_a")) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second backup does not wait")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case db := <-started:
		t.Fatalf("%s started while the first backup holds the only slot", db)
	default:
	}
	close(release)
	if got := <-started; got != "second" {
		t.Fatalf("started %s", got)
	}
	wg.Wait()
}

// timedStorage stores what it reads and reports how long the read took.
type timedStorage struct {
	*storage.MockStorage
	took time.Duration
}

func (s *timedStorage) Save(ctx context.Context, key string, r io.Reader) (*models.StorageObject, error) {
	start := time.Now()
	obj, err := s.MockStorage.Save(ctx, key, r)
	s.took = time.Since(start)
	return obj, err
}

func TestBackupUploadCap(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 300_000)
	st := &timedStorage{MockStorage: storage.NewMockStorage()}
	engine := NewEngine(st, "", WithRunner(staticRunner(payload)))
	// 8 Mbit/s is 1 MB/s: 300 kB with a 100 kB bucket take about 0.2 s.
	rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://db1", MaxUploadMbps: 8})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SizeBytes != int64(len(payload)) {
		t.Fatalf("stored %d bytes", rec.SizeBytes)
	}
	if st.took < 150*time.Millisecond {
		t.Fatalf("a capped upload took %v, want about 200ms", st.took)
	}
	if engine.uploadMbps(models.BackupOptions{}) != 0 {
		t.Fatal("no default cap expected")
	}
	engine.maxUploadMbps = 50
	if engine.uploadMbps(models.BackupOptions{}) != 50 || engine.uploadMbps(models.BackupOptions{MaxUploadMbps: 5}) != 5 {
		t.Fatal("the job's cap must override the default")
	}
}
