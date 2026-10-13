//go:build integration

package integration

import (
	"os"
	"testing"
)

// TestWholeInstance runs, one after another, the tests that act on the whole MongoDB
// instance rather than on databases of their own: instance-scope base backups and
// point-in-time restores (every database of the server), whole-instance mongodump
// --oplog, admin and config compared before and after a restore, users created in
// admin, and waits for the majority optime to reach the newest oplog entry, which
// never settles while other tests write. Every other test runs in parallel.
//
// With MONGORESCUE_TEST_WHOLE_INSTANCE_URI set (scripts/test-integration-docker.sh
// starts a second server for replica set runs), these tests use that server, which no
// other test touches, and run in parallel with the rest of the suite. Without it they
// share MONGORESCUE_TEST_MONGO_URI and so run alone, before the parallel tests start.
func TestWholeInstance(t *testing.T) {
	if os.Getenv(envWholeInstanceURI) != "" {
		t.Parallel()
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"OplogSyntheticArchiveReplay", testOplogSyntheticArchiveReplay},
		{"OplogFilterReplaysIntoSafeClone", testOplogFilterReplaysIntoSafeClone},
		{"OplogWindowAndRangeReads", testOplogWindowAndRangeReads},
		{"OplogSessionPinsOneMember", testOplogSessionPinsOneMember},
		{"CanReadOplog", testCanReadOplog},
		{"PITRBaseBackupOfTheWholeInstance", testPITRBaseBackupOfTheWholeInstance},
		{"PITRCollectorSurvivesARestart", testPITRCollectorSurvivesARestart},
		{"PITRRestoreToJustBeforeADrop", testPITRRestoreToJustBeforeADrop},
		{"PITRRestoreStopsInsideATransaction", testPITRRestoreStopsInsideATransaction},
		{"PITRRestoreInPlace", testPITRRestoreInPlace},
		{"PITRChainTest", testPITRChainTest},
		{"ExactCountsAt", testExactCountsAt},
	} {
		t.Run(tc.name, tc.run)
	}
}
