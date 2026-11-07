package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestBackupRefusesAMongosUnlessAllowed uses the member of a hello reply of a
// mongos (msg: isdbgrid, classified as models.MemberMongos by the prober): the
// backup is refused before mongodump starts, unless the connection sets
// allow_mongos, which records the sharded_unsupported warning.
func TestBackupRefusesAMongosUnlessAllowed(t *testing.T) {
	mongos := models.SourceMember{Host: "router:27017", State: models.MemberMongos}
	probe := func(context.Context, string) (models.SourceMember, error) { return mongos, nil }

	t.Run("refused by default", func(t *testing.T) {
		r := &configRunner{}
		engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run), WithMemberProbe(probe))
		rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://router"})
		if !errors.Is(err, models.ErrShardedUnsupported) || rec.Status != models.StatusFailed {
			t.Fatalf("err %v, status %s", err, rec.Status)
		}
		if !strings.Contains(rec.ErrorMessage, "allow_mongos") || r.args != nil {
			t.Fatalf("error %q, mongodump started %v", rec.ErrorMessage, r.args != nil)
		}
	})

	t.Run("warns with the opt-in", func(t *testing.T) {
		r := &configRunner{}
		engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run), WithMemberProbe(probe))
		rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://router", AllowMongos: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(rec.Warnings) != 1 || !models.IsShardedWarning(rec.Warnings[0]) {
			t.Fatalf("warnings %q", rec.Warnings)
		}
		if rec.SourceMember == nil || rec.SourceMember.State != models.MemberMongos {
			t.Fatalf("member %+v", rec.SourceMember)
		}
	})

	t.Run("a failed probe fails closed", func(t *testing.T) {
		r := &configRunner{}
		engine := NewEngine(storage.NewMockStorage(), "", WithRunner(r.run),
			WithMemberProbe(func(context.Context, string) (models.SourceMember, error) {
				return models.SourceMember{}, errors.New("boom")
			}))
		rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://router"})
		if !errors.Is(err, models.ErrShardedUnsupported) || rec.Status != models.StatusFailed || r.args != nil {
			t.Fatalf("err %v, status %s, mongodump started %v", err, rec.Status, r.args != nil)
		}
	})

	t.Run("a replica set member passes without a warning", func(t *testing.T) {
		engine := NewEngine(storage.NewMockStorage(), "", WithRunner((&configRunner{}).run),
			WithMemberProbe(func(context.Context, string) (models.SourceMember, error) {
				return models.SourceMember{Host: "db1:27017", State: models.MemberPrimary, SetName: "rs0"}, nil
			}))
		rec, err := engine.Run(context.Background(), models.BackupOptions{Database: "shop", MongoURI: "mongodb://db1"})
		if err != nil || len(rec.Warnings) != 0 {
			t.Fatalf("err %v, warnings %q", err, rec.Warnings)
		}
	})
}
