package store_test

import (
	"context"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/pitr"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestStreamKeepsItsChainTestSchedule(t *testing.T) {
	s := storetest.New(t)
	ctx := context.Background()
	st := &pitr.Stream{ID: "str_a", ConnectionID: "conn_a", BaseCron: "@daily", BaseKeepCount: 7, ChunkSeconds: 60, ChainTestCron: "0 4 * * 0"}
	if err := s.CreateStream(ctx, st); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetStream(ctx, "str_a")
	if err != nil || got.ChainTestCron != "0 4 * * 0" {
		t.Fatalf("stream = %+v, err = %v", got, err)
	}
	got.ChainTestCron = ""
	if err = s.UpdateStream(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetStream(ctx, "str_a"); err != nil || got.ChainTestCron != "" {
		t.Fatalf("after turning chain tests off: %+v, %v", got, err)
	}
}
