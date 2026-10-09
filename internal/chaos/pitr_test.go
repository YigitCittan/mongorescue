//go:build chaos

package chaos

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/yigitcittan/mongorescue/internal/models"
)

type pitrTS struct {
	T uint32 `json:"t"`
	I uint32 `json:"i"`
}

type pitrChunk struct {
	ID      string `json:"id"`
	ChainID string `json:"chain_id"`
	From    pitrTS `json:"from"`
	To      pitrTS `json:"to"`
	Entries int64  `json:"entries"`
	Status  string `json:"status"`
}

type pitrStatus struct {
	Windows []struct {
		ChainID string    `json:"chain_id"`
		Open    bool      `json:"open"`
		EndTime time.Time `json:"end_time"`
	} `json:"windows"`
	ChainBreaks int `json:"chain_breaks"`
}

// writer inserts {_id: seq} documents one at a time until stopped. A write that
// fails (no primary) is retried with the same _id; a duplicate key error on a retry
// means the first attempt was applied. So every seq is written exactly once.
type writer struct {
	next atomic.Int64
	stop chan struct{}
	wg   sync.WaitGroup
}

func startWriter(t *testing.T, coll *mongo.Collection) *writer {
	w := &writer{stop: make(chan struct{})}
	w.wg.Go(func() {
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			seq := w.next.Load()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := coll.InsertOne(ctx, bson.D{{Key: "_id", Value: seq}, {Key: "at", Value: time.Now()}})
			cancel()
			if err == nil || mongo.IsDuplicateKeyError(err) {
				w.next.Add(1)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	t.Cleanup(w.halt)
	return w
}

// halt stops the writer and returns the number of documents written.
func (w *writer) halt() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	w.wg.Wait()
}

// TestPrimaryStepdownDuringPITR steps the primary down while a PITR stream collects
// the oplog (15 s chunks) and a writer inserts a document every 50 ms, retrying
// through the election.
//
// Expected: the collector carries on after the election: the stream keeps one
// chain (no chain break, no pitr.chain_broken), its chunks are contiguous (each
// starts where the previous one ended); and a point-in-time restore to the moment
// after the last write holds exactly the documents written: no gap and no
// duplicate.
func TestPrimaryStepdownDuringPITR(t *testing.T) {
	e := requireEnv(t)
	r := newRig(t, e, rigOptions{})
	var key struct {
		Identity  string `json:"identity"`
		Recipient string `json:"recipient"`
	}
	r.api.data("POST", "/api/v1/settings/encryption/generate-key", nil, http.StatusOK, &key)
	r.settings(map[string]any{"encryption": map[string]any{
		"enabled": true, "mode": "x25519", "recipients": []string{key.Recipient}, "identity": key.Identity,
	}})
	db := e.uniqueDB(t, "pitr")
	coll := e.Mongo.Database(db).Collection("events")
	if _, err := coll.InsertOne(context.Background(), bson.D{{Key: "_id", Value: int64(-1)}}); err != nil {
		t.Fatal(err)
	}

	var created struct {
		ID string `json:"id"`
	}
	r.api.data("POST", "/api/v1/pitr/streams", map[string]any{
		"connection_id": r.connID, "chunk_seconds": 15, "base_cron": "0 2 * * *",
	}, http.StatusCreated, &created)
	streamID := created.ID
	if streamID == "" {
		t.Fatal("the created stream has no ID")
	}
	t.Cleanup(func() {
		r.api.do("PATCH", "/api/v1/pitr/streams/"+streamID, map[string]any{"enabled": false})
	})
	status := func() pitrStatus {
		var s pitrStatus
		r.api.data("GET", "/api/v1/pitr/streams/"+streamID, nil, http.StatusOK, &s)
		return s
	}
	waitFor(t, 5*time.Minute, "a restorable window", func() bool { return len(status().Windows) > 0 })

	w := startWriter(t, coll)
	time.Sleep(20 * time.Second)
	e.stepDown(t, 10)
	e.waitPrimary(t)
	time.Sleep(40 * time.Second)
	w.halt()
	written := w.next.Load()
	lastWrite := time.Now().UTC()
	t.Logf("%d documents written across the stepdown", written)

	waitFor(t, 3*time.Minute, "the chunks to cover the last write", func() bool {
		s := status()
		for _, win := range s.Windows {
			if win.Open && win.EndTime.After(lastWrite.Add(time.Second)) {
				return true
			}
		}
		return false
	})
	final := status()
	if final.ChainBreaks != 0 || len(final.Windows) != 1 {
		t.Fatalf("the stepdown broke the chain: %d breaks, windows %+v", final.ChainBreaks, final.Windows)
	}
	if got := r.hook.find("pitr.chain_broken", "", ""); len(got) > 0 {
		t.Fatalf("pitr.chain_broken fired: %v", got)
	}
	var page struct {
		Chunks []pitrChunk `json:"chunks"`
	}
	r.api.data("GET", "/api/v1/pitr/streams/"+streamID+"/chunks?limit=500", nil, http.StatusOK, &page)
	// By start, then end: the first chunk of a chain holds only its start entry
	// ([From, From], read inclusively) and the next one starts there.
	less := func(a, b pitrTS) bool { return a.T < b.T || a.T == b.T && a.I < b.I }
	sort.Slice(page.Chunks, func(i, j int) bool {
		a, b := page.Chunks[i], page.Chunks[j]
		if a.From != b.From {
			return less(a.From, b.From)
		}
		return less(a.To, b.To)
	})
	for i := 1; i < len(page.Chunks); i++ {
		prev, cur := page.Chunks[i-1], page.Chunks[i]
		if cur.ChainID != prev.ChainID || cur.From != prev.To {
			t.Fatalf("chunks %s and %s are not contiguous: %+v then %+v", prev.ID, cur.ID, prev, cur)
		}
	}
	t.Logf("%d contiguous chunks in one chain", len(page.Chunks))

	var accepted models.RestoreRecord
	r.api.data("POST", "/api/v1/restore", map[string]any{
		"pitr": map[string]any{"stream_id": streamID, "at": lastWrite.Add(time.Second).Format(time.RFC3339)}, "databases": []string{db},
	}, http.StatusAccepted, &accepted)
	rst := r.waitRestore(accepted.ID)
	if rst.Status != models.RestoreStatusCompleted || rst.PITR == nil || len(rst.PITR.Clones) == 0 {
		t.Fatalf("point-in-time restore = %+v", rst)
	}
	clone := ""
	for _, c := range rst.PITR.Clones {
		if len(c) > len(db) && c[:len(db)] == db {
			clone = c
		}
	}
	if clone == "" {
		t.Fatalf("no clone of %s in %v", db, rst.PITR.Clones)
	}
	if n := e.count(t, clone, "events"); n != written+1 {
		t.Fatalf("the point-in-time clone holds %d documents; %d were written (plus the seed)", n, written)
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	var maxDoc struct {
		ID int64 `bson:"_id"`
	}
	if err := e.Mongo.Database(clone).Collection("events").FindOne(ctx, bson.D{},
		options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&maxDoc); err != nil || maxDoc.ID != written-1 {
		t.Fatalf("the newest document of the clone is %d (%v); want %d", maxDoc.ID, err, written-1)
	}
}
