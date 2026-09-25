package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
)

// TestWithTxRollsBackOnPanic proves a panicking transaction does not leave the single
// pooled connection stuck in an open transaction.
func TestWithTxRollsBackOnPanic(t *testing.T) {
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "mongorescue.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v; want the original panic", r)
			}
		}()
		_ = s.withTx(context.Background(), func(tx *sql.Tx) error {
			if err := putJob(context.Background(), tx, &models.Job{ID: "inside", Name: "x"}); err != nil {
				t.Fatal(err)
			}
			panic("boom")
		})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.SaveJob(ctx, &models.Job{ID: "after", Name: "y"}); err != nil {
		t.Fatalf("store unusable after a panicking transaction: %v", err)
	}
	if _, err := s.GetJob(ctx, "inside"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write of the panicking transaction was not rolled back: %v", err)
	}
}
