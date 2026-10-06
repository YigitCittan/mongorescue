//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestPostRestoreCommandsReapplyErasures restores a database into a safe clone with
// a post-restore command that deletes one user by _id (a re-applied erasure): the
// clone lacks that user, every other document is there, and the source database is
// untouched. A command that fails on the server fails the restore and keeps the
// clone for inspection.
func TestPostRestoreCommandsReapplyErasures(t *testing.T) {
	env := requireMongo(t)
	st, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	db := env.uniqueDB(t, "erase")
	users := env.Client.Database(db).Collection("users")
	if _, err = users.InsertMany(ctx, []any{
		bson.D{{Key: "_id", Value: 1}, {Key: "email", Value: "ann@example.com"}},
		bson.D{{Key: "_id", Value: 2}, {Key: "email", Value: "erased@example.com"}},
		bson.D{{Key: "_id", Value: 3}, {Key: "email", Value: "bob@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	env.seed(t, db, "orders", 5)
	bkp := runBackup(t, env, st, db, true)

	prober := mongoconn.New()
	var audited []models.PostRestoreResult
	engine := newRestoreEngine(env, st, restore.WithCommandRunner(prober),
		restore.WithCommandAudit(func(_ context.Context, _ *models.RestoreRecord, res models.PostRestoreResult) {
			audited = append(audited, res)
		}))
	erasure := models.PostRestoreCommand{Database: "*",
		Command: json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": {"$in": [2]}}, "limit": 0}]}`)}
	if err = prober.CheckPostRestoreCommand(erasure.Command); err != nil {
		t.Fatal(err)
	}

	rec, err := engine.Run(ctx, models.RestoreRequest{BackupID: bkp.ID, MongoURI: env.URI,
		PostRestoreCommands: []models.PostRestoreCommand{erasure}}, bkp)
	if err != nil || rec.Status != models.RestoreStatusCompleted {
		t.Fatalf("restore: %v (%s)", err, rec.ErrorMessage)
	}
	clone := rec.TargetDatabase
	if clone == db {
		t.Fatalf("restored into the source database %s", db)
	}
	if got := env.count(t, clone, "users"); got != 2 {
		t.Fatalf("%s.users = %d documents; want 2 after the erasure", clone, got)
	}
	if n, _ := env.Client.Database(clone).Collection("users").CountDocuments(ctx, bson.D{{Key: "_id", Value: 2}}); n != 0 {
		t.Fatalf("the erased user is in the clone %s", clone)
	}
	if got := env.count(t, clone, "orders"); got != 5 {
		t.Fatalf("%s.orders = %d; want 5", clone, got)
	}
	if got := env.count(t, db, "users"); got != 3 {
		t.Fatalf("the source %s.users = %d; want it untouched (3)", db, got)
	}
	pr := rec.PostRestore
	if pr == nil || pr.Status != models.PostRestoreCompleted || len(pr.Commands) != 1 || pr.Commands[0].N != 1 || pr.Commands[0].Database != clone {
		t.Fatalf("post_restore = %+v", pr)
	}
	if len(audited) != 1 || audited[0].Command != "delete" || audited[0].N != 1 {
		t.Fatalf("audited %+v", audited)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "erased@example.com") {
		t.Fatal("the restore record holds document contents")
	}

	// A command the server refuses (collMod of a collection that does not exist)
	// fails the restore and keeps the clone.
	failing := models.PostRestoreCommand{Database: db, Command: json.RawMessage(`{"collMod": "no_such_collection", "validationLevel": "off"}`)}
	rec, runErr := engine.Run(ctx, models.RestoreRequest{BackupID: bkp.ID, MongoURI: env.URI, CloneDatabase: db + "_kept",
		PostRestoreCommands: []models.PostRestoreCommand{erasure, failing}}, bkp)
	if !errors.Is(runErr, restore.ErrPostRestoreFailed) || rec.Status != models.RestoreStatusFailed {
		t.Fatalf("failing command: %v, %s", runErr, rec.Status)
	}
	names, err := env.Client.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: rec.TargetDatabase}})
	if err != nil || !slices.Equal(names, []string{rec.TargetDatabase}) {
		t.Fatalf("the clone %s must be kept for inspection: %v, %v", rec.TargetDatabase, names, err)
	}
	if !slices.Equal(rec.PostRestore.ClonesKept, []string{rec.TargetDatabase}) || !strings.Contains(rec.ErrorMessage, "kept for inspection") {
		t.Fatalf("record = %+v, %q", rec.PostRestore, rec.ErrorMessage)
	}
	// The erasure before it did run.
	if got := env.count(t, rec.TargetDatabase, "users"); got != 2 {
		t.Fatalf("kept clone users = %d; want 2", got)
	}
	assertNoSecret(t, env.Password, "post-restore failure", rec.ErrorMessage, runErr.Error())
}
