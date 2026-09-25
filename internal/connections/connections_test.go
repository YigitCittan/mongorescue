package connections_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/connections"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/redact"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

const secretURI = "mongodb://backup:Sup3r-secret@db1.internal:27017,db2.internal:27017/?replicaSet=rs0&authSource=admin"

// prober is a scripted connections.Prober.
type prober struct {
	mu      sync.Mutex
	err     error
	block   bool
	dbs     []connections.Database
	cols    []connections.Collection
	lastURI string
}

func (p *prober) record(uri string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastURI = uri
	return p.block, p.err
}

func (p *prober) Ping(ctx context.Context, uri string) (connections.ServerInfo, error) {
	block, err := p.record(uri)
	if block {
		<-ctx.Done()
		return connections.ServerInfo{}, ctx.Err()
	}
	if err != nil {
		return connections.ServerInfo{}, err
	}
	return connections.ServerInfo{Version: "8.0.1"}, nil
}

func (p *prober) ListDatabases(_ context.Context, uri string) ([]connections.Database, error) {
	_, err := p.record(uri)
	return p.dbs, err
}

func (p *prober) ListCollections(_ context.Context, uri, _ string) ([]connections.Collection, error) {
	_, err := p.record(uri)
	return p.cols, err
}

func (p *prober) uri() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastURI
}

func newService(t *testing.T, opts ...connections.Option) (*connections.Service, *store.SQLiteStore, *prober) {
	t.Helper()
	st := storetest.New(t)
	p := &prober{}
	return connections.NewService(st, p, opts...), st, p
}

func TestCreateListGetAreRedacted(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()

	c, err := svc.Create(ctx, connections.Input{Name: "  Production  ", URI: secretURI, Description: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Production" || !strings.HasPrefix(c.ID, "conn_") || c.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", c)
	}
	list, _ := svc.List(ctx)
	got, _ := svc.Get(ctx, c.ID)
	for _, v := range []any{c, list, got} {
		raw, _ := json.Marshal(v)
		if strings.Contains(string(raw), "Sup3r-secret") || !strings.Contains(string(raw), redact.Mask) {
			t.Fatalf("response leaks or lacks the mask: %s", raw)
		}
	}
	full, err := svc.Resolve(ctx, c.ID)
	if err != nil || full.URI != secretURI {
		t.Fatalf("Resolve must return the full URI: %v", err)
	}
	stored, _ := st.GetConnection(ctx, c.ID)
	if stored.URI != secretURI {
		t.Fatal("repository must decrypt transparently")
	}
	if _, err := svc.Resolve(ctx, ""); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("Resolve(empty) = %v", err)
	}
	if _, err := svc.Get(ctx, "conn_missing"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("Get(missing) = %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	for name, in := range map[string]connections.Input{
		"no name":         {URI: secretURI},
		"control chars":   {Name: "a\x00b", URI: secretURI},
		"long name":       {Name: strings.Repeat("n", 101), URI: secretURI},
		"no uri":          {Name: "x"},
		"bad scheme":      {Name: "x", URI: "postgres://u:p@h/db"},
		"raw slash in pw": {Name: "x", URI: "mongodb://u:pa/ss@h/db"},
		"long desc":       {Name: "x", URI: secretURI, Description: strings.Repeat("d", 501)},
	} {
		_, err := svc.Create(ctx, in)
		if !errors.Is(err, connections.ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", name, err)
		}
		if err != nil && in.URI != "" && strings.Contains(err.Error(), in.URI) {
			t.Errorf("%s: error echoes the URI", name)
		}
	}
	if _, err := svc.Create(ctx, connections.Input{Name: "x", URI: redact.URI(secretURI)}); !errors.Is(err, connections.ErrMaskedURI) {
		t.Fatalf("masked uri on create: %v", err)
	}
}

