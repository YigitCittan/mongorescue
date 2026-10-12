//go:build integration

package integration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/mongoconn"
	"github.com/yigitcittan/mongorescue/internal/operations"
	"github.com/yigitcittan/mongorescue/internal/pitr"
)

// TestPITRRestoreInPlace writes data, takes a base, writes more, then drops a
// collection, updates documents and creates a new collection, and restores the
// database in place to just before the drop (issue #142). The original database
// then equals its state before the destructive writes, the collection created
// afterwards is gone, the safety backup holds the state after them, no clone is
// left behind, and the stream collects again in a new chain whose base was taken
// after the restore.
func TestPITRRestoreInPlace(t *testing.T) {
	r := newPITRRig(t)
	env, ctx := r.env, r.ctx
	db := env.uniqueDB(t, "pitrip")
	env.seed(t, db, "orders", 40)
	env.seed(t, db, "items", 25)
	r.base(t)

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	d := env.Client.Database(db)
	_, err := d.Collection("orders").InsertMany(ctx, []any{bson.D{{Key: "seq", Value: 100}}, bson.D{{Key: "seq", Value: 101}}})
	must("insert", err)
	_, err = d.Collection("orders").Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "seq", Value: -1}}})
	must("index build", err)
	want := env.state(t, db)
	at := r.nextSecond(t)
	oldChain := r.openChain(t)

	// The disaster: a dropped collection, overwritten documents, and a
	// collection the restored state never had.
	must("drop", d.Collection("items").Drop(ctx))
	if drop := env.lastOplogTS(t); drop.T <= at {
		t.Fatalf("the drop at %d.%d is in the restored second %d (clock skew)", drop.T, drop.I, at)
	}
	_, err = d.Collection("orders").UpdateMany(ctx, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "broken", Value: true}}}})
	must("update", err)
	_, err = d.Collection("extra").InsertOne(ctx, bson.D{{Key: "after", Value: true}})
	must("new collection", err)
	after := env.state(t, db)
	r.waitCovered(t)
	system := r.systemState(t)

	target := time.Unix(int64(at), 0).UTC()
	req := models.RestoreRequest{PITR: &models.PITRTarget{StreamID: r.stream.ID, At: &target, InPlace: true, ConfirmInPlace: db},
		Databases: []string{db}}
	// An unconfirmed request is refused before anything runs.
	unconfirmed := req
	unconfirmedTarget := *req.PITR
	unconfirmedTarget.ConfirmInPlace = "other"
	unconfirmed.PITR = &unconfirmedTarget
	if _, err = r.ops.StartRestore(auth.WithPrincipal(ctx, auth.SystemPrincipal()), unconfirmed); !errors.Is(err, models.ErrPITRInPlaceNotConfirmed) {
		t.Fatalf("unconfirmed: %v", err)
	}
	restoreStart := time.Now().UTC()
	rec := r.restore(t, req)
	ip := rec.PITR.InPlace
	if ip == nil || ip.Stage != models.PITRInPlaceDone || !slices.Equal(ip.Swapped, []string{db}) || !rec.InPlace {
		t.Fatalf("in place = %+v", ip)
	}

	// The original holds the state before the disaster; the collection created
	// afterwards is gone; admin and config are untouched.
	assertSameState(t, "original after the in-place restore", env.state(t, db), want)
	names, err := d.ListCollectionNames(ctx, bson.D{})
	must("list collections", err)
	if slices.Contains(names, "extra") {
		t.Error("the collection created after the target time survived")
	}
	if got := r.systemState(t); got != system {
		t.Errorf("admin or config changed: %s, was %s", got, system)
	}
	// No clone is left behind.
	for _, name := range r.databases(t) {
		if strings.HasSuffix(name, rec.PITR.CloneSuffix) {
			t.Errorf("clone %s was left behind", name)
		}
	}

	// The safety backup holds the state after the disaster.
	if len(ip.SafetyBackupIDs) != 1 {
		t.Fatalf("safety backups %v", ip.SafetyBackupIDs)
	}
	safety, err := r.repo.GetBackupRecord(ctx, ip.SafetyBackupIDs[0])
	must("safety backup", err)
	if safety.Status != models.StatusCompleted || safety.Label != models.BackupLabelPrePITRInPlace || safety.RestoreID != rec.ID ||
		safety.Database != db {
		t.Fatalf("safety backup %+v", safety)
	}
	clone := r.restoreBackup(t, safety.ID)
	assertSameState(t, "restore of the safety backup", env.state(t, clone), after)

	// The stream collects again in a new chain, with a base taken after the
	// restore.
	st, err := r.repo.GetStream(ctx, r.stream.ID)
	must("stream", err)
	if !st.Enabled || !ip.StreamWasEnabled || !ip.StreamPaused || !ip.StreamResumed {
		t.Fatalf("stream enabled %v, in place %+v", st.Enabled, ip)
	}
	chains, err := r.repo.ListChains(ctx, r.stream.ID)
	must("chains", err)
	var newChain *pitr.Chain
	for _, c := range chains {
		switch {
		case c.ChainID == oldChain && (c.Open() || c.EndReason != pitr.EndStopped):
			t.Errorf("the chain before the restore: open %v, reason %q; want ended as stopped", c.Open(), c.EndReason)
		case c.Open():
			newChain = c
		}
	}
	if newChain == nil || newChain.ChainID == oldChain {
		t.Fatalf("no new open chain: %+v", chains)
	}
	if ip.NewBaseID == "" {
		t.Fatalf("no base backup of the new chain (warning %q)", rec.Warning)
	}
	base := r.waitBackup(t, ip.NewBaseID)
	if base.StartedAt.Before(restoreStart) || base.TBefore == nil || base.TBefore.TS.Compare(newChain.Start) < 0 {
		t.Fatalf("base %s started %s (t_before %v), restore started %s, new chain starts %s",
			base.ID, base.StartedAt, base.TBefore, restoreStart, newChain.Start)
	}
}

