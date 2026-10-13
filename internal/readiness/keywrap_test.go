package readiness_test

import (
	"context"
	"testing"

	"github.com/yigitcittan/mongorescue/internal/readiness"
	"github.com/yigitcittan/mongorescue/internal/store/storetest"
)

func TestReportShowsKeyWrapping(t *testing.T) {
	st := storetest.New(t)
	off, err := readiness.New(readiness.Config{Store: unedited{st}}).Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if off.KeyWrapping != nil {
		t.Fatalf("key_wrapping without a source = %+v", off.KeyWrapping)
	}
	svc := readiness.New(readiness.Config{Store: unedited{st},
		KeyWrapping: func() readiness.KeyWrapping { return readiness.KeyWrapping{Active: true, Provider: "gcp-kms"} }})
	r, err := svc.Report(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.KeyWrapping == nil || !r.KeyWrapping.Active || r.KeyWrapping.Provider != "gcp-kms" {
		t.Fatalf("key_wrapping = %+v", r.KeyWrapping)
	}
}
