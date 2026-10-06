package mongoconn

import (
	"context"
	"encoding/json"
	"errors"
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
	p := New()
	// An unreachable URI: a refusal must come before any connection attempt.
	const uri = "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=1"
	for _, db := range []string{"", "admin", "config", "local"} {
		if _, err := p.RunPostRestoreCommand(context.Background(), uri, db, json.RawMessage(`{"drop": "x"}`)); !errors.Is(err, postrestore.ErrNotClone) {
			t.Errorf("database %q: %v", db, err)
		}
	}
	if _, err := p.RunPostRestoreCommand(context.Background(), uri, "shop_rescue_x", json.RawMessage(`{"eval": "1"}`)); !errors.Is(err, postrestore.ErrInvalid) {
		t.Errorf("refused command: %v", err)
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
	err := commandError(mongo.CommandError{Code: 13, Name: "Unauthorized", Message: "not authorized on shop_rescue_x"})
	if !errors.Is(err, ErrPostRestoreCommand) || err.Error() != "mongoconn: post-restore command failed: Unauthorized (code 13): not authorized on shop_rescue_x" {
		t.Fatalf("commandError = %v", err)
	}
}
