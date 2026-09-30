//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/restore"
)

// TestUsersAndRolesAreNotBackedUp documents current behaviour: a per-database backup
// does not use --dumpDbUsersAndRoles, so users and roles defined on the database are
// neither in the archive nor in the restored clone. They must be recreated (or
// backed up separately) after a disaster.
func TestUsersAndRolesAreNotBackedUp(t *testing.T) {
	env := requireMongo(t)
	st := storageTargets(t)[0].Storage
	db := env.uniqueDB(t, "usr")
	env.seed(t, db, "orders", 10)

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	const user, role = "it_app_user_marker", "it_app_role_marker"
	run := func(target string, cmd bson.D) bson.M {
		t.Helper()
		var out bson.M
		if err := env.Client.Database(target).RunCommand(ctx, cmd).Decode(&out); err != nil {
			t.Fatalf("%v on %s: %v", cmd[0].Key, target, err)
		}
		return out
	}
	run(db, bson.D{
		{Key: "createRole", Value: role},
		{Key: "privileges", Value: bson.A{bson.D{
			{Key: "resource", Value: bson.D{{Key: "db", Value: db}, {Key: "collection", Value: "orders"}}},
			{Key: "actions", Value: bson.A{"find"}},
		}}},
		{Key: "roles", Value: bson.A{}},
	})
	run(db, bson.D{
		{Key: "createUser", Value: user},
		{Key: "pwd", Value: randomHex(t, 12)},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: role}, {Key: "db", Value: db}}}},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = env.Client.Database(db).RunCommand(ctx, bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err()
		_ = env.Client.Database(db).RunCommand(ctx, bson.D{{Key: "dropAllRolesFromDatabase", Value: 1}}).Err()
	})

	bkp := mustBackup(t, env, st, models.BackupOptions{Database: db})
	rc, err := st.Retrieve(ctx, bkp.StorageKey)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(rc) // a small test archive
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archive, []byte(user)) || bytes.Contains(archive, []byte(role)) {
		t.Fatal("the archive unexpectedly contains the database's users or roles; update docs/testing.md")
	}

	rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
	count := func(target, cmd, field string) int {
		res := run(target, bson.D{{Key: cmd, Value: 1}})
		list, _ := res[field].(bson.A)
		return len(list)
	}
	if n := count(rst.TargetDatabase, "usersInfo", "users"); n != 0 {
		t.Fatalf("restored clone has %d users; users are not part of per-database backups", n)
	}
	if n := count(rst.TargetDatabase, "rolesInfo", "roles"); n != 0 {
		t.Fatalf("restored clone has %d roles; roles are not part of per-database backups", n)
	}
	if count(db, "usersInfo", "users") != 1 || count(db, "rolesInfo", "roles") != 1 {
		t.Fatal("the source's users and roles must be untouched")
	}
}

// TestRestoreWithReadWriteUser restores with a least-privilege user (readWrite on
// the source and target, no bypassDocumentValidation): plain collections restore,
// and a document a validator would reject fails the restore loudly instead of being
// dropped silently. The fidelity suite covers users with the privilege (root).
func TestRestoreWithReadWriteUser(t *testing.T) {
	env := requireMongo(t)
	if env.Password == "" {
		t.Skip("needs a server with access control (a URI with credentials)")
	}
	st := storageTargets(t)[0].Storage
	db := env.uniqueDB(t, "rw")
	target := db + "_target"
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	env.seed(t, db, "plain", 20)
	vdb := env.Client.Database(db)
	if _, err := vdb.Collection("validated").InsertOne(ctx, bson.D{{Key: "_id", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := vdb.RunCommand(ctx, bson.D{
		{Key: "collMod", Value: "validated"},
		{Key: "validator", Value: bson.D{{Key: "$jsonSchema", Value: bson.D{{Key: "required", Value: bson.A{"name"}}}}}},
		{Key: "validationLevel", Value: "moderate"},
	}).Err(); err != nil {
		t.Fatal(err)
	}

	password := randomHex(t, 12)
	if err := vdb.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "it_rw"},
		{Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{
			bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: db}},
			bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: target}},
		}},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = env.Client.Database(db).RunCommand(ctx, bson.D{{Key: "dropUser", Value: "it_rw"}}).Err()
	})
	u, err := url.Parse(env.URI)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("it_rw", password)
	q := u.Query()
	q.Set("authSource", db)
	u.RawQuery = q.Encode()
	rw := &mongoEnv{URI: u.String(), Password: password, Client: env.Client}

	no := false
	into := models.RestoreRequest{SafeClone: &no, ConfirmInPlace: true, DropTarget: true, TargetDatabase: target}

	plain := mustBackup(t, rw, st, models.BackupOptions{Database: db, Collections: []string{"plain"}})
	mustRestore(t, rw, st, into, plain)
	if n := env.count(t, target, "plain"); n != 20 {
		t.Fatalf("restored %d documents with a readWrite user; want 20", n)
	}

	full := mustBackup(t, rw, st, models.BackupOptions{Database: db})
	rec, err := tryRestore(t, rw, st, into, full)
	if !errors.Is(err, restore.ErrDocumentsFailed) || rec.Status != models.RestoreStatusFailed ||
		!strings.Contains(rec.ErrorMessage, "bypassDocumentValidation") {
		t.Fatalf("a rejected document must fail the restore with a hint; got %v (%+v)", err, rec)
	}
}
