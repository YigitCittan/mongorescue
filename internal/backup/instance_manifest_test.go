package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

func TestInstanceBackupCapturesAnInstanceManifest(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	reader, _ := opTimes(pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}}, pitr.OpTime{TS: pitr.Timestamp{T: 105, I: 1}})
	var mu sync.Mutex
	var asked []string
	count := int64(10)
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(runner), WithEncryptor(testEncryptor(t)), WithOpTimeReader(reader),
		WithDatabaseLister(func(context.Context, string) ([]string, error) {
			return []string{"admin", "shop", "config", "crm", "local", "shop_rescue_20261001_000000"}, nil
		}),
		WithManifestCapturer(func(_ context.Context, _, db string) (*models.Manifest, error) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, db)
			count++
			return &models.Manifest{ServerVersion: "8.0.4", Collections: []models.CollectionManifest{
				{Name: "orders", DocumentsMin: count, DocumentsMax: count},
			}}, nil
		}))
	rec, err := engine.Run(context.Background(), instanceOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.HasManifest || rec.Manifest == nil || rec.ServerVersion != "8.0.4" {
		t.Fatalf("record = %+v", rec)
	}
	if got := strings.Join(asked, ","); got != "shop,crm,shop,crm" {
		t.Fatalf("manifests of %s; want shop and crm before and after the dump", got)
	}
	names := []string{}
	for _, c := range rec.Manifest.Collections {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "crm.orders,shop.orders" {
		t.Fatalf("collections %v", names)
	}
	// Counts moved between the captures: the range covers both.
	if got := strings.Join(rec.InstanceDatabases, ","); got != "crm,shop,shop_rescue_20261001_000000" {
		t.Fatalf("instance databases %s", got)
	}
	if c := rec.Manifest.Collection("shop.orders"); c == nil || c.DocumentsMin != 11 || c.DocumentsMax != 13 {
		t.Fatalf("shop.orders = %+v", c)
	}
}

// exactEngine is an instance backup engine whose base is consistent at 105:1, with
// shop.orders and shop.users in its manifest and counter as its exact counter.
func exactEngine(t *testing.T, counter ExactCountFunc) *Engine {
	t.Helper()
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	reader, _ := opTimes(pitr.OpTime{TS: pitr.Timestamp{T: 100, I: 1}}, pitr.OpTime{TS: pitr.Timestamp{T: 105, I: 1}})
	return NewEngine(storage.NewMockStorage(), "", WithRunner(runner), WithEncryptor(testEncryptor(t)), WithOpTimeReader(reader),
		WithDatabaseLister(func(context.Context, string) ([]string, error) { return []string{"shop"}, nil }),
		WithManifestCapturer(func(context.Context, string, string) (*models.Manifest, error) {
			return &models.Manifest{Collections: []models.CollectionManifest{
				{Name: "orders", DocumentsMin: 20000, DocumentsMax: 20000}, {Name: "users", DocumentsMin: 3, DocumentsMax: 3},
			}}, nil
		}),
		WithExactCounter(counter))
}

func TestBaseRecordsExactCountsAtTAfter(t *testing.T) {
	var gotNS []string
	var gotAt pitr.Timestamp
	engine := exactEngine(t, func(_ context.Context, _ string, namespaces []string, at pitr.Timestamp) (*models.ExactCounts, error) {
		gotNS, gotAt = namespaces, at
		return &models.ExactCounts{At: at, Method: models.CountMethodSnapshot, Counts: map[string]int64{"shop.orders": 20001},
			Unverified: map[string]string{"shop.users": "renamed to shop.people at Timestamp(106, 1)"}}, nil
	})
	rec, err := engine.Run(context.Background(), instanceOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(gotNS, ",") != "shop.orders,shop.users" || gotAt != (pitr.Timestamp{T: 105, I: 1}) {
		t.Fatalf("counted %v at %s", gotNS, gotAt)
	}
	m := rec.Manifest
	if m == nil || m.ExactAt == nil || *m.ExactAt != gotAt || m.ExactMethod != models.CountMethodSnapshot {
		t.Fatalf("manifest = %+v", m)
	}
	if c := m.Collection("shop.orders"); c.Exact == nil || *c.Exact != 20001 || c.DocumentsMax != 20000 {
		t.Fatalf("shop.orders = %+v", c)
	}
	if c := m.Collection("shop.users"); c.Exact != nil || !strings.Contains(c.ExactNote, "renamed") {
		t.Fatalf("shop.users = %+v", c)
	}
}

func TestBaseWithoutExactCountsStillCompletes(t *testing.T) {
	engine := exactEngine(t, func(context.Context, string, []string, pitr.Timestamp) (*models.ExactCounts, error) {
		return nil, errors.New("connection refused")
	})
	rec, err := engine.Run(context.Background(), instanceOptions())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.HasManifest || rec.Manifest.ExactAt != nil || rec.Manifest.Collection("shop.orders").Exact != nil {
		t.Fatalf("manifest = %+v", rec.Manifest)
	}
}
