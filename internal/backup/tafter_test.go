package backup

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/storage"
)

// pair is one answer of a fake OpTimeReader.
type pair struct{ last, majority pitr.OpTime }

// opTimePairs hands out the answers in order and repeats the last one.
func opTimePairs(answers ...pair) OpTimeReader {
	var mu sync.Mutex
	i := 0
	return func(context.Context, string) (pitr.OpTime, pitr.OpTime, error) {
		mu.Lock()
		defer mu.Unlock()
		a := answers[min(i, len(answers)-1)]
		i++
		return a.last, a.majority, nil
	}
}

func op(t, i uint32, term int64) pitr.OpTime {
	return pitr.OpTime{TS: pitr.Timestamp{T: t, I: i}, Term: term}
}

func TestTAfterIsMajorityCommitted(t *testing.T) {
	// Before the dump; after it the newest write (105, term 3) is not
	// majority-committed at first, then the majority passes it in term 3.
	reader := opTimePairs(
		pair{op(100, 1, 3), op(99, 1, 3)},
		pair{op(105, 1, 3), op(103, 1, 3)},
		pair{op(106, 1, 3), op(104, 1, 3)},
		pair{op(107, 1, 3), op(106, 2, 3)},
	)
	engine := NewEngine(storage.NewMockStorage(), "", WithRunner(staticRunner([]byte("archive"))),
		WithEncryptor(testEncryptor(t)), WithOpTimeReader(reader))
	rec, err := engine.Run(context.Background(), instanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	if *rec.TBefore != op(100, 1, 3) || *rec.TAfter != op(106, 2, 3) {
		t.Fatalf("T_before %v, T_after %v; want the newest write before and the majority after", rec.TBefore, rec.TAfter)
	}
}

func TestBaseFailsWhenItsWritesAreNotMajorityCommitted(t *testing.T) {
	// A primary cut off from its majority: the dump's writes may be rolled back.
	reader := opTimePairs(pair{op(100, 1, 3), op(99, 1, 3)}, pair{op(105, 1, 3), op(101, 1, 3)})
	st := storage.NewMockStorage()
	engine := NewEngine(st, "", WithRunner(staticRunner([]byte("archive"))), WithEncryptor(testEncryptor(t)), WithOpTimeReader(reader))
	engine.majorityWait = 600 * time.Millisecond
	rec, err := engine.Run(context.Background(), instanceOptions())
	if !errors.Is(err, ErrOpTime) || rec.Status != models.StatusFailed || rec.TAfter != nil {
		t.Fatalf("base without a majority: %v, %+v", err, rec)
	}
	if _, statErr := st.Stat(context.Background(), rec.StorageKey); !errors.Is(statErr, storage.ErrNotFound) {
		t.Fatalf("the archive of a base without a majority stays: %v", statErr)
	}
}
