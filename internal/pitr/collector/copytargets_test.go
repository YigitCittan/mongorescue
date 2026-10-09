package collector

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// TestStreamCopyTargets proves that a stream stores its copy targets, refuses
// its primary target or more than three among them, and removes them with an
// empty list.
func TestStreamCopyTargets(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	off := false
	update := func(ids ...string) error {
		list := ids
		_, err := fx.svc.UpdateStream(ctx, fx.stream.ID, StreamRequest{Enabled: &off, CopyTargets: &list})
		return err
	}
	if err := update("tgt_b", " tgt_c "); err != nil {
		t.Fatal(err)
	}
	st, err := fx.repo.GetStream(ctx, fx.stream.ID)
	if err != nil || !slices.Equal(st.CopyTargets, []string{"tgt_b", "tgt_c"}) {
		t.Fatalf("copy targets = %v, %v", st.CopyTargets, err)
	}
	if err = update("tgt_a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("the primary as a copy target = %v; want ErrInvalid", err)
	}
	if err = update("t1", "t2", "t3", "t4"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("four copy targets = %v; want ErrInvalid", err)
	}
	if err = update(); err != nil {
		t.Fatal(err)
	}
	if st, _ = fx.repo.GetStream(ctx, fx.stream.ID); len(st.CopyTargets) != 0 {
		t.Fatalf("copy targets after removing them = %v", st.CopyTargets)
	}
}
