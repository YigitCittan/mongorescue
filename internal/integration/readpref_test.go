//go:build integration

package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/backup"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// TestBackupReadPreferenceOnAReplicaSet runs backups with a read preference
// against the replica set member of scripts/test-integration-docker.sh
// (MONGO_TOPOLOGY=replset). That replica set has a single member, the primary,
// reached with directConnection=true, so there is no secondary: secondaryPreferred
// must back up from the primary and record it, and secondary must fail with
// backup.ErrNoEligibleMember before mongodump starts instead of silently reading
// from the primary. That a secondary job really reads from a secondary is left to
// a three-member replica set (the nightly integration job, once it exists), where
// the recorded source member must then be in state "secondary".
func TestBackupReadPreferenceOnAReplicaSet(t *testing.T) {
	env := requireMongo(t)
	requireReplicaSet(t, env)
	db := env.uniqueDB(t, "readpref")
	env.seed(t, db, "orders", 25)

	prober := mongoconn.New()
	st := storage.NewMockStorage()
	engine := newBackupEngine(env, st, backup.WithMemberProbe(prober.ServingMember), backup.WithManifestCapturer(prober.Manifest))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	rec, err := engine.Run(ctx, models.BackupOptions{Database: db, Gzip: true,
		ReadPreference: models.ReadPreference{Mode: models.ReadSecondaryPreferred}, NumParallelCollections: 1, MaxUploadMbps: 100})
	if err != nil {
		t.Fatalf("secondaryPreferred backup: %v", err)
	}
	if rec.Status != models.StatusCompleted || rec.SizeBytes == 0 {
		t.Fatalf("secondaryPreferred backup %s, %d bytes", rec.Status, rec.SizeBytes)
	}
	if rec.SourceMember == nil || rec.SourceMember.State != models.MemberPrimary || rec.SourceMember.SetName == "" || rec.SourceMember.Host == "" {
		t.Fatalf("source member %+v, want the primary of the replica set", rec.SourceMember)
	}
	if rec.ReadPreference != models.ReadSecondaryPreferred {
		t.Fatalf("recorded read preference %q", rec.ReadPreference)
	}
	if !rec.HasManifest || len(rec.Manifest.Collections) != 1 || rec.Manifest.Collections[0].DocumentsMax != 25 {
		t.Fatalf("manifest under the read preference: %+v", rec.Manifest)
	}

	rec, err = engine.Run(ctx, models.BackupOptions{Database: db, Gzip: true,
		ReadPreference: models.ReadPreference{Mode: models.ReadSecondary}})
	if !errors.Is(err, backup.ErrNoEligibleMember) {
		t.Fatalf("secondary backup without a secondary: %v, want ErrNoEligibleMember", err)
	}
	if rec.Status != models.StatusFailed || !strings.Contains(rec.ErrorMessage, "read preference secondary needs a replica set secondary") {
		t.Fatalf("secondary backup %s: %q", rec.Status, rec.ErrorMessage)
	}
	assertNoSecret(t, env.Password, "error message", rec.ErrorMessage)
}
