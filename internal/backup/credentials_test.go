package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// testHolder is a models.CredentialHolder; with cause set, the held context
// ends with it shortly after the run started (a lease that could not be kept).
type testHolder struct {
	mu       sync.Mutex
	cause    error
	id, uri  string
	released int
}

func (h *testHolder) Hold(ctx context.Context, id, uri string) (context.Context, string, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.id, h.uri = id, uri
	held, cancel := context.WithCancelCause(ctx)
	if h.cause != nil {
		cause := h.cause
		time.AfterFunc(50*time.Millisecond, func() { cancel(cause) })
	}
	return held, strings.Replace(uri, "db:", "leased:pw@db:", 1), func() {
		h.mu.Lock()
		h.released++
		h.mu.Unlock()
		cancel(context.Canceled)
	}, nil
}

var errLeaseLost = errors.New("the vault lease of the credentials could not be kept valid")

func TestRunHoldsTheConnectionsCredentials(t *testing.T) {
	runner := func(_ context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		return io.NopCloser(bytes.NewReader([]byte("archive"))), strings.NewReader(""), func() error { return nil }, nil
	}
	h := &testHolder{}
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(runner), WithCredentialHolder(h))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", ConnectionID: "conn_v", MongoURI: "mongodb://db:27017/"})
	if err != nil || record.Status != models.StatusCompleted {
		t.Fatalf("run = %s, %v", record.Status, err)
	}
	if h.id != "conn_v" || !strings.HasPrefix(h.uri, "mongodb://db:27017/") || h.released != 1 {
		t.Fatalf("hold id %q uri %q released %d", h.id, h.uri, h.released)
	}
	// Without a managed connection nothing is held.
	h2 := &testHolder{}
	engine = NewEngine(storage.NewMockStorage(), "mongodb://localhost:27017", WithRunner(runner), WithCredentialHolder(h2))
	if _, err = engine.Run(context.Background(), models.BackupOptions{Database: "shop"}); err != nil || h2.id != "" {
		t.Fatalf("default URI run = %v, held %q", err, h2.id)
	}
}

func TestRunFailsWhenItsCredentialsEnd(t *testing.T) {
	runner := func(ctx context.Context, _ string, _ ...string) (io.ReadCloser, io.Reader, func() error, error) {
		pr, pw := io.Pipe()
		go func() {
			<-ctx.Done()
			_ = pw.CloseWithError(ctx.Err())
		}()
		return pr, strings.NewReader(""), func() error { <-ctx.Done(); return ctx.Err() }, nil
	}
	h := &testHolder{cause: errLeaseLost}
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(runner), WithCredentialHolder(h))
	record, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", ConnectionID: "conn_v", MongoURI: "mongodb://db:27017/"})
	if record.Status != models.StatusFailed || !errors.Is(err, errLeaseLost) || !strings.Contains(record.ErrorMessage, "lease") {
		t.Fatalf("run = %s, %v (%q)", record.Status, err, record.ErrorMessage)
	}
	if h.released != 1 {
		t.Fatalf("released %d; want 1", h.released)
	}
}
