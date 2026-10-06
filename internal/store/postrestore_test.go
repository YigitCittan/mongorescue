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
