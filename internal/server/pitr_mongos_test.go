package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// TestPITRStreamOnAMongosIsRefused covers issue #222: the hello reply of a mongos
// has no setName, so the oplog reads answer pitr.ErrNotReplicaSet (or a window
// without a replica set) and no stream is created on it.
func TestPITRStreamOnAMongosIsRefused(t *testing.T) {
	f := newPITRFixture(t)
	const body = `{"connection_id":"conn_a","enabled":false}`

	f.inspErr = pitr.ErrNotReplicaSet
	if code, msg := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", body); code != http.StatusBadRequest || !strings.Contains(msg, "replica set") {
		t.Fatalf("mongos (hello without setName): %d %s", code, msg)
	}

	f.inspErr = nil
	f.inspect.Window.ReplicaSet = ""
	if code, msg := f.do(auth.ScopeAdmin, "POST", "/api/v1/pitr/streams", body); code != http.StatusBadRequest || !strings.Contains(msg, "replica set") {
		t.Fatalf("mongos (window without a replica set): %d %s", code, msg)
	}
}
