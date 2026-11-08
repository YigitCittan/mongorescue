package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yigitcittan/mongorescue/internal/auth"
	"github.com/yigitcittan/mongorescue/internal/models"
	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/store"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

// TestConstraintViolationsMapToSentinels checks that every unique or primary key
// violation of an insert reaches the caller as its sentinel, read from the driver's
// result code.
func TestConstraintViolationsMapToSentinels(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	t.Run("user id", func(t *testing.T) {
		mustCreate(t, st, localUser("usr_a", "alice", auth.RoleAdmin))
		if err := st.CreateUser(ctx, localUser("usr_a", "other", auth.RoleViewer)); !errors.Is(err, auth.ErrUserExists) {
			t.Fatalf("duplicate id = %v; want ErrUserExists", err)
		}
	})
	t.Run("username", func(t *testing.T) {
		if err := st.CreateUser(ctx, localUser("usr_b", "alice", auth.RoleViewer)); !errors.Is(err, auth.ErrUserExists) {
			t.Fatalf("duplicate username = %v; want ErrUserExists", err)
		}
	})
	t.Run("stream id and connection", func(t *testing.T) {
		if err := st.CreateStream(ctx, newStream("pst_a", "conn_a")); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateStream(ctx, newStream("pst_a", "conn_z")); !errors.Is(err, pitr.ErrAlreadyExists) {
			t.Errorf("duplicate stream id = %v; want ErrAlreadyExists", err)
		}
		if err := st.CreateStream(ctx, newStream("pst_b", "conn_a")); !errors.Is(err, pitr.ErrConnectionTaken) {
			t.Errorf("duplicate connection = %v; want ErrConnectionTaken", err)
		}
	})
	t.Run("chain id", func(t *testing.T) {
		start := pitr.OpTime{TS: pos(100, 1), Term: 1}
		if err := st.StartChain(ctx, "pst_a", "ch1", start, at); err != nil {
			t.Fatal(err)
		}
		if err := st.EndChain(ctx, "pst_a", "ch1", pos(100, 1), pitr.EndStopped, at); err != nil {
			t.Fatal(err)
		}
		if err := st.StartChain(ctx, "pst_a", "ch1", start, at); !errors.Is(err, pitr.ErrAlreadyExists) {
			t.Fatalf("reused chain id = %v; want ErrAlreadyExists", err)
		}
	})
	t.Run("chunk id and storage key", func(t *testing.T) {
		if err := st.StartChain(ctx, "pst_a", "ch2", pitr.OpTime{TS: pos(100, 1), Term: 1}, at); err != nil {
			t.Fatal(err)
		}
		first := chunk("chk_1", "ch2", pos(100, 1), pos(160, 1), 1)
		if err := st.CommitChunk(ctx, first); err != nil {
			t.Fatal(err)
		}
		sameID := chunk("chk_1", "ch2", pos(160, 1), pos(200, 1), 1)
		if err := st.CommitChunk(ctx, sameID); !errors.Is(err, pitr.ErrAlreadyExists) {
			t.Errorf("duplicate chunk id = %v; want ErrAlreadyExists", err)
		}
		sameKey := chunk("chk_2", "ch2", pos(160, 1), pos(200, 1), 1)
		sameKey.StorageKey = first.StorageKey
		if err := st.CommitChunk(ctx, sameKey); !errors.Is(err, pitr.ErrAlreadyExists) {
			t.Errorf("duplicate storage key = %v; want ErrAlreadyExists", err)
		}
	})
	t.Run("approval id", func(t *testing.T) {
		a := &models.Approval{ID: "apr_1", Status: models.ApprovalPending, CreatedAt: at, ExpiresAt: at.Add(time.Hour)}
		if err := st.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateApproval(ctx, a); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate approval = %v; want ErrAlreadyExists", err)
		}
	})
	t.Run("job id", func(t *testing.T) {
		if err := st.CreateJob(ctx, &models.Job{ID: "job_1", Name: "one"}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, &models.Job{ID: "job_1", Name: "two"}); !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("duplicate job = %v; want ErrAlreadyExists", err)
		}
	})
}
