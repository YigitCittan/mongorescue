package mongoconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/postrestore"
)

func TestCheckPostRestoreCommand(t *testing.T) {
	p := New()
	if err := p.CheckPostRestoreCommand(json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": {"$oid": "65f0a1b2c3d4e5f601234567"}}, "limit": 0}]}`)); err != nil {
		t.Fatalf("valid command: %v", err)
	}
	if err := p.CheckPostRestoreCommand(json.RawMessage(`{"delete": "users", "deletes": [{"q": {"_id": {"$oid": "nope"}}, "limit": 0}]}`)); !errors.Is(err, postrestore.ErrInvalid) {
		t.Fatalf("bad extended JSON: %v", err)
	}
	if err := p.CheckPostRestoreCommand(json.RawMessage(`{"applyOps": []}`)); !errors.Is(err, postrestore.ErrInvalid) {
		t.Fatalf("refused command: %v", err)
	}
}

func TestCommandDocumentKeepsKeyOrder(t *testing.T) {
	doc, err := commandDocument(json.RawMessage(`{"update": "users", "updates": [], "ordered": false}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc) != 3 || doc[0].Key != "update" || doc[1].Key != "updates" || doc[2].Key != "ordered" {
		t.Fatalf("doc = %v", doc)
	}
}

func TestRunPostRestoreCommandRefusesBeforeConnecting(t *testing.T) {
	// An unreachable URI: the client connects lazily, and a refusal must come
	// before any connection attempt.
	s, err := New().OpenCommandSession(context.Background(), "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, db := range []string{"", "admin", "config", "local"} {
		if _, err := s.RunPostRestoreCommand(context.Background(), db, json.RawMessage(`{"drop": "x"}`)); !errors.Is(err, postrestore.ErrNotClone) {
			t.Errorf("database %q: %v", db, err)
		}
	}
	if _, err := s.RunPostRestoreCommand(context.Background(), "shop_rescue_x", json.RawMessage(`{"eval": "1"}`)); !errors.Is(err, postrestore.ErrInvalid) {
		t.Errorf("refused command: %v", err)
	}
	if _, err := New().OpenCommandSession(context.Background(), "mongo://u:pw@host"); err == nil || strings.Contains(err.Error(), "pw") {
		t.Errorf("invalid URI: %v", err)
	}
}

func TestCommandErrorsNeverQuoteServerMessages(t *testing.T) {
	ctx := context.Background()
	dup := mongo.CommandError{Code: 11000, Name: "DuplicateKey",
		Message: `E11000 duplicate key error collection: shop_rescue_x.users index: email_1 dup key: { email: "ann@example.com" }`}
	invalid := mongo.CommandError{Code: 121, Name: "DocumentValidationFailure", Message: `Document failed validation: {"ssn": "123-45-6789"}`}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{dup, "mongoconn: post-restore command failed: DuplicateKey (code 11000)"},
		{fmt.Errorf("run: %w", invalid), "mongoconn: post-restore command failed: DocumentValidationFailure (code 121)"},
		{mongo.CommandError{Code: 2, Message: "bad value ann@example.com"}, "mongoconn: post-restore command failed: error (code 2)"},
		{errors.New("server selection error: ann@example.com"), "mongoconn: post-restore command failed: driver error (the server could not be reached or selected)"},
	} {
		got := commandError(ctx, tc.err)
		if !errors.Is(got, ErrPostRestoreCommand) || got.Error() != tc.want {
			t.Errorf("commandError(%v) = %q; want %q", tc.err, got, tc.want)
		}
		for _, value := range []string{"ann@example.com", "123-45-6789", "email_1", "E11000"} {
			if strings.Contains(got.Error(), value) {
				t.Errorf("commandError quotes %q: %q", value, got)
			}
		}
	}
	expired, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	if got := commandError(expired, context.DeadlineExceeded); got.Error() != "mongoconn: post-restore command failed: timed out" {
		t.Errorf("timeout = %q", got)
	}
}

func TestReplyCounts(t *testing.T) {
	r := &commandReply{N: 4, NModified: 3, Upserted: []bson.Raw{nil}}
	if got := replyCounts("update", r); got != (models.PostRestoreCounts{N: 4, Modified: 3, Upserted: 1}) {
		t.Errorf("update = %+v", got)
	}
	if got := replyCounts("delete", r); got != (models.PostRestoreCounts{N: 4}) {
		t.Errorf("delete = %+v", got)
	}
	if got := replyCounts("drop", r); got != (models.PostRestoreCounts{}) {
		t.Errorf("drop = %+v", got)
	}
	fam := &commandReply{}
	if got := replyCounts("findAndModify", fam); got != (models.PostRestoreCounts{}) {
		t.Errorf("findAndModify without lastErrorObject = %+v", got)
	}
	fam.LastErrorObject = &struct {
		N        int64         `bson:"n"`
		Upserted bson.RawValue `bson:"upserted"`
	}{N: 1}
	if got := replyCounts("findAndModify", fam); got != (models.PostRestoreCounts{N: 1}) {
		t.Errorf("findAndModify = %+v", got)
	}
}

func TestDroppingWhatIsNotThereSucceeds(t *testing.T) {
	missingNS := mongo.CommandError{Code: namespaceNotFoundCode, Name: "NamespaceNotFound"}
	missingIndex := mongo.CommandError{Code: indexNotFoundCode, Name: "IndexNotFound"}
	switch {
	case !notThere("drop", missingNS), !notThere("dropIndexes", missingNS), !notThere("dropIndexes", missingIndex):
		t.Fatal("dropping a missing collection or index must succeed")
	case notThere("delete", missingNS), notThere("drop", missingIndex), notThere("drop", errors.New("x")):
		t.Fatal("other failures must fail")
	}
}
