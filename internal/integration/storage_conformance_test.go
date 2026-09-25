//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

// multipartPayloadSize exceeds the S3 driver's 5 MiB part size twice over, forcing a
// real multipart upload with at least three parts.
const multipartPayloadSize = 12 << 20

// TestStorageConformance runs the provider-agnostic storage contract against local disk
// and every configured S3-compatible provider.
func TestStorageConformance(t *testing.T) {
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			runStorageConformance(t, target.Storage)
		})
	}
}

// runStorageConformance verifies the storage.Storage contract on st. Every object is
// written under a unique prefix that is purged on cleanup.
func runStorageConformance(t *testing.T, st storage.Storage) {
	prefix := "it-conformance/" + randomHex(t, 6) + "/"
	cleanupPrefix(t, st, prefix)

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	t.Cleanup(cancel)

	t.Run("save retrieve stat small", func(t *testing.T) {
		key := prefix + "small.bin"
		payload := []byte("mongorescue conformance payload")
		obj, err := st.Save(ctx, key, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if obj == nil || obj.Key != key {
			t.Fatalf("Save returned %+v; want key %q", obj, key)
		}
		assertContent(ctx, t, st, key, payload)

		info, err := st.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.SizeBytes != int64(len(payload)) {
			t.Fatalf("Stat size = %d; want %d", info.SizeBytes, len(payload))
		}
	})

	t.Run("multipart payload checksum", func(t *testing.T) {
		key := prefix + "large.bin"
		payload := make([]byte, multipartPayloadSize)
		if _, err := rand.Read(payload); err != nil {
			t.Fatalf("random payload: %v", err)
		}
		// Hide the concrete type so drivers cannot shortcut via io.Seeker/ReaderAt.
		obj, err := st.Save(ctx, key, struct{ io.Reader }{bytes.NewReader(payload)})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if obj.SizeBytes != int64(len(payload)) {
			t.Fatalf("Save size = %d; want %d", obj.SizeBytes, len(payload))
		}
		assertContent(ctx, t, st, key, payload)
	})

	t.Run("empty object", func(t *testing.T) {
		key := prefix + "empty.bin"
		if _, err := st.Save(ctx, key, bytes.NewReader(nil)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		assertContent(ctx, t, st, key, nil)
		info, err := st.Stat(ctx, key)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if info.SizeBytes != 0 {
			t.Fatalf("Stat size = %d; want 0", info.SizeBytes)
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		key := prefix + "overwrite.bin"
		if _, err := st.Save(ctx, key, strings.NewReader("first version, longer")); err != nil {
			t.Fatalf("Save first: %v", err)
		}
		second := []byte("second")
		if _, err := st.Save(ctx, key, bytes.NewReader(second)); err != nil {
			t.Fatalf("Save second: %v", err)
		}
		assertContent(ctx, t, st, key, second)
	})

	t.Run("missing key sentinel", func(t *testing.T) {
		key := prefix + "does-not-exist.bin"
		if _, err := st.Retrieve(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Retrieve missing: got %v; want storage.ErrNotFound", err)
		}
		if _, err := st.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat missing: got %v; want storage.ErrNotFound", err)
		}
	})

	t.Run("nested key list delete", func(t *testing.T) {
		nested := prefix + "nested/a/b/c/object.archive.gz"
		sibling := prefix + "nested/a/other.bin"
		for _, k := range []string{nested, sibling} {
			if _, err := st.Save(ctx, k, strings.NewReader(k)); err != nil {
				t.Fatalf("Save %s: %v", k, err)
			}
		}

		objs, err := st.List(ctx, prefix+"nested/a/b/")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(objs) != 1 || objs[0].Key != nested || objs[0].SizeBytes != int64(len(nested)) {
			t.Fatalf("List(prefix) = %+v; want exactly %q", objs, nested)
		}

		all, err := st.List(ctx, prefix+"nested/")
		if err != nil {
			t.Fatalf("List all: %v", err)
		}
		if len(all) != 2 {
			t.Fatalf("List(nested/) returned %d objects; want 2", len(all))
		}

		if err := st.Delete(ctx, nested); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := st.Stat(ctx, nested); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat after Delete: got %v; want storage.ErrNotFound", err)
		}
		if _, err := st.Stat(ctx, sibling); err != nil {
			t.Fatalf("sibling must survive Delete: %v", err)
		}
	})

	t.Run("context cancel mid upload", func(t *testing.T) {
		key := prefix + "cancelled.bin"
		uploadCtx, cancelUpload := context.WithCancel(ctx)
		defer cancelUpload()

		// Deliver more than one multipart part, then cancel while data is still flowing.
		r := &cancellingReader{
			remaining: multipartPayloadSize,
			cancelAt:  6 << 20,
			cancel:    cancelUpload,
		}
		if _, err := st.Save(uploadCtx, key, r); err == nil {
			t.Fatal("Save with cancelled context succeeded; want error")
		}
		if _, err := st.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("cancelled upload left an object behind: Stat err = %v", err)
		}
	})
}

// assertContent retrieves key and compares its SHA-256 and length with want.
func assertContent(ctx context.Context, t *testing.T, st storage.Storage, key string, want []byte) {
	t.Helper()
	rc, err := st.Retrieve(ctx, key)
	if err != nil {
		t.Fatalf("Retrieve %s: %v", key, err)
	}
	defer rc.Close()

	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	wantSum := sha256.Sum256(want)
	if n != int64(len(want)) || !bytes.Equal(h.Sum(nil), wantSum[:]) {
		t.Fatalf("content mismatch for %s: got %d bytes, want %d (checksum differs)", key, n, len(want))
	}
}

// cancellingReader yields zero bytes until remaining is exhausted and invokes cancel
// once cancelAt bytes have been produced.
type cancellingReader struct {
	remaining int
	produced  int
	cancelAt  int
	cancel    context.CancelFunc
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.produced += n
	if r.produced >= r.cancelAt && r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return n, nil
}
