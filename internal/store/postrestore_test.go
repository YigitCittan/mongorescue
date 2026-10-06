package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/secretbox"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// erasureMarker is an identifier of an erased person in a post-restore command.
const erasureMarker = "erased-subject-7f3a9c"

func erasureConnection(id string) *models.Connection {
	now := time.Now().UTC()
	return &models.Connection{ID: id, Name: id, URI: "mongodb://h/", CreatedAt: now, UpdatedAt: now,
		PostRestoreCommands: []models.PostRestoreCommand{{Database: "*",
			Command: json.RawMessage(`{"delete":"users","deletes":[{"q":{"customer":"` + erasureMarker + `"},"limit":0}]}`)}}}
}

func TestPostRestoreCommandsAreSealedAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	want := erasureConnection("conn_a")
	if err := s.SaveConnection(ctx, want); err != nil {
		t.Fatal(err)
	}
	// Read back through the store: the commands come back in plain form.
	for _, get := range []func() (*models.Connection, error){
		func() (*models.Connection, error) { return s.GetConnection(ctx, "conn_a") },
		func() (*models.Connection, error) {
			list, err := s.ListConnections(ctx)
			if err != nil || len(list) != 1 {
				return nil, err
			}
			return list[0], nil
		},
	} {
		got, err := get()
		if err != nil || got == nil || len(got.PostRestoreCommands) != 1 || string(got.PostRestoreCommands[0].Command) != string(want.PostRestoreCommands[0].Command) {
			t.Fatalf("read back %+v, %v", got, err)
		}
	}

	// The data column holds only the sealed value.
	raw := rawData(t, path, "connections")
	if strings.Contains(raw, erasureMarker) || strings.Contains(raw, "post_restore_commands") || !strings.Contains(raw, `"post_restore_sealed":"`+secretbox.Prefix) {
		t.Fatalf("stored connection = %s", raw)
	}
	// Nor does any byte of the database file (or its write-ahead log) hold it.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(erasureMarker)) {
			t.Fatalf("%s holds a post-restore command in plain form", filepath.Base(f))
		}
	}
}

// TestPostRestoreCommandsSurviveAKeyRotation rotates secret.key and restarts with
// the new key: the commands of a connection, and those a pending approval holds,
// are re-sealed and open again; nothing is skipped and nothing is left in plain form.
func TestPostRestoreCommandsSurviveAKeyRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	oldBox := storetest.NewBox(t)
	s := storetest.OpenWithBox(t, path, oldBox)
	ctx := context.Background()
	want := erasureConnection("conn_a")
	if err := s.SaveConnection(ctx, want); err != nil {
		t.Fatal(err)
	}
	held := `[{"database":"*","command":{"drop":"` + erasureMarker + `"}}]`
	now := time.Now().UTC()
	if err := s.CreateApproval(ctx, &models.Approval{ID: "apr_1", Action: models.ApprovalPostRestoreCommands, Status: models.ApprovalPending,
		Subject: "conn_a", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Secret: held}); err != nil {
		t.Fatal(err)
	}
	before := rawData(t, path, "connections")
	var approvalBefore string
	if err := rawDB(t, path).QueryRow("SELECT secret FROM approvals WHERE id = 'apr_1'").Scan(&approvalBefore); err != nil {
		t.Fatal(err)
	}

	next := storetest.NewBox(t)
	res, err := s.RotateSecretBox(ctx, store.SecretKeyRotation{Next: next})
	if err != nil || len(res.Skipped) != 0 {
		t.Fatalf("rotation = %+v, %v; want nothing skipped", res, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart with the new key.
	s = storetest.OpenWithBox(t, path, next)
	got, err := s.GetConnection(ctx, "conn_a")
	if err != nil || len(got.PostRestoreCommands) != 1 || string(got.PostRestoreCommands[0].Command) != string(want.PostRestoreCommands[0].Command) {
		t.Fatalf("after the rotation = %+v, %v", got, err)
	}
	if secret, secretErr := s.ApprovalSecret(ctx, "apr_1"); secretErr != nil || secret != held {
		t.Fatalf("approval secret after the rotation = %q, %v", secret, secretErr)
	}
	after := rawData(t, path, "connections")
	var approvalAfter string
	if err = rawDB(t, path).QueryRow("SELECT secret FROM approvals WHERE id = 'apr_1'").Scan(&approvalAfter); err != nil {
		t.Fatal(err)
	}
	if after == before || approvalAfter == approvalBefore || strings.Contains(after+approvalAfter, erasureMarker) {
		t.Fatal("the post-restore commands were not re-sealed, or are stored in plain form")
	}
}

func TestPostRestoreCommandsAreBoundToTheirConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), dbFile)
	s := storetest.OpenWithBox(t, path, testBox)
	ctx := context.Background()
	for _, id := range []string{"conn_a", "conn_b"} {
		c := erasureConnection(id)
		if id == "conn_b" {
			c.PostRestoreCommands = nil
		}
		if err := s.SaveConnection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	// A sealed value copied to another connection does not open there.
	if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.post_restore_sealed',
		(SELECT json_extract(data, '$.post_restore_sealed') FROM connections WHERE id = 'conn_a')) WHERE id = 'conn_b'`); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetConnection(ctx, "conn_b"); err == nil {
		t.Fatalf("a copied sealed value opened: %+v", c)
	}
	// Plain commands planted in the data column are refused.
	if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_set(data, '$.post_restore_commands',
		json('[{"database":"*","command":{"drop":"users"}}]')) WHERE id = 'conn_a'`); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetConnection(ctx, "conn_a"); !errors.Is(err, store.ErrUnsealedSecret) {
		t.Fatalf("planted plain commands = %+v, %v; want ErrUnsealedSecret", c, err)
	}
	// A connection without commands stores no sealed field.
	if _, err := rawDB(t, path).Exec(`UPDATE connections SET data = json_remove(data, '$.post_restore_sealed') WHERE id = 'conn_b'`); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetConnection(ctx, "conn_b"); err != nil || c.PostRestoreCommands != nil {
		t.Fatalf("connection without commands = %+v, %v", c, err)
	}
}
