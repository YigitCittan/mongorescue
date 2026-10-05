package backup

import (
	"bytes"
	"context"
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
