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
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/restore"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

const itUser, itRole = "it_app_user_marker", "it_app_role_marker"

// usersRolesDB seeds a database with a role and a user that has it, dropped again on
// cleanup. run executes a command on a database; count counts the users (usersInfo,
// "users") or roles (rolesInfo, "roles") of one.
func usersRolesDB(t *testing.T, env *mongoEnv) (db string, run func(target string, cmd bson.D) bson.M, count func(target, cmd, field string) int) {
	t.Helper()
	db = env.uniqueDB(t, "usr")
	env.seed(t, db, "orders", 10)
	run = func(target string, cmd bson.D) bson.M {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		var out bson.M
		if err := env.Client.Database(target).RunCommand(ctx, cmd).Decode(&out); err != nil {
			t.Fatalf("%v on %s: %v", cmd[0].Key, target, err)
		}
		return out
	}
	count = func(target, cmd, field string) int {
		t.Helper()
		res := run(target, bson.D{{Key: cmd, Value: 1}})
		list, _ := res[field].(bson.A)
		return len(list)
	}
	run(db, bson.D{
		{Key: "createRole", Value: itRole},
		{Key: "privileges", Value: bson.A{bson.D{
			{Key: "resource", Value: bson.D{{Key: "db", Value: db}, {Key: "collection", Value: "orders"}}},
			{Key: "actions", Value: bson.A{"find"}},
		}}},
		{Key: "roles", Value: bson.A{}},
	})
	run(db, bson.D{
		{Key: "createUser", Value: itUser},
		{Key: "pwd", Value: randomHex(t, 12)},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: itRole}, {Key: "db", Value: db}}}},
	})
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = env.Client.Database(db).RunCommand(cctx, bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}}).Err()
		_ = env.Client.Database(db).RunCommand(cctx, bson.D{{Key: "dropAllRolesFromDatabase", Value: 1}}).Err()
	})
	return db, run, count
}

// archiveBytes reads a small test archive.
func archiveBytes(t *testing.T, st storage.Storage, key string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	rc, err := st.Retrieve(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

// TestUsersAndRolesAreNotBackedUp pins the default: a per-database backup without
// include_users_and_roles does not use --dumpDbUsersAndRoles, so users and roles
// defined on the database are neither in the archive nor in the restored clone.
func TestUsersAndRolesAreNotBackedUp(t *testing.T) {
	env := requireMongo(t)
	st := storageTargets(t)[0].Storage
	db, _, count := usersRolesDB(t, env)

	bkp := mustBackup(t, env, st, models.BackupOptions{Database: db})
	if bkp.UsersAndRoles {
		t.Fatal("a backup without include_users_and_roles is recorded with users and roles")
	}
	archive := archiveBytes(t, st, bkp.StorageKey)
	if bytes.Contains(archive, []byte(itUser)) || bytes.Contains(archive, []byte(itRole)) {
		t.Fatal("the archive unexpectedly contains the database's users or roles; update docs/testing.md")
	}

	rst := mustRestore(t, env, st, models.RestoreRequest{}, bkp)
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

// TestUsersAndRolesSurviveInPlaceRestore backs up a database with
// include_users_and_roles, drops its users and roles, and restores them in place
// with restore_users_and_roles. A safe clone of the same backup refuses the option.
func TestUsersAndRolesSurviveInPlaceRestore(t *testing.T) {
	env := requireMongo(t)
	st := storageTargets(t)[0].Storage
	db, run, count := usersRolesDB(t, env)

	bkp := mustBackup(t, env, st, models.BackupOptions{Database: db, IncludeUsersAndRoles: true, Gzip: true})
	if !bkp.UsersAndRoles {
		t.Fatal("the backup is not recorded with users and roles")
	}

	if _, err := tryRestore(t, env, st, models.RestoreRequest{RestoreUsersAndRoles: true}, bkp); !errors.Is(err, models.ErrUsersAndRolesNotAllowed) {
		t.Fatalf("safe clone with restore_users_and_roles = %v; want ErrUsersAndRolesNotAllowed", err)
	}

	// The disaster: the database loses its users and roles.
	run(db, bson.D{{Key: "dropAllUsersFromDatabase", Value: 1}})
	run(db, bson.D{{Key: "dropAllRolesFromDatabase", Value: 1}})
	if count(db, "usersInfo", "users") != 0 || count(db, "rolesInfo", "roles") != 0 {
		t.Fatal("users and roles were not dropped")
	}

	no := false
	rst := mustRestore(t, env, st, models.RestoreRequest{
		SafeClone: &no, ConfirmInPlace: true, DropTarget: true, RestoreUsersAndRoles: true,
	}, bkp)
	if !rst.UsersAndRoles || rst.TargetDatabase != db {
		t.Fatalf("restore record = %+v", rst)
	}
	users := run(db, bson.D{{Key: "usersInfo", Value: itUser}})
	if list, _ := users["users"].(bson.A); len(list) != 1 {
		t.Fatalf("user %s not restored: %v", itUser, users)
	}
	if n := count(db, "rolesInfo", "roles"); n != 1 {
		t.Fatalf("restored database has %d roles; want 1", n)
	}
	if n := env.count(t, db, "orders"); n != 10 {
		t.Fatalf("restored %d documents; want 10", n)
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
		cctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = env.Client.Database(db).RunCommand(cctx, bson.D{{Key: "dropUser", Value: "it_rw"}}).Err()
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

	// Several collections: the database is listed with the readWrite user.
	multi := mustBackup(t, rw, st, models.BackupOptions{Database: db, Collections: []string{"plain", "validated"}})
	if multi.Status != models.StatusCompleted {
		t.Fatalf("multi-collection backup with a readWrite user: %s", multi.ErrorMessage)
	}

	// A user that may only read one collection (no listCollections privilege) can
	// still list what it is authorized for (nameOnly + authorizedCollections).
	if err = vdb.RunCommand(ctx, bson.D{
		{Key: "createRole", Value: "it_find_plain"},
		{Key: "privileges", Value: bson.A{bson.D{
			{Key: "resource", Value: bson.D{{Key: "db", Value: db}, {Key: "collection", Value: "plain"}}},
			{Key: "actions", Value: bson.A{"find"}},
		}}},
		{Key: "roles", Value: bson.A{}},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if err = vdb.RunCommand(ctx, bson.D{
		{Key: "createUser", Value: "it_ro"},
		{Key: "pwd", Value: password},
		{Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "it_find_plain"}, {Key: "db", Value: db}}}},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = env.Client.Database(db).RunCommand(cctx, bson.D{{Key: "dropUser", Value: "it_ro"}}).Err()
		_ = env.Client.Database(db).RunCommand(cctx, bson.D{{Key: "dropRole", Value: "it_find_plain"}}).Err()
	})
	u.User = url.UserPassword("it_ro", password)
	cols, err := mongoconn.New().ListCollections(ctx, u.String(), db)
	if err != nil || len(cols) != 1 || cols[0].Name != "plain" {
		t.Fatalf("a find-only user must list its collection: %v %+v", err, cols)
	}

	full := mustBackup(t, rw, st, models.BackupOptions{Database: db})
	rec, err := tryRestore(t, rw, st, into, full)
	if !errors.Is(err, restore.ErrDocumentsFailed) || rec.Status != models.RestoreStatusFailed ||
		!strings.Contains(rec.ErrorMessage, "bypassDocumentValidation") {
		t.Fatalf("a rejected document must fail the restore with a hint; got %v (%+v)", err, rec)
	}
}
