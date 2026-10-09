package operations

import "time"

// SetBulkCap lowers the item cap of bulk operations, so tests need not store
// MaxBulkItems records.
func SetBulkCap(s *Service, n int) { s.bulkCap = n }

// SetNow replaces the service's clock.
func SetNow(s *Service, now func() time.Time) { s.now = now }

// ReplayRunForTest is a measured replay for FitReplayForTest.
type ReplayRunForTest struct{ Entries, Bytes, Seconds float64 }

// FitReplayForTest runs fitReplay on runs.
func FitReplayForTest(runs []ReplayRunForTest) (model string, perEntry, perByte float64) {
	in := make([]replayRun, 0, len(runs))
	for _, r := range runs {
		in = append(in, replayRun{entries: r.Entries, bytes: r.Bytes, seconds: r.Seconds})
	}
	f := fitReplay(in)
	return f.model, f.perEntry, f.perByte
}