// TestCollectionCountsCountsTimeSeries checks that the listing the swap of an
// in-place restore compares counts a time-series collection's measurements (it
// is no view), and leaves only real views uncounted.
func TestCollectionCountsCountsTimeSeries(t *testing.T) {
	env := requireMongo(t)
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	db := env.uniqueDB(t, "tscount")
	d := env.Client.Database(db)
	if err := d.CreateCollection(ctx, "metrics", options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("ts"))); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.D{{Key: "ts", Value: now.Add(time.Duration(i) * time.Second)}, {Key: "v", Value: i}})
	}
	if _, err := d.Collection("metrics").InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateView(ctx, "recent", "metrics", mongo.Pipeline{}); err != nil {
		t.Fatal(err)
	}
	counts, err := mongoconn.New().CollectionCounts(ctx, env.URI, db)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]models.CollectionCount{}
	for _, c := range counts {
		got[c.Name] = c
	}
	if c, ok := got["metrics"]; !ok || c.View || c.Documents != 5 {
		t.Fatalf("metrics = %+v (%v); want a counted collection of 5", c, counts)
	}
	if c, ok := got["recent"]; !ok || !c.View {
		t.Fatalf("recent = %+v; want a view", c)
	}
}

// openChain returns the ID of the stream's open chain.
func (r *pitrRig) openChain(t *testing.T) string {
	t.Helper()
	chains, err := r.repo.ListChains(r.ctx, r.stream.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chains {
		if c.Open() {
			return c.ChainID
		}
	}
	t.Fatal("the stream has no open chain")
	return ""
}

// restoreBackup restores backup id into a safe clone through the operations
// service, waits for it, drops the clone on cleanup and returns its name.
func (r *pitrRig) restoreBackup(t *testing.T, id string) string {
	t.Helper()
	rec, err := r.ops.StartRestore(auth.WithPrincipal(r.ctx, auth.SystemPrincipal()), models.RestoreRequest{BackupID: id})
	var pre *operations.PreflightError
	if errors.As(err, &pre) {
		t.Fatalf("preflight refused the restore: %+v", pre.Result.Checks)
	}
	if err != nil {
		t.Fatalf("restore backup %s: %v", id, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		_ = r.env.Client.Database(rec.TargetDatabase).Drop(ctx)
	})
	deadline := time.Now().Add(5 * time.Minute)
	for {
		got, getErr := r.repo.GetRestoreRecord(r.ctx, rec.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != models.RestoreStatusInProgress && got.Status != models.RestoreStatusPending {
			if got.Status != models.RestoreStatusCompleted {
				t.Fatalf("restore of backup %s %s: %s", id, got.Status, got.ErrorMessage)
			}
			return got.TargetDatabase
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore did not finish")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitBackup waits until backup id completed.
func (r *pitrRig) waitBackup(t *testing.T, id string) *models.BackupRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		rec, err := r.repo.GetBackupRecord(r.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch rec.Status {
		case models.StatusCompleted:
			return rec
		case models.StatusInProgress, models.StatusPending:
		default:
			t.Fatalf("backup %s %s: %s", id, rec.Status, rec.ErrorMessage)
		}
		if time.Now().After(deadline) {
			t.Fatalf("backup %s did not finish", id)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
