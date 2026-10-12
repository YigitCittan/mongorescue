package integrity

import (
	"context"
	"errors"
	"testing"
)

// lostFence is a fence of an instance that lost the HA lease.
type lostFence struct{}

var errLost = errors.New("this instance does not hold the HA lease")

func (lostFence) Check() error  { return errLost }
func (lostFence) Epoch() uint64 { return 0 }

// TestNoIntegrityWorkWithoutTheFence checks that scans and scheduled work do not run
// on an instance that lost the HA lease.
func TestNoIntegrityWorkWithoutTheFence(t *testing.T) {
	f := newFixture(t)
	f.svc.cfg.Fence = lostFence{}
	if _, err := f.svc.ScanTarget(context.Background(), "tgt_local", TriggerManual); !errors.Is(err, errLost) {
		t.Fatalf("ScanTarget without the HA lease = %v; want the fence's error", err)
	}
	f.svc.RunDue(context.Background())
	if _, ok := f.svc.LastScan(context.Background(), "tgt_local"); ok {
		t.Fatal("a scan ran without the HA lease")
	}
}
