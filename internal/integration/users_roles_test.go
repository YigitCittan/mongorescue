//go:build integration

package integration

import (
	"bytes"
	"context"
	"io"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/yigitcittan/mongorescue/internal/models"
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