func TestUpdateKeepsSecretOnlyForExactRedactedForm(t *testing.T) {
	svc, _, p := newService(t)
	ctx := context.Background()
	c, _ := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})
	if _, err := svc.Test(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	// Round-tripping the redacted URI keeps the stored credentials and test result.
	updated, err := svc.Update(ctx, c.ID, connections.Input{Name: "prod renamed", URI: c.URI})
	if err != nil {
		t.Fatal(err)
	}
	if full, _ := svc.Resolve(ctx, c.ID); full.URI != secretURI || updated.Name != "prod renamed" || !updated.LastTestOK {
		t.Fatalf("redacted round trip: %+v", updated)
	}

	// A different masked URI (another host) must not inherit the password.
	other := strings.Replace(c.URI, "db1.internal", "evil.example.com", 1)
	if _, err = svc.Update(ctx, c.ID, connections.Input{Name: "x", URI: other}); !errors.Is(err, connections.ErrMaskedURI) {
		t.Fatalf("masked uri for another host: %v", err)
	}
	if full, _ := svc.Resolve(ctx, c.ID); full.URI != secretURI {
		t.Fatal("rejected update must not change the stored URI")
	}

	// A new full URI replaces it and clears the stale test result.
	const next = "mongodb://other:pw@db3.internal:27017/"
	updated, err = svc.Update(ctx, c.ID, connections.Input{Name: "x", URI: next})
	if err != nil || updated.LastTestAt != nil || updated.LastTestOK || updated.ServerVersion != "" {
		t.Fatalf("update with new uri = %+v, %v", updated, err)
	}
	if full, _ := svc.Resolve(ctx, c.ID); full.URI != next {
		t.Fatalf("new uri not stored")
	}
	if _, err := svc.Update(ctx, "conn_missing", connections.Input{Name: "x", URI: next}); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	_ = p
}

func TestKeepSecret(t *testing.T) {
	for _, tc := range []struct {
		in, stored, want string
		err              error
	}{
		{"mongodb://h/db", secretURI, "mongodb://h/db", nil},
		{redact.URI(secretURI), secretURI, secretURI, nil},
		{redact.URI(secretURI), "", "", connections.ErrMaskedURI},
		{"mongodb://backup:" + redact.Mask + "@db9:27017/", secretURI, "", connections.ErrMaskedURI},
	} {
		got, err := connections.KeepSecret(tc.in, tc.stored)
		if got != tc.want || !errors.Is(err, tc.err) {
			t.Errorf("KeepSecret(%q) = %q, %v", tc.in, got, err)
		}
	}
}

func TestDeleteRefusedWhileJobsReferenceIt(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	c, _ := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})
	if err := st.SaveJob(ctx, &models.Job{ID: "job_1", Name: "j", Database: "shop", ConnectionID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, c.ID); !errors.Is(err, connections.ErrInUse) {
		t.Fatalf("delete in use: %v", err)
	}
	if err := st.DeleteJob(ctx, "job_1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, c.ID); err != nil {
		t.Fatalf("delete unused: %v", err)
	}
	if err := svc.Delete(ctx, c.ID); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestTestRecordsResultAndRedactsErrors(t *testing.T) {
	svc, _, p := newService(t)
	ctx := context.Background()
	c, _ := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})

	res, err := svc.Test(ctx, c.ID)
	if err != nil || !res.OK || res.ServerVersion != "8.0.1" || p.uri() != secretURI {
		t.Fatalf("Test = %+v, %v (prober must get the full URI)", res, err)
	}
	got, _ := svc.Get(ctx, c.ID)
	if got.LastTestAt == nil || !got.LastTestOK || got.ServerVersion != "8.0.1" {
		t.Fatalf("test result not recorded: %+v", got)
	}

	p.mu.Lock()
	p.err = errors.New("connection() error occurred during connection handshake: auth error: " + secretURI + " password=Sup3r-secret")
	p.mu.Unlock()
	res, err = svc.Test(ctx, c.ID)
	if err != nil || res.OK || res.Error == "" || strings.Contains(res.Error, "Sup3r-secret") {
		t.Fatalf("failed test = %+v, %v (error must be redacted)", res, err)
	}
	got, _ = svc.Get(ctx, c.ID)
	if got.LastTestOK || got.LastTestError == "" || strings.Contains(got.LastTestError, "Sup3r-secret") || got.ServerVersion != "8.0.1" {
		t.Fatalf("failed test not recorded safely: %+v", got)
	}
	if _, err := svc.Test(ctx, "conn_missing"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("test missing: %v", err)
	}
}

func TestTestHonoursTimeout(t *testing.T) {
	svc, _, p := newService(t, connections.WithTestTimeout(200*time.Millisecond))
	p.block = true
	start := time.Now()
	res, err := svc.TestURI(context.Background(), "mongodb://10.255.255.1:27017/", "")
	if err != nil || res.OK || res.Error == "" {
		t.Fatalf("TestURI = %+v, %v", res, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("test took %v; want it bounded by the timeout", elapsed)
	}
}

func TestTestURI(t *testing.T) {
	svc, _, p := newService(t)
	ctx := context.Background()
	c, _ := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})

	if res, err := svc.TestURI(ctx, "mongodb://h:27017/", ""); err != nil || !res.OK {
		t.Fatalf("TestURI(plain) = %+v, %v", res, err)
	}
	// The edit form may test the redacted URI of an existing connection.
	if res, err := svc.TestURI(ctx, c.URI, c.ID); err != nil || !res.OK || p.uri() != secretURI {
		t.Fatalf("TestURI(redacted, id) = %+v, %v", res, err)
	}
	if _, err := svc.TestURI(ctx, c.URI, ""); !errors.Is(err, connections.ErrMaskedURI) {
		t.Fatalf("TestURI(redacted, no id) = %v", err)
	}
	if _, err := svc.TestURI(ctx, "http://nope", ""); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("TestURI(invalid) = %v", err)
	}
	if _, err := svc.TestURI(ctx, c.URI, "conn_missing"); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("TestURI(missing id) = %v", err)
	}
}

