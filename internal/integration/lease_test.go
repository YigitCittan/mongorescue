//go:build integration

package integration

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestLeaseStoreConditionalWrites checks the conditional writes behind the HA lease
// (storage.LeaseStore) on local disk and every configured S3 provider: the startup
// probe passes, and of concurrent replaces of one version exactly one wins. MinIO
// must enforce the conditions; another provider that does not is skipped, since HA
// refuses to start on it anyway.
func TestLeaseStoreConditionalWrites(t *testing.T) {
	t.Parallel()
	for _, target := range storageTargets(t) {
		t.Run(target.Name, func(t *testing.T) {
			ls, err := storage.AsLeaseStore(target.Storage)
			if err != nil {
				t.Fatalf("AsLeaseStore: %v", err)
			}
			prefix := "it-lease/" + randomHex(t, 6) + "/"
			cleanupPrefix(t, target.Storage, prefix)
			ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
			defer cancel()

			err = storage.ProbeLeaseStore(ctx, ls, prefix+"probe.json", target.Storage.Delete)
			if errors.Is(err, storage.ErrConditionalWritesUnsupported) && target.Name != "local" && target.Name != "minio" {
				t.Skipf("%s does not enforce conditional writes: %v", target.Name, err)
			}
			if err != nil {
				t.Fatalf("ProbeLeaseStore: %v", err)
			}

			key := prefix + "lease.json"
			v0, err := ls.CreateIfAbsent(ctx, key, []byte(`{"seq":0}`))
			if err != nil {
				t.Fatalf("CreateIfAbsent: %v", err)
			}
			if _, err = ls.CreateIfAbsent(ctx, key, []byte(`{"seq":-1}`)); !errors.Is(err, storage.ErrPreconditionFailed) {
				t.Fatalf("second CreateIfAbsent = %v; want ErrPreconditionFailed", err)
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			var winners []string
			for i := range 6 {
				wg.Go(func() {
					body := []byte(`{"seq":1,"writer":` + string(rune('0'+i)) + `}`)
					if _, werr := ls.ReplaceIfMatch(ctx, key, body, v0); werr == nil {
						mu.Lock()
						winners = append(winners, string(body))
						mu.Unlock()
					} else if !errors.Is(werr, storage.ErrPreconditionFailed) {
						t.Errorf("ReplaceIfMatch: %v", werr)
					}
				})
			}
			wg.Wait()
			if len(winners) != 1 {
				t.Fatalf("%d concurrent replaces of one version succeeded; want exactly 1", len(winners))
			}
			data, _, err := ls.ReadVersioned(ctx, key)
			if err != nil || string(data) != winners[0] {
				t.Fatalf("ReadVersioned = %q, %v; want the winner's write %q", data, err, winners[0])
			}
		})
	}
}