func TestDiscovery(t *testing.T) {
	svc, _, p := newService(t)
	ctx := context.Background()
	c, _ := svc.Create(ctx, connections.Input{Name: "prod", URI: secretURI})
	p.dbs = []connections.Database{{Name: "shop", SizeBytes: 10}, {Name: "admin"}, {Name: "analytics", Empty: true}, {Name: "local"}, {Name: "config"}}
	p.cols = []connections.Collection{{Name: "orders", Type: "collection"}, {Name: "by_day", Type: "view"}}

	dbs, err := svc.Databases(ctx, c.ID, false)
	if err != nil || len(dbs) != 2 || dbs[0].Name != "analytics" || dbs[1].Name != "shop" {
		t.Fatalf("Databases = %+v, %v; want sorted user databases", dbs, err)
	}
	all, _ := svc.Databases(ctx, c.ID, true)
	if len(all) != 5 {
		t.Fatalf("Databases(system) = %+v", all)
	}
	cols, err := svc.Collections(ctx, c.ID, "shop")
	if err != nil || len(cols) != 2 || cols[0].Name != "by_day" {
		t.Fatalf("Collections = %+v, %v", cols, err)
	}
	if _, err := svc.Collections(ctx, c.ID, " "); !errors.Is(err, connections.ErrInvalid) {
		t.Fatalf("Collections(empty db) = %v", err)
	}

	p.err = errors.New("dial " + secretURI)
	if _, err := svc.Databases(ctx, c.ID, false); !errors.Is(err, connections.ErrUnavailable) || strings.Contains(err.Error(), "Sup3r-secret") {
		t.Fatalf("Databases on failure = %v", err)
	}
	if _, err := svc.Collections(ctx, c.ID, "shop"); !errors.Is(err, connections.ErrUnavailable) || strings.Contains(err.Error(), "Sup3r-secret") {
		t.Fatalf("Collections on failure = %v", err)
	}
	if _, err := svc.Databases(ctx, "conn_missing", false); !errors.Is(err, connections.ErrNotFound) {
		t.Fatalf("Databases(missing) = %v", err)
	}
}

func TestNilProberIsUnavailable(t *testing.T) {
	st := storetest.New(t)
	svc := connections.NewService(st, nil)
	ctx := context.Background()
	c, err := svc.Create(ctx, connections.Input{Name: "x", URI: secretURI})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := svc.Test(ctx, c.ID); res.OK || res.Error == "" {
		t.Fatalf("Test without prober = %+v", res)
	}
	if _, err := svc.Databases(ctx, c.ID, false); !errors.Is(err, connections.ErrUnavailable) {
		t.Fatalf("Databases without prober = %v", err)
	}
}

func TestEnsureDefault(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	if created, err := svc.EnsureDefault(ctx, ""); created || err != nil {
		t.Fatalf("EnsureDefault(empty) = %v, %v", created, err)
	}
	if created, err := svc.EnsureDefault(ctx, secretURI); !created || err != nil {
		t.Fatalf("EnsureDefault = %v, %v", created, err)
	}
	if created, err := svc.EnsureDefault(ctx, "mongodb://other/"); created || err != nil {
		t.Fatalf("EnsureDefault with existing connections = %v, %v", created, err)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 || list[0].Name != "default" {
		t.Fatalf("connections = %+v", list)
	}
	if _, err := svc.EnsureDefault(ctx, "postgres://x"); err != nil {
		t.Fatalf("with connections present the URI is not even looked at: %v", err)
	}
}

func TestHostList(t *testing.T) {
	for uri, want := range map[string]string{
		secretURI:                            "db1.internal:27017,db2.internal:27017",
		"mongodb+srv://u:p@cluster0.x.net/a": "cluster0.x.net",
		"mongodb://localhost":                "localhost",
		"mongodb://h:1?x=@y":                 "h:1",
		"mongodb://":                         "mongodb",
	} {
		if got := connections.HostList(uri); got != want {
			t.Errorf("HostList(%q) = %q; want %q", uri, got, want)
		}
	}
}
